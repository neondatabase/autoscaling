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
	"math"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

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

	Metrics *api.Metrics
}

// resourceBounds tracks the uncertainty associated with possibly-failed requests.
//
// To explain why this is necessary, consider the following sequence of events:
//
// 1. autoscaler-agent scales down from 2 CU -> 1 CU
// 2. autoscaler-agent sends (informative) request to the scheduler plugin for 1 CU
// 3. scheduler plugin starts processing the request
// 4. autoscaler-agent times out waiting for the response, interprets the request as "failed"
// 5. scheduler plugin successfully processes the request
//
// From that point, if the autoscaler-agent decides that the VM should actually remain at 2 CU
// rather than downscaling, if it's only tracking successful requests, it may falsely infer that the
// scheduler has still approved it up to 2 CU.
//
// For that specific case, the "scheduler approved" value is only ever used as an upper bound - so
// we can probably just store a single value and continually min() that with any amount we request.
// But NeonVM requests, for example, are used both as an upper *and* lower bound (for scheduler and
// vm-monitor requests, respectively).
type resourceBounds struct {
	// Confirmed is the most recent value that we have received confirmation for.
	//
	// For the scheduler, this refers to the value of PluginResponse.Permit.
	// For the vm-monitor, this is the resources associated with the last successful upscale or
	// downscale request.
	// For NeonVM object changes, we *currently* (temporarily) set this value based on successful
	// requests (and not events from Kubernetes).
	// (for more detail, see https://github.com/neondatabase/autoscaling/issues/350#issuecomment-1857295638)
	//
	// When a request fails, this value is set to nil - we no longer know the state, and therefore
	// no longer have confirmation for this value.
	// (Technically, we could have designed this however we like. This model works out easier than
	// the other options.)
	Confirmed *api.Resources

	// Lower is the smallest possible value that we could have made effective - with or without
	// confirmation of such a request.
	//
	// For example, if we make a request to the scheduler saying we've scaled down to 1 CU, this
	// value will *immediately* be equal to 1 CU. Lower would remain 1 CU until we receive
	// confirmation for some value greater than 1 CU (most likely on a different request).
	Lower api.Resources
	// Upper is the largest possible value that we could have made effective - with or without
	// confirmation of such a request.
	//
	// This value is basically just the inverse of Lower.
	Upper api.Resources
}

// fixedResourceBounds returns a new resourceBounds with all values set to resources.
func fixedResourceBounds(resources api.Resources) resourceBounds {
	return resourceBounds{Confirmed: &resources, Lower: resources, Upper: resources}
}

// updateTentative sets the bounds according to a request that has started, but not necessarily
// confirmed yet.
func (r *resourceBounds) updateTentative(value api.Resources) {
	r.Lower = r.Lower.Min(value)
	r.Upper = r.Upper.Max(value)
}

// confirmConsistent updates r.Confirmed with the value, under the assumption that receiving
// confirmation for the value aligns the lower and upper bounds.
//
// Note: This is only suitable for situations where there are synchronization guarantees that mean
// that (a) the "true" confirmed value never changes without our request, and (b) a successful
// request means the "true" value was immediately updated.
func (r *resourceBounds) confirmConsistent(value api.Resources) {
	if value.HasFieldLessThan(r.Lower) {
		panic(fmt.Errorf(
			"confirmConsistent called with value that has field less than r.Lower: %+v has field less than %+v",
			value, r.Lower,
		))
	} else if value.HasFieldGreaterThan(r.Upper) {
		panic(fmt.Errorf(
			"confirmConsistent called with value that has field greater than r.Upper: %+v has field greater than %+v",
			value, r.Upper,
		))
	}

	r.Lower = value
	r.Upper = value
	r.Confirmed = &value
}

type pluginState struct {
	// OngoingRequest is true iff there is currently an ongoing request to *this* scheduler plugin.
	OngoingRequest bool
	// LastRequest, if not nil, gives information about the most recently started request to the
	// plugin (maybe unfinished!)
	LastRequest *pluginRequested
	// LastFailureAt, if not nil, gives the time of the most recent request failure
	LastFailureAt *time.Time
	// Permitted, if not nil, stores the range of values that the scheduler plugin *may* have
	// approved, depending on whether requests are successful.
	//
	// This field will be nil if we have not sent requests to *any* scheduler.
	Permitted *resourceBounds
}

type pluginRequested struct {
	At        time.Time
	Resources api.Resources
	Failed    bool
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
	Approved *resourceBounds

	// LastFailureAt, if not nil, stores the time at which a request most recently failed (where
	// "failed" means that some unexpected error occurred - rather than being just denied, in the
	// case of downscaling).
	LastFailureAt *time.Time
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
	CurrentResources resourceBounds
	RequestFailedAt  *time.Time
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
				OngoingRequest: false,
				LastRequest:    nil,
				LastFailureAt:  nil,
				Permitted:      nil,
			},
			Monitor: monitorState{
				OngoingRequest:   nil,
				RequestedUpscale: nil,
				DeniedDownscale:  nil,
				Approved:         nil,
				LastFailureAt:    nil,
			},
			NeonVM: neonvmState{
				LastSuccess:      nil,
				OngoingRequested: nil,
				CurrentResources: fixedResourceBounds(vm.Using()),
				RequestFailedAt:  nil,
			},
			Metrics: nil,
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
	var pluginRequestedPhase string = "<this string should not appear>"
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
			requiredWait = util.Min(requiredWait, *w)
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
	currentResources := s.NeonVM.CurrentResources.Upper
	requestResources := s.clampResources(
		currentResources,
		desiredResources,
		&currentResources, // don't decrease below VM using (decrease happens *before* telling the plugin)
		nil,               // but any increase is ok
	)
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
		s.Plugin.Permitted != nil &&
		s.Plugin.Permitted.Confirmed != nil &&
		s.Plugin.LastRequest.Resources.HasFieldGreaterThan(*s.Plugin.Permitted.Confirmed)
	if requestPreviouslyDenied {
		timeUntilRetryBackoffExpires = s.Plugin.LastRequest.At.Add(s.Config.PluginDeniedRetryWait).Sub(now)
	}

	waitingOnRetryBackoff := timeUntilRetryBackoffExpires > 0

	// changing the resources we're requesting from the plugin
	wantToRequestNewResources := s.Plugin.LastRequest != nil &&
		requestResources != s.Plugin.LastRequest.Resources
	// ... and this isn't a duplicate (or, at least it's been long enough)
	shouldRequestNewResources := wantToRequestNewResources && !waitingOnRetryBackoff

	// Can't make a duplicate request
	if s.Plugin.OngoingRequest {
		// ... but if the desired request is different from what we would be making,
		// then it's worth logging
		if s.Plugin.LastRequest.Resources != requestResources {
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
	if timeForRequest || shouldRequestNewResources || s.Plugin.LastRequest.Failed {
		var lastPermit *api.Resources
		if s.Plugin.Permitted != nil {
			// NOTE: We must send Permitted.Lower instead of Permitted.Confirmed because if our last
			// request was downscaling, and we falsely believe it failed, we could end up telling
			// the scheduler that it last permitted more than it actually did.
			lastPermit = &s.Plugin.Permitted.Lower
		}
		return &ActionPluginRequest{
			LastPermit: lastPermit,
			Target:     requestResources,
			Metrics:    s.Metrics,
		}, nil
	} else {
		if wantToRequestNewResources && waitingOnRetryBackoff {
			logFailureReason("but previous request for more resources was denied too recently")
		}
		waitTime := timeUntilNextRequestTick
		if waitingOnRetryBackoff {
			waitTime = util.Min(waitTime, timeUntilRetryBackoffExpires)
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
	// clamp desiredResources to what we're allowed to make a request for
	// TODO: After switching to VM events as the source of truth, need to modify this function to
	// handle cases where the VM spec changes outside of our control (ref #350); otherwise we'll end
	// up panicking because s.VM.Using() may suddenly become greater than plugin approved or less
	// than monitor approved.
	desiredResources = s.clampResources(
		s.NeonVM.CurrentResources.Lower, // current: what we're using already
		desiredResources,                // target: desired resources
		ptr(s.monitorApproved().Upper),  // lower bound: downscaling that the monitor has approved
		ptr(s.pluginApproved().Lower),   // upper bound: upscaling that the plugin has approved
	)

	// If we're already using the desired resources, then no need to make a request
	if current := s.NeonVM.CurrentResources.Confirmed; current != nil && *current == desiredResources {
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

		return &ActionNeonVMRequest{
			Current: s.VM.Using(),
			Target:  desiredResources,
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
		s.Monitor.Approved.Lower,                              // current: last resources we got the OK from the monitor on
		s.NeonVM.CurrentResources.Lower.Min(desiredResources), // target: what the VM is currently using
		&s.Monitor.Approved.Lower,                             // lower: don't decrease
		ptr(desiredResources.Max(s.Monitor.Approved.Upper)),   // don't increase above desired resources
	)

	// Clamp the request resources so we're not increasing by more than 1 CU:
	requestResources = s.clampResources(
		s.Monitor.Approved.Lower,
		requestResources,
		nil, // no lower bound
		ptr(requestResources.Add(s.Config.ComputeUnit)), // upper bound: must not increase by >1 CU
	)

	// Check validity of the request that we would send, before sending it
	if requestResources.HasFieldLessThan(s.Monitor.Approved.Lower) {
		panic(fmt.Errorf(
			"resources for vm-monitor upscaling are less than lower bound of what was last approved: %+v has field less than %+v",
			requestResources,
			s.Monitor.Approved.Lower,
		))
	}

	wantToDoRequest := requestResources.HasFieldGreaterThan(s.Monitor.Approved.Lower) &&
		(s.Monitor.Approved.Confirmed == nil || requestResources.HasFieldGreaterThan(*s.Monitor.Approved.Confirmed))
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
	if s.Monitor.LastFailureAt != nil {
		timeUntilFailureBackoffExpires := s.Monitor.LastFailureAt.Add(s.Config.MonitorRetryWait).Sub(now)
		if timeUntilFailureBackoffExpires > 0 {
			s.warn("Wanted to send vm-monitor upscale request, but failed too recently")
			return nil, &timeUntilFailureBackoffExpires
		}
	}

	// Otherwise, we can make the request:
	return &ActionMonitorUpscale{
		Current: s.Monitor.Approved.Lower,
		Target:  requestResources,
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
		s.Monitor.Approved.Upper,  // current: what the monitor is already aware of
		desiredResources,          // target: what we'd like the VM to be using
		nil,                       // lower bound: any decrease is fine
		&s.Monitor.Approved.Upper, // upper bound: don't increase (this is only downscaling!)
	)

	// Clamp the request resources so we're not decreasing by more than 1 CU:
	requestResources = s.clampResources(
		s.Monitor.Approved.Upper,
		requestResources,
		ptr(s.Monitor.Approved.Upper.SaturatingSub(s.Config.ComputeUnit)), // lower bound: must not decrease by >1 CU
		nil, // no upper bound
	)

	// Check validity of the request that we would send, before sending it
	if requestResources.HasFieldGreaterThan(s.Monitor.Approved.Upper) {
		panic(fmt.Errorf(
			"resources for vm-monitor downscaling are greater than upper bound of what was last approved: %+v has field greater than %+v",
			requestResources,
			s.Monitor.Approved.Upper,
		))
	}

	wantToDoRequest := requestResources.HasFieldLessThan(s.Monitor.Approved.Upper) &&
		(s.Monitor.Approved.Confirmed == nil || requestResources.HasFieldLessThan(*s.Monitor.Approved.Confirmed))
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
	if s.Monitor.LastFailureAt != nil {
		timeUntilFailureBackoffExpires := s.Monitor.LastFailureAt.Add(s.Config.MonitorRetryWait).Sub(now)
		if timeUntilFailureBackoffExpires > 0 {
			s.warn("Wanted to send vm-monitor downscale request, but failed too recently")
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
		Current: s.Monitor.Approved.Upper,
		Target:  requestResources,
	}, nil
}

func (s *state) scalingConfig() api.ScalingConfig {
	if s.VM.ScalingConfig != nil {
		return *s.VM.ScalingConfig
	} else {
		return s.Config.DefaultScalingConfig
	}
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

	var goalCU uint32
	if s.Metrics != nil {
		// For CPU:
		// Goal compute unit is at the point where (CPUs) × (LoadAverageFractionTarget) == (load
		// average),
		// which we can get by dividing LA by LAFT, and then dividing by the number of CPUs per CU
		goalCPUs := float64(s.Metrics.LoadAverage1Min) / s.scalingConfig().LoadAverageFractionTarget
		cpuGoalCU := uint32(math.Round(goalCPUs / s.Config.ComputeUnit.VCPU.AsFloat64()))

		// For Mem:
		// Goal compute unit is at the point where (Mem) * (MemoryUsageFractionTarget) == (Mem Usage)
		// We can get the desired memory allocation in bytes by dividing MU by MUFT, and then convert
		// that to CUs
		//
		// NOTE: use uint64 for calculations on bytes as uint32 can overflow
		memGoalBytes := api.Bytes(math.Round(float64(s.Metrics.MemoryUsageBytes) / s.scalingConfig().MemoryUsageFractionTarget))
		memGoalCU := uint32(memGoalBytes / s.Config.ComputeUnit.Mem)

		goalCU = util.Max(cpuGoalCU, memGoalCU)
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
			goalCU = util.Max(goalCU, reqCU)
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
			goalCU = util.Max(goalCU, reqCU)
		}
	}

	// resources for the desired "goal" compute units
	var goalResources api.Resources

	// If there's no constraints and s.metrics is nil, then we'll end up with goalCU = 0.
	// But if we have no metrics, we'd prefer to keep things as-is, rather than scaling down.
	if s.Metrics == nil && goalCU == 0 {
		goalResources = s.VM.Using()
	} else {
		goalResources = s.Config.ComputeUnit.Mul(uint16(goalCU))
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
			waitTime = util.Min(waitTime, timeUntilDeniedDownscaleExpired)
			waiting = true
		}
		if requestedUpscalingAffectedResult {
			waitTime = util.Min(waitTime, timeUntilRequestedUpscalingExpired)
			waiting = true
		}

		if waiting {
			return &waitTime
		} else {
			return nil
		}
	}

	s.info("Calculated desired resources", zap.Object("current", s.VM.Using()), zap.Object("target", result))

	return result, calculateWaitTime
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
		required = util.Max(required, 1+uint32(base.VCPU/computeUnit.VCPU))
	}
	if requested.Memory {
		required = util.Max(required, 1+uint32(base.Mem/computeUnit.Mem))
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

	return util.Max(requiredFromCPU, requiredFromMem)
}

func (s *state) minRequiredResourcesForDeniedDownscale(computeUnit api.Resources, denied deniedDownscale) api.Resources {
	// for each resource, increase the value by one CU's worth, but not greater than the value we
	// were at while attempting to downscale.
	//
	// phrasing it like this cleanly handles some subtle edge cases when denied.current isn't a
	// multiple of the compute unit.
	return api.Resources{
		VCPU: util.Min(denied.Current.VCPU, computeUnit.VCPU*(1+denied.Requested.VCPU/computeUnit.VCPU)),
		Mem:  util.Min(denied.Current.Mem, computeUnit.Mem*(1+denied.Requested.Mem/computeUnit.Mem)),
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
		cpu = util.Max(desired.VCPU, lowerBound.VCPU)
	} else if desired.VCPU > current.VCPU && upperBound != nil {
		cpu = util.Min(desired.VCPU, upperBound.VCPU)
	}

	mem := desired.Mem
	if desired.Mem < current.Mem && lowerBound != nil {
		mem = util.Max(desired.Mem, lowerBound.Mem)
	} else if desired.Mem > current.Mem && upperBound != nil {
		mem = util.Min(desired.Mem, upperBound.Mem)
	}

	return api.Resources{VCPU: cpu, Mem: mem}
}

func (s *state) monitorApproved() resourceBounds {
	if s.Monitor.Approved != nil {
		return *s.Monitor.Approved
	} else {
		return fixedResourceBounds(s.VM.Using())
	}
}

func (s *state) pluginApproved() resourceBounds {
	if s.Plugin.Permitted != nil {
		return *s.Plugin.Permitted
	} else {
		return fixedResourceBounds(s.VM.Using())
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
}

func (s *State) UpdateMetrics(metrics api.Metrics) {
	s.internal.Metrics = &metrics
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
		Failed:    false, // We'll set this later when the request completes.
	}
	h.s.Plugin.OngoingRequest = true
	if h.s.Plugin.Permitted == nil {
		h.s.Plugin.Permitted = ptr(fixedResourceBounds(h.s.NeonVM.CurrentResources.Lower))
		h.s.Plugin.Permitted.Confirmed = nil
	}
	h.s.Plugin.Permitted.updateTentative(resources)
}

func (h PluginHandle) RequestFailed(now time.Time) {
	h.s.Plugin.OngoingRequest = false
	h.s.Plugin.LastFailureAt = &now
	h.s.Plugin.LastRequest.Failed = true
}

func (h PluginHandle) RequestSuccessful(now time.Time, resp api.PluginResponse) (_err error) {
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
	h.s.Plugin.Permitted.confirmConsistent(resp.Permit)
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
		OngoingRequest:   nil,
		RequestedUpscale: nil,
		DeniedDownscale:  nil,
		Approved:         nil,
		LastFailureAt:    nil,
	}
}

func (h MonitorHandle) Active(active bool) {
	if active {
		approved := h.s.VM.Using()
		h.s.Monitor.Approved = ptr(fixedResourceBounds(approved)) // TODO: this is racy
	} else {
		h.s.Monitor.Approved = nil
	}
}

func (h MonitorHandle) UpscaleRequested(now time.Time, resources api.MoreResources) {
	h.s.Monitor.RequestedUpscale = &requestedUpscale{
		At:        now,
		Base:      *h.s.Monitor.Approved.Confirmed,
		Requested: resources,
	}
}

func (h MonitorHandle) StartingUpscaleRequest(now time.Time, resources api.Resources) {
	h.s.Monitor.Approved.updateTentative(resources)
	h.s.Monitor.OngoingRequest = &ongoingMonitorRequest{
		Kind:      monitorRequestKindUpscale,
		Requested: resources,
	}
}

func (h MonitorHandle) UpscaleRequestSuccessful(now time.Time) {
	h.s.Monitor.Approved.confirmConsistent(h.s.Monitor.OngoingRequest.Requested)
	h.s.Monitor.OngoingRequest = nil
	h.s.Monitor.LastFailureAt = nil
}

func (h MonitorHandle) UpscaleRequestFailed(now time.Time) {
	h.s.Monitor.OngoingRequest = nil
	h.s.Monitor.Approved.Confirmed = nil
	h.s.Monitor.LastFailureAt = &now
}

func (h MonitorHandle) StartingDownscaleRequest(now time.Time, resources api.Resources) {
	h.s.Monitor.Approved.updateTentative(resources)
	h.s.Monitor.OngoingRequest = &ongoingMonitorRequest{
		Kind:      monitorRequestKindDownscale,
		Requested: resources,
	}
}

func (h MonitorHandle) DownscaleRequestAllowed(now time.Time) {
	h.s.Monitor.Approved.confirmConsistent(h.s.Monitor.OngoingRequest.Requested)
	h.s.Monitor.OngoingRequest = nil
	h.s.Monitor.LastFailureAt = nil
}

// Downscale request was successful but the monitor denied our request.
func (h MonitorHandle) DownscaleRequestDenied(now time.Time) {
	h.s.Monitor.DeniedDownscale = &deniedDownscale{
		At:        now,
		Current:   h.s.Monitor.Approved.Upper,
		Requested: h.s.Monitor.OngoingRequest.Requested,
	}
	if h.s.Monitor.Approved.Confirmed != nil {
		h.s.Monitor.Approved.confirmConsistent(*h.s.Monitor.Approved.Confirmed)
	}
	h.s.Monitor.OngoingRequest = nil
	h.s.Monitor.LastFailureAt = nil // nb: "failed" means there was an unexpected error, not just being denied
}

func (h MonitorHandle) DownscaleRequestFailed(now time.Time) {
	h.s.Monitor.OngoingRequest = nil
	h.s.Monitor.Approved.Confirmed = nil
	h.s.Monitor.LastFailureAt = &now
}

type NeonVMHandle struct {
	s *state
}

func (s *State) NeonVM() NeonVMHandle {
	return NeonVMHandle{&s.internal}
}

func (h NeonVMHandle) StartingRequest(now time.Time, resources api.Resources) {
	// FIXME: add time to ongoing request info (or maybe only in RequestFailed?)
	h.s.NeonVM.OngoingRequested = &resources
	h.s.NeonVM.CurrentResources.updateTentative(resources)
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
	h.s.NeonVM.CurrentResources.confirmConsistent(resources)
	h.s.NeonVM.OngoingRequested = nil
}

func (h NeonVMHandle) RequestFailed(now time.Time) {
	h.s.NeonVM.OngoingRequested = nil
	h.s.NeonVM.RequestFailedAt = &now
	h.s.NeonVM.CurrentResources.Confirmed = nil
}
