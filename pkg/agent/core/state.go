package core

// The core scaling logic at the heart of the autoscaler-agent. This file implements everything with
// mostly pure-ish functions, so that all the making & receiving requests can be done elsewhere.
//
// Broadly our strategy is to mimic the kind of eventual consistency that is itself used in
// Kubernetes. The scaling logic wasn't always implemented like this, but because the
// autoscaler-agent *fundamentally* exists in an eventual consistency world, we have to either:
//  (a) make assumptions that we know are false; or
//  (b) design our system so it assumes less.
// We used to solve this by (a). We ran into¹ issues² going that way, because sometimes those false
// assumptions come back to haunt you.
//
// That said, there's still some tricky semantics we want to maintain. Internally, the
// autoscaler-agent must be designed around eventual consistency, but the API we expose to the
// vm-monitor is strictly synchronous. As such, there's some subtle logic to make sure that we're
// not violating our own guarantees unless required to.
//
// ---
// ¹ https://github.com/neondatabase/autoscaling/issues/23
// ² https://github.com/neondatabase/autoscaling/issues/350

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samber/lo"
	"go.uber.org/zap"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/agent/core/revsource"
	"github.com/neondatabase/autoscaling/pkg/api"
)

type ObservabilityCallbacks struct {
	PluginLatency  revsource.ObserveCallback
	MonitorLatency revsource.ObserveCallback
	NeonVMLatency  revsource.ObserveCallback

	ActualScaling       ReportActualScalingEventCallback
	HypotheticalScaling ReportHypotheticalScalingEventCallback
}

type (
	ReportActualScalingEventCallback       func(timestamp time.Time, current uint32, target uint32)
	ReportHypotheticalScalingEventCallback func(timestamp time.Time, current uint32, target uint32, parts ScalingGoalParts)
)

type RevisionSource interface {
	Next(ts time.Time, flags vmv1.Flag) vmv1.Revision
	Observe(moment time.Time, rev vmv1.Revision) error
}

// Config represents some of the static configuration underlying the decision-making of State
type Config struct {
	// ComputeUnit is the desired ratio between CPU and memory, copied from the global
	// autoscaler-agent config.
	ComputeUnit api.Resources

	// DefaultScalingConfig is just copied from the global autoscaler-agent config.
	// If the VM's ScalingConfig is nil, we use this field instead.
	DefaultScalingConfig api.ScalingConfig

	// NeonVMRetryWait gives the amount of time to wait to retry after a failed request
	NeonVMRetryWait time.Duration

	// PluginRequestTick gives the period at which we should be making requests to the scheduler
	// plugin, even if nothing's changed.
	PluginRequestTick time.Duration

	// PluginRetryWait gives the amount of time to wait to retry after a failed request
	PluginRetryWait time.Duration

	// PluginDeniedRetryWait gives the amount of time we must wait before re-requesting resources
	// that were not fully granted.
	PluginDeniedRetryWait time.Duration

	// MonitorDeniedDownscaleCooldown gives the time we must wait between making duplicate
	// downscale requests to the vm-monitor where the previous failed.
	MonitorDeniedDownscaleCooldown time.Duration

	// MonitorRequestedUpscaleValidPeriod gives the duration for which requested upscaling from the
	// vm-monitor must be respected.
	MonitorRequestedUpscaleValidPeriod time.Duration

	// MonitorRetryWait gives the amount of time to wait to retry after a *failed* request.
	MonitorRetryWait time.Duration

	// Log provides an outlet for (*State).NextActions() to give informative messages or warnings
	// about conditions that are impeding its ability to execute.
	Log LogConfig `json:"-"`

	// RevisionSource is the source of revisions to track the progress during scaling.
	RevisionSource RevisionSource `json:"-"`

	// ObservabilityCallbacks are the callbacks to submit datapoints for observability.
	ObservabilityCallbacks ObservabilityCallbacks `json:"-"`
}

type LogConfig struct {
	// Info, if not nil, will be called to provide information during normal functioning.
	// For example, we log the calculated desired resources on every call to NextActions.
	Info func(string, ...zap.Field)
	// Warn, if not nil, will be called to log conditions that are impeding the ability to move the
	// current resources to what's desired.
	// A typical warning may be something like "wanted to do X but couldn't because of Y".
	Warn func(string, ...zap.Field)
}

// State holds all of the necessary internal state for a VM in order to make scaling
// decisions
type State struct {
	internal state
}

// one level of indirection below State so that the fields can be public, and JSON-serializable
type state struct {
	Config Config

	// unused. Exists to make it easier to add print debugging (via .config.Warn) for a single call
	// to NextActions.
	Debug bool

	// VM gives the current state of the VM - or at least, the state of the fields we care about.
	//
	// NB: any contents behind pointers in VM are immutable. Any time the field is updated, we
	// replace it with a fresh object.
	VM api.VmInfo

	// Plugin records all state relevant to communications with the scheduler plugin
	Plugin pluginState

	// Monitor records all state relevant to communications with the vm-monitor
	Monitor monitorState

	// NeonVM records all state relevant to the NeonVM k8s API
	NeonVM neonvmState

	Metrics *SystemMetrics

	LFCMetrics *LFCMetrics

	// TargetRevision is the revision agent works towards.
	TargetRevision vmv1.Revision

	// LastDesiredResources is the last target agent wanted to scale to.
	LastDesiredResources *api.Resources
}

type pluginState struct {
	// OngoingRequest is true iff there is currently an ongoing request to *this* scheduler plugin.
	OngoingRequest bool
	// LastRequest, if not nil, gives information about the most recently started request to the
	// plugin (maybe unfinished!)
	LastRequest *pluginRequested
	// LastFailureAt, if not nil, gives the time of the most recent request failure
	LastFailureAt *time.Time
	// Permit, if not nil, stores the Permit in the most recent PluginResponse. This field will be
	// nil if we have not been able to contact *any* scheduler.
	Permit *api.Resources

	// CurrentRevision is the most recent revision the plugin has acknowledged.
	CurrentRevision vmv1.Revision
}

type pluginRequested struct {
	At        time.Time
	Resources api.Resources
}

type monitorState struct {
	OngoingRequest *ongoingMonitorRequest

	// RequestedUpscale, if not nil, stores the most recent *unresolved* upscaling requested by the
	// vm-monitor, along with the time at which it occurred.
	RequestedUpscale *requestedUpscale

	// DeniedDownscale, if not nil, stores the result of the latest denied /downscale request.
	DeniedDownscale *deniedDownscale

	// Approved stores the most recent Resources associated with either (a) an accepted downscale
	// request, or (b) a successful upscale notification.
	Approved *api.Resources

	// DownscaleFailureAt, if not nil, stores the time at which a downscale request most recently
	// failed (where "failed" means that some unexpected error occurred, not that it was merely
	// denied).
	DownscaleFailureAt *time.Time
	// UpscaleFailureAt, if not nil, stores the time at which an upscale request most recently
	// failed
	UpscaleFailureAt *time.Time

	// CurrentRevision is the most recent revision the monitor has acknowledged.
	CurrentRevision vmv1.Revision
}

func (ms *monitorState) active() bool {
	return ms.Approved != nil
}

type ongoingMonitorRequest struct {
	Kind      monitorRequestKind
	Requested api.Resources
}

type monitorRequestKind string

const (
	monitorRequestKindDownscale monitorRequestKind = "downscale"
	monitorRequestKindUpscale   monitorRequestKind = "upscale"
)

type requestedUpscale struct {
	At        time.Time
	Base      api.Resources
	Requested api.MoreResources
}

type deniedDownscale struct {
	At        time.Time
	Current   api.Resources
	Requested api.Resources
}

type neonvmState struct {
	LastSuccess *api.Resources
	// OngoingRequested, if not nil, gives the resources requested
	OngoingRequested *api.Resources
	RequestFailedAt  *time.Time

	// TargetRevision is the revision agent works towards. Contrary to monitor/plugin, we
	// store it not only in action, but also here. This is needed, because for NeonVM propagation
	// happens after the changes are actually applied, when the action object is long gone.
	TargetRevision  vmv1.RevisionWithTime
	CurrentRevision vmv1.Revision
}

func (ns *neonvmState) ongoingRequest() bool {
	return ns.OngoingRequested != nil
}

func NewState(vm api.VmInfo, config Config) *State {
	return &State{
		internal: state{
			Config: config,
			Debug:  false,
			VM:     vm,
			Plugin: pluginState{
				OngoingRequest:  false,
				LastRequest:     nil,
				LastFailureAt:   nil,
				Permit:          nil,
				CurrentRevision: vmv1.ZeroRevision,
			},
			Monitor: monitorState{
				OngoingRequest:     nil,
				RequestedUpscale:   nil,
				DeniedDownscale:    nil,
				Approved:           nil,
				DownscaleFailureAt: nil,
				UpscaleFailureAt:   nil,
				CurrentRevision:    vmv1.ZeroRevision,
			},
			NeonVM: neonvmState{
				LastSuccess:      nil,
				OngoingRequested: nil,
				RequestFailedAt:  nil,
				TargetRevision:   vmv1.ZeroRevision.WithTime(time.Time{}),
				CurrentRevision:  vmv1.ZeroRevision,
			},
			Metrics:              nil,
			LFCMetrics:           nil,
			LastDesiredResources: nil,
			TargetRevision:       vmv1.ZeroRevision,
		},
	}
}

func (s *state) info(msg string, fields ...zap.Field) {
	if s.Config.Log.Info != nil {
		s.Config.Log.Info(msg, fields...)
	}
}

func (s *state) warn(msg string /* , fields ...zap.Field */) {
	if s.Config.Log.Warn != nil {
		s.Config.Log.Warn(msg /* , fields... */)
	}
}

func (s *state) warnf(msg string, args ...any) {
	s.warn(fmt.Sprintf(msg, args...))
}

// NextActions is used to implement the state machine. It's a pure function that *just* indicates
// what the executor should do.
func (s *State) NextActions(now time.Time) ActionSet {
	return s.internal.nextActions(now)
}

func (s *state) nextActions(now time.Time) ActionSet {
	var actions ActionSet

	desiredResources, calcDesiredResourcesWait := s.desiredResourcesFromMetricsOrRequestedUpscaling(now)
	if calcDesiredResourcesWait == nil {
		// our handling later on is easier if we can assume it's non-nil
		calcDesiredResourcesWait = func(ActionSet) *time.Duration { return nil }
	}

	// ----
	// Requests to the scheduler plugin:
	var pluginRequiredWait *time.Duration
	actions.PluginRequest, pluginRequiredWait = s.calculatePluginAction(now, desiredResources)

	// ----
	// Requests to NeonVM:
	var pluginRequested *api.Resources
	pluginRequestedPhase := "<this string should not appear>"
	if s.Plugin.OngoingRequest {
		pluginRequested = &s.Plugin.LastRequest.Resources
		pluginRequestedPhase = "ongoing"
	} else if actions.PluginRequest != nil {
		pluginRequested = &actions.PluginRequest.Target
		pluginRequestedPhase = "planned"
	}
	var neonvmRequiredWait *time.Duration
	actions.NeonVMRequest, neonvmRequiredWait = s.calculateNeonVMAction(now, desiredResources, pluginRequested, pluginRequestedPhase)

	// ----
	// Requests to vm-monitor (upscaling)
	//
	// NB: upscaling takes priority over downscaling requests, because otherwise we'd potentially
	// forego notifying the vm-monitor of increased resources because we were busy asking if it
	// could downscale.
	var monitorUpscaleRequiredWait *time.Duration
	actions.MonitorUpscale, monitorUpscaleRequiredWait = s.calculateMonitorUpscaleAction(now, desiredResources)

	// ----
	// Requests to vm-monitor (downscaling)
	plannedUpscale := actions.MonitorUpscale != nil
	var monitorDownscaleRequiredWait *time.Duration
	actions.MonitorDownscale, monitorDownscaleRequiredWait = s.calculateMonitorDownscaleAction(now, desiredResources, plannedUpscale)

	// --- and that's all the request types! ---

	// If there's anything waiting, we should also note how long we should wait for.
	// There's two components we could be waiting on: the scheduler plugin, and the vm-monitor.
	maximumDuration := time.Duration(int64(uint64(1)<<63 - 1))
	requiredWait := maximumDuration

	requiredWaits := []*time.Duration{
		calcDesiredResourcesWait(actions),
		pluginRequiredWait,
		neonvmRequiredWait,
		monitorUpscaleRequiredWait,
		monitorDownscaleRequiredWait,
	}
	for _, w := range requiredWaits {
		if w != nil {
			requiredWait = min(requiredWait, *w)
		}
	}

	// If we're waiting on anything, add it as an action
	if requiredWait != maximumDuration {
		actions.Wait = &ActionWait{Duration: requiredWait}
	}

	return actions
}

func (s *state) calculatePluginAction(
	now time.Time,
	desiredResources api.Resources,
) (*ActionPluginRequest, *time.Duration) {
	logFailureReason := func(reason string) {
		s.warnf("Wanted to make a request to the scheduler plugin, but %s", reason)
	}

	// additional resources we want to request OR previous downscaling we need to inform the plugin of
	// NOTE: only valid if s.plugin.permit != nil AND there's no ongoing NeonVM request.
	requestResources := s.clampResources(
		s.VM.Using(),
		desiredResources,
		ptr(s.VM.Using()), // don't decrease below VM using (decrease happens *before* telling the plugin)
		nil,               // but any increase is ok
	)
	// resources if we're just informing the plugin of current resource usage.
	currentResources := s.VM.Using()
	if s.NeonVM.OngoingRequested != nil {
		// include any ongoing NeonVM request, because we're already using that.
		currentResources = currentResources.Max(*s.NeonVM.OngoingRequested)
	}

	// We want to make a request to the scheduler plugin if:
	//  1. it's been long enough since the previous request (so we're obligated by PluginRequestTick); or
	//  2.a. we want to request resources / inform it of downscale;
	//    b. there isn't any ongoing, conflicting request; and
	//    c. we haven't recently been denied these resources
	var timeUntilNextRequestTick time.Duration
	if s.Plugin.LastRequest != nil {
		timeUntilNextRequestTick = s.Config.PluginRequestTick - now.Sub(s.Plugin.LastRequest.At)
	}

	timeForRequest := timeUntilNextRequestTick <= 0

	var timeUntilRetryBackoffExpires time.Duration
	requestPreviouslyDenied := !s.Plugin.OngoingRequest &&
		s.Plugin.LastRequest != nil &&
		s.Plugin.Permit != nil &&
		s.Plugin.LastRequest.Resources.HasFieldGreaterThan(*s.Plugin.Permit)
	if requestPreviouslyDenied {
		timeUntilRetryBackoffExpires = s.Plugin.LastRequest.At.Add(s.Config.PluginDeniedRetryWait).Sub(now)
	}

	waitingOnRetryBackoff := timeUntilRetryBackoffExpires > 0

	// changing the resources we're requesting from the plugin
	wantToRequestNewResources := s.Plugin.LastRequest != nil && s.Plugin.Permit != nil &&
		requestResources != *s.Plugin.Permit
	// ... and this isn't a duplicate (or, at least it's been long enough)
	shouldRequestNewResources := wantToRequestNewResources && !waitingOnRetryBackoff

	permittedRequestResources := requestResources
	if !shouldRequestNewResources {
		permittedRequestResources = currentResources
	}

	// Can't make a duplicate request
	if s.Plugin.OngoingRequest {
		// ... but if the desired request is different from what we would be making,
		// then it's worth logging
		if s.Plugin.LastRequest.Resources != permittedRequestResources {
			logFailureReason("there's already an ongoing request for different resources")
		}
		return nil, nil
	}

	// Can't make a request if we failed too recently
	if s.Plugin.LastFailureAt != nil {
		timeUntilFailureBackoffExpires := s.Plugin.LastFailureAt.Add(s.Config.PluginRetryWait).Sub(now)
		if timeUntilFailureBackoffExpires > 0 {
			logFailureReason("previous request failed too recently")
			return nil, &timeUntilFailureBackoffExpires
		}
	}

	// At this point, all that's left is either making the request, or saying to wait.
	// The rest of the complication is just around accurate logging.
	if timeForRequest || shouldRequestNewResources {
		return &ActionPluginRequest{
			LastPermit: s.Plugin.Permit,
			Target:     permittedRequestResources,
			// convert maybe-nil '*Metrics' to maybe-nil '*core.Metrics'
			Metrics: func() *api.Metrics {
				if s.Metrics != nil {
					return lo.ToPtr(s.Metrics.ToAPI())
				} else {
					return nil
				}
			}(),
			TargetRevision: s.TargetRevision.WithTime(now),
		}, nil
	} else {
		if wantToRequestNewResources && waitingOnRetryBackoff {
			logFailureReason("previous request for more resources was denied too recently")
		}
		waitTime := timeUntilNextRequestTick
		if waitingOnRetryBackoff {
			waitTime = min(waitTime, timeUntilRetryBackoffExpires)
		}
		return nil, &waitTime
	}
}

func ptr[T any](t T) *T { return &t }

func (s *state) calculateNeonVMAction(
	now time.Time,
	desiredResources api.Resources,
	pluginRequested *api.Resources,
	pluginRequestedPhase string,
) (*ActionNeonVMRequest, *time.Duration) {
	targetRevision := s.TargetRevision
	if desiredResources.HasFieldLessThan(s.VM.Using()) && s.Monitor.CurrentRevision.Value > 0 {
		// We are downscaling, so we needed a permit from the monitor
		targetRevision = targetRevision.Min(s.Monitor.CurrentRevision)
	}

	if desiredResources.HasFieldGreaterThan(s.VM.Using()) && s.Plugin.CurrentRevision.Value > 0 {
		// We are upscaling, so we needed a permit from the plugin
		targetRevision = targetRevision.Min(s.Plugin.CurrentRevision)
	}

	// clamp desiredResources to what we're allowed to make a request for
	desiredResources = s.clampResources(
		s.VM.Using(),                       // current: what we're using already
		desiredResources,                   // target: desired resources
		ptr(s.monitorApprovedLowerBound()), // lower bound: downscaling that the monitor has approved
		ptr(s.pluginApprovedUpperBound()),  // upper bound: upscaling that the plugin has approved
	)

	// If we're already using the desired resources, then no need to make a request
	if s.VM.Using() == desiredResources {
		return nil, nil
	}

	conflictingPluginRequest := pluginRequested != nil && pluginRequested.HasFieldLessThan(desiredResources)

	if !s.NeonVM.ongoingRequest() && !conflictingPluginRequest {
		// We *should* be all clear to make a request; not allowed to make one if we failed too
		// recently
		if s.NeonVM.RequestFailedAt != nil {
			timeUntilFailureBackoffExpires := s.NeonVM.RequestFailedAt.Add(s.Config.NeonVMRetryWait).Sub(now)
			if timeUntilFailureBackoffExpires > 0 {
				s.warn("Wanted to make a request to NeonVM API, but recent request failed too recently")
				return nil, &timeUntilFailureBackoffExpires
			}
		}

		s.NeonVM.TargetRevision = targetRevision.WithTime(now)
		return &ActionNeonVMRequest{
			Current:        s.VM.Using(),
			Target:         desiredResources,
			TargetRevision: s.NeonVM.TargetRevision,
		}, nil
	} else {
		var reqs []string
		if s.Plugin.OngoingRequest {
			reqs = append(reqs, fmt.Sprintf("plugin request %s", pluginRequestedPhase))
		}
		if s.NeonVM.ongoingRequest() && *s.NeonVM.OngoingRequested != desiredResources {
			reqs = append(reqs, "NeonVM request (for different resources) ongoing")
		}

		if len(reqs) != 0 {
			s.warnf("Wanted to make a request to NeonVM API, but there's already %s", strings.Join(reqs, " and "))
		}

		return nil, nil
	}
}

func (s *state) calculateMonitorUpscaleAction(
	now time.Time,
	desiredResources api.Resources,
) (*ActionMonitorUpscale, *time.Duration) {
	// can't do anything if we don't have an active connection to the vm-monitor
	if !s.Monitor.active() {
		return nil, nil
	}

	requestResources := s.clampResources(
		*s.Monitor.Approved,      // current: last resources we got the OK from the monitor on
		s.VM.Using(),             // target: what the VM is currently using
		ptr(*s.Monitor.Approved), // don't decrease below what the monitor is currently set to (this is an *upscale* request)
		ptr(desiredResources.Max(*s.Monitor.Approved)), // don't increase above desired resources
	)

	// Clamp the request resources so we're not increasing by more than 1 CU:
	requestResources = s.clampResources(
		*s.Monitor.Approved,
		requestResources,
		nil, // no lower bound
		ptr(requestResources.Add(s.Config.ComputeUnit)), // upper bound: must not increase by >1 CU
	)

	// Check validity of the request that we would send, before sending it
	if requestResources.HasFieldLessThan(*s.Monitor.Approved) {
		panic(fmt.Errorf(
			"resources for vm-monitor upscaling are less than what was last approved: %+v has field less than %+v",
			requestResources,
			*s.Monitor.Approved,
		))
	}

	wantToDoRequest := requestResources != *s.Monitor.Approved
	if !wantToDoRequest {
		return nil, nil
	}

	// Can't make another request if there's already one ongoing
	if s.Monitor.OngoingRequest != nil {
		var requestDescription string
		if s.Monitor.OngoingRequest.Kind == monitorRequestKindUpscale && s.Monitor.OngoingRequest.Requested != requestResources {
			requestDescription = "upscale request (for different resources)"
		} else if s.Monitor.OngoingRequest.Kind == monitorRequestKindDownscale {
			requestDescription = "downscale request"
		}

		if requestDescription != "" {
			s.warnf("Wanted to send vm-monitor upscale request, but waiting on ongoing %s", requestDescription)
		}
		return nil, nil
	}

	// Can't make another request if we failed too recently:
	if s.Monitor.UpscaleFailureAt != nil {
		timeUntilFailureBackoffExpires := s.Monitor.UpscaleFailureAt.Add(s.Config.MonitorRetryWait).Sub(now)
		if timeUntilFailureBackoffExpires > 0 {
			s.warn("Wanted to send vm-monitor upscale request, but failed too recently")
			return nil, &timeUntilFailureBackoffExpires
		}
	}

	// Otherwise, we can make the request:
	return &ActionMonitorUpscale{
		Current:        *s.Monitor.Approved,
		Target:         requestResources,
		TargetRevision: s.TargetRevision.WithTime(now),
	}, nil
}

func (s *state) calculateMonitorDownscaleAction(
	now time.Time,
	desiredResources api.Resources,
	plannedUpscaleRequest bool,
) (*ActionMonitorDownscale, *time.Duration) {
	// can't do anything if we don't have an active connection to the vm-monitor
	if !s.Monitor.active() {
		if desiredResources.HasFieldLessThan(s.VM.Using()) {
			s.warn("Wanted to send vm-monitor downscale request, but there's no active connection")
		}
		return nil, nil
	}

	requestResources := s.clampResources(
		*s.Monitor.Approved,      // current: what the monitor is already aware of
		desiredResources,         // target: what we'd like the VM to be using
		nil,                      // lower bound: any decrease is fine
		ptr(*s.Monitor.Approved), // upper bound: don't increase (this is only downscaling!)
	)

	// Clamp the request resources so we're not decreasing by more than 1 CU:
	requestResources = s.clampResources(
		*s.Monitor.Approved,
		requestResources,
		ptr(s.Monitor.Approved.SaturatingSub(s.Config.ComputeUnit)), // Must not decrease by >1 CU
		nil, // no upper bound
	)

	// Check validity of the request that we would send, before sending it
	if requestResources.HasFieldGreaterThan(*s.Monitor.Approved) {
		panic(fmt.Errorf(
			"resources for vm-monitor downscaling are greater than what was last approved: %+v has field greater than %+v",
			requestResources,
			*s.Monitor.Approved,
		))
	}

	wantToDoRequest := requestResources != *s.Monitor.Approved
	if !wantToDoRequest {
		return nil, nil
	}

	// Can't make another request if there's already one ongoing (or if an upscaling request is
	// planned)
	if plannedUpscaleRequest {
		s.warn("Wanted to send vm-monitor downscale request, but waiting on other planned upscale request")
		return nil, nil
	} else if s.Monitor.OngoingRequest != nil {
		var requestDescription string
		if s.Monitor.OngoingRequest.Kind == monitorRequestKindDownscale && s.Monitor.OngoingRequest.Requested != requestResources {
			requestDescription = "downscale request (for different resources)"
		} else if s.Monitor.OngoingRequest.Kind == monitorRequestKindUpscale {
			requestDescription = "upscale request"
		}

		if requestDescription != "" {
			s.warnf("Wanted to send vm-monitor downscale request, but waiting on other ongoing %s", requestDescription)
		}
		return nil, nil
	}

	// Can't make another request if we failed too recently:
	if s.Monitor.DownscaleFailureAt != nil {
		timeUntilFailureBackoffExpires := s.Monitor.DownscaleFailureAt.Add(s.Config.MonitorRetryWait).Sub(now)
		if timeUntilFailureBackoffExpires > 0 {
			s.warn("Wanted to send vm-monitor downscale request but failed too recently")
			return nil, &timeUntilFailureBackoffExpires
		}
	}

	// Can't make another request if a recent request for resources less than or equal to the
	// proposed request was denied. In general though, this should be handled by
	// DesiredResourcesFromMetricsOrRequestedUpscaling, so it's we're better off panicking here.
	if s.timeUntilDeniedDownscaleExpired(now) > 0 && !s.Monitor.DeniedDownscale.Requested.HasFieldLessThan(requestResources) {
		panic(errors.New(
			"Wanted to send vm-monitor downscale request, but too soon after previously denied downscaling that should have been handled earlier",
		))
	}

	// Nothing else to check, we're good to make the request
	return &ActionMonitorDownscale{
		Current:        *s.Monitor.Approved,
		Target:         requestResources,
		TargetRevision: s.TargetRevision.WithTime(now),
	}, nil
}

func (s *state) scalingConfig() api.ScalingConfig {
	// nb: WithOverrides allows its arg to be nil, in which case it does nothing.
	return s.Config.DefaultScalingConfig.WithOverrides(s.VM.Config.ScalingConfig)
}

// public version, for testing.
func (s *State) DesiredResourcesFromMetricsOrRequestedUpscaling(now time.Time) (api.Resources, func(ActionSet) *time.Duration) {
	return s.internal.desiredResourcesFromMetricsOrRequestedUpscaling(now)
}

func (s *state) desiredResourcesFromMetricsOrRequestedUpscaling(now time.Time) (api.Resources, func(ActionSet) *time.Duration) {
	// There's some annoying edge cases that this function has to be able to handle properly. For
	// the sake of completeness, they are:
	//
	// 1. s.vm.Using() is not a multiple of s.computeUnit
	// 2. s.vm.Max() is less than s.computeUnit (or: has at least one resource that is)
	// 3. s.vm.Using() is a fractional multiple of s.computeUnit, but !allowDecrease and rounding up
	//    is greater than s.vm.Max()
	// 4. s.vm.Using() is much larger than s.vm.Min() and not a multiple of s.computeUnit, but load
	//    is low so we should just decrease *anyways*.
	//
	// ---
	//
	// Broadly, the implementation works like this:
	// For CPU:
	// Based on load average, calculate the "goal" number of CPUs (and therefore compute units)
	//
	// For Memory:
	// Based on memory usage, calculate the VM's desired memory allocation and extrapolate a
	// goal number of CUs from that.
	//
	// 1. Take the maximum of these two goal CUs to create a unified goal CU
	// 2. Cap the goal CU by min/max, etc
	// 3. that's it!

	reportGoals := func(goalCU uint32, parts ScalingGoalParts) {
		currentCU, ok := s.VM.Using().DivResources(s.Config.ComputeUnit)
		if !ok {
			return // skip reporting if the current CU is not right.
		}

		if report := s.Config.ObservabilityCallbacks.HypotheticalScaling; report != nil {
			report(now, uint32(currentCU), goalCU, parts)
		}
	}

	sg, goalCULogFields := calculateGoalCU(
		s.warn,
		s.scalingConfig(),
		s.Config.ComputeUnit,
		s.Metrics,
		s.LFCMetrics,
	)
	goalCU := sg.GoalCU()
	// If we don't have all the metrics we need, we'll later prevent downscaling to avoid flushing
	// the VM's cache on autoscaler-agent restart if we have SystemMetrics but not LFCMetrics.
	hasAllMetrics := sg.HasAllMetrics

	if hasAllMetrics {
		reportGoals(goalCU, sg.Parts)
	}

	// Copy the initial value of the goal CU so that we can accurately track whether either
	// requested upscaling or denied downscaling affected the outcome.
	// Otherwise as written, it'd be possible to update goalCU from requested upscaling and
	// incorrectly miss that denied downscaling could have had the same effect.
	initialGoalCU := goalCU

	var requestedUpscalingAffectedResult bool

	// Update goalCU based on any explicitly requested upscaling
	timeUntilRequestedUpscalingExpired := s.timeUntilRequestedUpscalingExpired(now)
	requestedUpscalingInEffect := timeUntilRequestedUpscalingExpired > 0
	if requestedUpscalingInEffect {
		reqCU := s.requiredCUForRequestedUpscaling(s.Config.ComputeUnit, *s.Monitor.RequestedUpscale)
		if reqCU > initialGoalCU {
			// FIXME: this isn't quite correct, because if initialGoalCU is already equal to the
			// maximum goal CU we *could* have, this won't actually have an effect.
			requestedUpscalingAffectedResult = true
			goalCU = max(goalCU, reqCU)
		}
	}

	var deniedDownscaleAffectedResult bool

	// Update goalCU based on any previously denied downscaling
	timeUntilDeniedDownscaleExpired := s.timeUntilDeniedDownscaleExpired(now)
	deniedDownscaleInEffect := timeUntilDeniedDownscaleExpired > 0
	if deniedDownscaleInEffect {
		reqCU := s.requiredCUForDeniedDownscale(s.Config.ComputeUnit, s.Monitor.DeniedDownscale.Requested)
		if reqCU > initialGoalCU {
			deniedDownscaleAffectedResult = true
			goalCU = max(goalCU, reqCU)
		}
	}

	// resources for the desired "goal" compute units
	goalResources := s.Config.ComputeUnit.Mul(uint16(goalCU))

	// If we don't have all the metrics we need to make a proper decision, make sure that we aren't
	// going to scale down below the current resources.
	// Otherwise, we can make an under-informed decision that has undesirable impacts (e.g., scaling
	// down because we don't have LFC metrics and flushing the cache because of it).
	if !hasAllMetrics {
		goalResources = goalResources.Max(s.VM.Using())
	}

	// bound goalResources by the minimum and maximum resource amounts for the VM
	result := goalResources.Min(s.VM.Max()).Max(s.VM.Min())

	// ... but if we aren't allowed to downscale, then we *must* make sure that the VM's usage value
	// won't decrease to the previously denied amount, even if it's greater than the maximum.
	//
	// We can run into siutations like this when VM scale-down on bounds change fails, so we end up
	// with a usage value greater than the maximum.
	//
	// It's not a great situation to be in, but it's easier to make the policy "give the users a
	// little extra if we mess up" than "oops we OOM-killed your DB, hope you weren't doing anything".
	if deniedDownscaleInEffect {
		// roughly equivalent to "result >= s.monitor.deniedDownscale.requested"
		if !result.HasFieldGreaterThan(s.Monitor.DeniedDownscale.Requested) {
			// This can only happen if s.vm.Max() is less than goalResources, because otherwise this
			// would have been factored into goalCU, affecting goalResources. Hence, the warning.
			s.warn("Can't decrease desired resources to within VM maximum because of vm-monitor previously denied downscale request")
		}
		preMaxResult := result
		result = result.Max(s.minRequiredResourcesForDeniedDownscale(s.Config.ComputeUnit, *s.Monitor.DeniedDownscale))
		if result != preMaxResult {
			deniedDownscaleAffectedResult = true
		}
	}

	// Check that the result is sound.
	//
	// With the current (naive) implementation, this is trivially ok. In future versions, it might
	// not be so simple, so it's good to have this integrity check here.
	if !deniedDownscaleAffectedResult && result.HasFieldGreaterThan(s.VM.Max()) {
		panic(fmt.Errorf(
			"produced invalid desired state: result has field greater than max. this = %+v", *s,
		))
	} else if result.HasFieldLessThan(s.VM.Min()) {
		panic(fmt.Errorf(
			"produced invalid desired state: result has field less than min. this = %+v", *s,
		))
	}

	calculateWaitTime := func(actions ActionSet) *time.Duration {
		var waiting bool
		waitTime := time.Duration(int64(1<<63 - 1)) // time.Duration is an int64. As an "unset" value, use the maximum.

		if deniedDownscaleAffectedResult && actions.MonitorDownscale == nil && s.Monitor.OngoingRequest == nil {
			waitTime = min(waitTime, timeUntilDeniedDownscaleExpired)
			waiting = true
		}
		if requestedUpscalingAffectedResult {
			waitTime = min(waitTime, timeUntilRequestedUpscalingExpired)
			waiting = true
		}

		if waiting {
			return &waitTime
		} else {
			return nil
		}
	}
	s.updateTargetRevision(now, result, s.VM.Using())

	// TODO: we are both saving the result into LastDesiredResources and returning it. This is
	// redundant, and we should remove one of the two.
	s.LastDesiredResources = &result

	logFields := []zap.Field{
		zap.Object("current", s.VM.Using()),
		zap.Object("target", result),
		zap.Object("targetRevision", &s.TargetRevision),
	}
	logFields = append(logFields, goalCULogFields...)
	s.info("Calculated desired resources", logFields...)

	return result, calculateWaitTime
}

func (s *state) updateTargetRevision(now time.Time, desired api.Resources, current api.Resources) {
	if s.LastDesiredResources == nil {
		s.LastDesiredResources = &current
	}

	if *s.LastDesiredResources == desired {
		// Nothing changed, so no need to update the target revision
		return
	}

	var flags vmv1.Flag

	if desired.HasFieldGreaterThan(*s.LastDesiredResources) {
		flags.Set(revsource.Upscale)
	}
	if desired.HasFieldLessThan(*s.LastDesiredResources) {
		flags.Set(revsource.Downscale)
	}

	s.TargetRevision = s.Config.RevisionSource.Next(now, flags)
}

func (s *state) updateNeonVMCurrentRevision(currentRevision vmv1.RevisionWithTime) {
	revsource.Propagate(currentRevision.UpdatedAt.Time,
		s.NeonVM.TargetRevision,
		&s.NeonVM.CurrentRevision,
		s.Config.ObservabilityCallbacks.NeonVMLatency,
	)
	err := s.Config.RevisionSource.Observe(currentRevision.UpdatedAt.Time, currentRevision.Revision)
	if err != nil {
		s.warnf("Failed to observe clock source: %v", err)
	}

	// We also zero out LastDesiredResources, because we are now starting from
	// a new current resources.
	s.LastDesiredResources = nil
}

func (s *state) timeUntilRequestedUpscalingExpired(now time.Time) time.Duration {
	if s.Monitor.RequestedUpscale != nil {
		return s.Monitor.RequestedUpscale.At.Add(s.Config.MonitorRequestedUpscaleValidPeriod).Sub(now)
	} else {
		return 0
	}
}

// NB: we could just use s.plugin.computeUnit or s.monitor.requestedUpscale from inside the
// function, but those are sometimes nil. This way, it's clear that it's the caller's responsibility
// to ensure that the values are non-nil.
func (s *state) requiredCUForRequestedUpscaling(computeUnit api.Resources, requestedUpscale requestedUpscale) uint32 {
	var required uint32
	requested := requestedUpscale.Requested
	base := requestedUpscale.Base

	// note: 1 + floor(x / M) gives the minimum integer value greater than x / M.

	if requested.Cpu {
		required = max(required, 1+uint32(base.VCPU/computeUnit.VCPU))
	}
	if requested.Memory {
		required = max(required, 1+uint32(base.Mem/computeUnit.Mem))
	}

	return required
}

func (s *state) timeUntilDeniedDownscaleExpired(now time.Time) time.Duration {
	if s.Monitor.DeniedDownscale != nil {
		return s.Monitor.DeniedDownscale.At.Add(s.Config.MonitorDeniedDownscaleCooldown).Sub(now)
	} else {
		return 0
	}
}

// NB: like requiredCUForRequestedUpscaling, we make the caller provide the values so that it's
// more clear that it's the caller's responsibility to ensure the values are non-nil.
func (s *state) requiredCUForDeniedDownscale(computeUnit, deniedResources api.Resources) uint32 {
	// note: floor(x / M) + 1 gives the minimum integer value greater than x / M.
	requiredFromCPU := 1 + uint32(deniedResources.VCPU/computeUnit.VCPU)
	requiredFromMem := 1 + uint32(deniedResources.Mem/computeUnit.Mem)

	return max(requiredFromCPU, requiredFromMem)
}

func (s *state) minRequiredResourcesForDeniedDownscale(computeUnit api.Resources, denied deniedDownscale) api.Resources {
	// for each resource, increase the value by one CU's worth, but not greater than the value we
	// were at while attempting to downscale.
	//
	// phrasing it like this cleanly handles some subtle edge cases when denied.current isn't a
	// multiple of the compute unit.
	return api.Resources{
		VCPU: min(denied.Current.VCPU, computeUnit.VCPU*(1+denied.Requested.VCPU/computeUnit.VCPU)),
		Mem:  min(denied.Current.Mem, computeUnit.Mem*(1+denied.Requested.Mem/computeUnit.Mem)),
	}
}

// clampResources uses the directionality of the difference between s.vm.Using() and desired to
// clamp the desired resources with the upper *or* lower bound
func (s *state) clampResources(
	current api.Resources,
	desired api.Resources,
	lowerBound *api.Resources,
	upperBound *api.Resources,
) api.Resources {
	// Internal validity checks:
	if lowerBound != nil && lowerBound.HasFieldGreaterThan(current) {
		panic(fmt.Errorf(
			"clampResources called with invalid arguments: lowerBound=%+v has field greater than current=%+v",
			lowerBound,
			current,
		))
	} else if upperBound != nil && upperBound.HasFieldLessThan(current) {
		panic(fmt.Errorf(
			"clampResources called with invalid arguments: upperBound=%+v has field less than current=%+v",
			upperBound,
			current,
		))
	}

	cpu := desired.VCPU
	if desired.VCPU < current.VCPU && lowerBound != nil {
		cpu = max(desired.VCPU, lowerBound.VCPU)
	} else if desired.VCPU > current.VCPU && upperBound != nil {
		cpu = min(desired.VCPU, upperBound.VCPU)
	}

	mem := desired.Mem
	if desired.Mem < current.Mem && lowerBound != nil {
		mem = max(desired.Mem, lowerBound.Mem)
	} else if desired.Mem > current.Mem && upperBound != nil {
		mem = min(desired.Mem, upperBound.Mem)
	}

	return api.Resources{VCPU: cpu, Mem: mem}
}

func (s *state) monitorApprovedLowerBound() api.Resources {
	if s.Monitor.Approved != nil {
		return *s.Monitor.Approved
	} else {
		return s.VM.Using()
	}
}

func (s *state) pluginApprovedUpperBound() api.Resources {
	if s.Plugin.Permit != nil {
		return *s.Plugin.Permit
	} else {
		return s.VM.Using()
	}
}

//////////////////////////////////////////
// PUBLIC FUNCTIONS TO UPDATE THE STATE //
//////////////////////////////////////////

// Debug sets s.debug = enabled. This method is exclusively meant to be used in tests, to make it
// easier to enable print debugging only for a single call to NextActions, via s.warn() or otherwise.
func (s *State) Debug(enabled bool) {
	s.internal.Debug = enabled
}

func (s *State) UpdatedVM(vm api.VmInfo) {
	// FIXME: overriding this is required right now because we trust that a successful request to
	// NeonVM means the VM was already updated, which... isn't true, and otherwise we could run into
	// sync issues.
	// A first-pass solution is possible by reading the values of VirtualMachine.Spec, but the
	// "proper" solution would read from VirtualMachine.Status, which (at time of writing) isn't
	// sound. For more, see:
	// - https://github.com/neondatabase/autoscaling/pull/371#issuecomment-1752110131
	// - https://github.com/neondatabase/autoscaling/issues/462
	vm.SetUsing(s.internal.VM.Using())
	s.internal.VM = vm
	if vm.CurrentRevision != nil {
		s.internal.updateNeonVMCurrentRevision(*vm.CurrentRevision)
	}

	// Make sure that if LFC metrics are disabled & later enabled, we don't make decisions based on
	// stale data.
	if !*s.internal.scalingConfig().EnableLFCMetrics {
		s.internal.LFCMetrics = nil
	}
}

func (s *State) UpdateSystemMetrics(metrics SystemMetrics) {
	s.internal.Metrics = &metrics
}

func (s *State) UpdateLFCMetrics(metrics LFCMetrics) {
	s.internal.LFCMetrics = &metrics
}

// PluginHandle provides write access to the scheduler plugin pieces of an UpdateState
type PluginHandle struct {
	s *state
}

func (s *State) Plugin() PluginHandle {
	return PluginHandle{&s.internal}
}

func (h PluginHandle) StartingRequest(now time.Time, resources api.Resources) {
	h.s.Plugin.LastRequest = &pluginRequested{
		At:        now,
		Resources: resources,
	}
	h.s.Plugin.OngoingRequest = true
}

func (h PluginHandle) RequestFailed(now time.Time) {
	h.s.Plugin.OngoingRequest = false
	h.s.Plugin.LastFailureAt = &now
}

func (h PluginHandle) RequestSuccessful(
	now time.Time,
	targetRevision vmv1.RevisionWithTime,
	resp api.PluginResponse,
) (_err error) {
	h.s.Plugin.OngoingRequest = false

	defer func() {
		if _err != nil {
			h.s.Plugin.LastFailureAt = &now
		}
	}()

	if err := resp.Permit.ValidateNonZero(); err != nil {
		return fmt.Errorf("Invalid permit: %w", err)
	}

	// Errors from resp in connection with the prior request
	if resp.Permit.HasFieldGreaterThan(h.s.Plugin.LastRequest.Resources) {
		return fmt.Errorf(
			"Permit has resources greater than request (%+v vs. %+v)",
			resp.Permit, h.s.Plugin.LastRequest.Resources,
		)
	}

	// Errors from resp in connection with the prior request AND the VM state
	if vmUsing := h.s.VM.Using(); resp.Permit.HasFieldLessThan(vmUsing) {
		return fmt.Errorf("Permit has resources less than VM (%+v vs %+v)", resp.Permit, vmUsing)
	}

	// All good - set everything.

	// NOTE: We don't set the compute unit, even though the plugin response contains it. We're in
	// the process of moving the source of truth for ComputeUnit from the scheduler plugin to the
	// autoscaler-agent.
	h.s.Plugin.Permit = &resp.Permit
	revsource.Propagate(now,
		targetRevision,
		&h.s.Plugin.CurrentRevision,
		h.s.Config.ObservabilityCallbacks.PluginLatency,
	)
	return nil
}

// MonitorHandle provides write access to the vm-monitor pieces of an UpdateState
type MonitorHandle struct {
	s *state
}

func (s *State) Monitor() MonitorHandle {
	return MonitorHandle{&s.internal}
}

func (h MonitorHandle) Reset() {
	h.s.Monitor = monitorState{
		OngoingRequest:     nil,
		RequestedUpscale:   nil,
		DeniedDownscale:    nil,
		Approved:           nil,
		DownscaleFailureAt: nil,
		UpscaleFailureAt:   nil,
		CurrentRevision:    vmv1.ZeroRevision,
	}
}

func (h MonitorHandle) Active(active bool) {
	if active {
		approved := h.s.VM.Using()
		h.s.Monitor.Approved = &approved // TODO: this is racy
	} else {
		h.s.Monitor.Approved = nil
	}
}

func (h MonitorHandle) UpscaleRequested(now time.Time, resources api.MoreResources) {
	h.s.Monitor.RequestedUpscale = &requestedUpscale{
		At:        now,
		Base:      *h.s.Monitor.Approved,
		Requested: resources,
	}
}

func (h MonitorHandle) StartingUpscaleRequest(now time.Time, resources api.Resources) {
	h.s.Monitor.OngoingRequest = &ongoingMonitorRequest{
		Kind:      monitorRequestKindUpscale,
		Requested: resources,
	}
	h.s.Monitor.UpscaleFailureAt = nil
}

func (h MonitorHandle) UpscaleRequestSuccessful(now time.Time) {
	h.s.Monitor.Approved = &h.s.Monitor.OngoingRequest.Requested
	h.s.Monitor.OngoingRequest = nil
}

func (h MonitorHandle) UpscaleRequestFailed(now time.Time) {
	h.s.Monitor.OngoingRequest = nil
	h.s.Monitor.UpscaleFailureAt = &now
}

func (h MonitorHandle) StartingDownscaleRequest(now time.Time, resources api.Resources) {
	h.s.Monitor.OngoingRequest = &ongoingMonitorRequest{
		Kind:      monitorRequestKindDownscale,
		Requested: resources,
	}
	h.s.Monitor.DownscaleFailureAt = nil
}

func (h MonitorHandle) DownscaleRequestAllowed(now time.Time, rev vmv1.RevisionWithTime) {
	h.s.Monitor.Approved = &h.s.Monitor.OngoingRequest.Requested
	h.s.Monitor.OngoingRequest = nil
	revsource.Propagate(now,
		rev,
		&h.s.Monitor.CurrentRevision,
		h.s.Config.ObservabilityCallbacks.MonitorLatency,
	)
}

// Downscale request was successful but the monitor denied our request.
func (h MonitorHandle) DownscaleRequestDenied(now time.Time, targetRevision vmv1.RevisionWithTime) {
	h.s.Monitor.DeniedDownscale = &deniedDownscale{
		At:        now,
		Current:   *h.s.Monitor.Approved,
		Requested: h.s.Monitor.OngoingRequest.Requested,
	}
	h.s.Monitor.OngoingRequest = nil
	revsource.Propagate(now,
		targetRevision,
		&h.s.Monitor.CurrentRevision,
		h.s.Config.ObservabilityCallbacks.MonitorLatency,
	)
}

func (h MonitorHandle) DownscaleRequestFailed(now time.Time) {
	h.s.Monitor.OngoingRequest = nil
	h.s.Monitor.DownscaleFailureAt = &now
}

type NeonVMHandle struct {
	s *state
}

func (s *State) NeonVM() NeonVMHandle {
	return NeonVMHandle{&s.internal}
}

func (h NeonVMHandle) StartingRequest(now time.Time, resources api.Resources) {
	if report := h.s.Config.ObservabilityCallbacks.ActualScaling; report != nil {
		currentCU, currentOk := h.s.VM.Using().DivResources(h.s.Config.ComputeUnit)
		targetCU, targetOk := resources.DivResources(h.s.Config.ComputeUnit)

		if currentOk && targetOk {
			report(now, uint32(currentCU), uint32(targetCU))
		}
	}

	// FIXME: add time to ongoing request info (or maybe only in RequestFailed?)
	h.s.NeonVM.OngoingRequested = &resources
}

func (h NeonVMHandle) RequestSuccessful(now time.Time) {
	if h.s.NeonVM.OngoingRequested == nil {
		panic("received NeonVM().RequestSuccessful() update without ongoing request")
	}

	resources := *h.s.NeonVM.OngoingRequested

	// FIXME: This is actually incorrect; we shouldn't trust that the VM has already been updated
	// just because the request completed. It takes longer for the reconcile cycle(s) to make the
	// necessary changes.
	// See the comments in (*State).UpdatedVM() for more info.
	h.s.VM.SetUsing(resources)

	h.s.NeonVM.OngoingRequested = nil
}

func (h NeonVMHandle) RequestFailed(now time.Time) {
	h.s.NeonVM.OngoingRequested = nil
	h.s.NeonVM.RequestFailedAt = &now
}
