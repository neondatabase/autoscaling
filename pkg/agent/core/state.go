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
// not violating our own guarantees.
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

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// Config represents some of the static configuration underlying the decision-making of State
type Config struct {
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

	// MonitorDownscaleFollowUpWait gives the minimum time we must wait between subsequent downscale
	// requests, *whether or not they are a duplicate*.
	MonitorDownscaleFollowUpWait time.Duration

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
	// ANY CHANGED FIELDS MUST BE UPDATED IN dumpstate.go AS WELL

	config Config

	// unused. Exists to make it easier to add print debugging (via .config.Warn) for a single call
	// to NextActions.
	debug bool

	// vm gives the current state of the VM - or at least, the state of the fields we care about.
	//
	// NB: any contents behind pointers in vm are immutable. Any time the field is updated, we
	// replace it with a fresh object.
	vm api.VmInfo

	// plugin records all state relevant to communications with the scheduler plugin
	plugin pluginState

	// monitor records all state relevant to communications with the vm-monitor
	monitor monitorState

	// neonvm records all state relevant to the NeonVM k8s API
	neonvm neonvmState

	metrics *api.Metrics
}

type pluginState struct {
	alive bool
	// ongoingRequest is true iff there is currently an ongoing request to *this* scheduler plugin.
	ongoingRequest bool
	// computeUnit, if not nil, gives the value of the compute unit we most recently got from a
	// PluginResponse
	computeUnit *api.Resources
	// lastRequest, if not nil, gives information about the most recently started request to the
	// plugin (maybe unfinished!)
	lastRequest *pluginRequested
	// lastFailureAt, if not nil, gives the time of the most recent request failure
	lastFailureAt *time.Time
	// permit, if not nil, stores the Permit in the most recent PluginResponse. This field will be
	// nil if we have not been able to contact *any* scheduler. If we switch schedulers, we trust
	// the old one.
	permit *api.Resources
}

type pluginRequested struct {
	at        time.Time
	resources api.Resources
}

type monitorState struct {
	ongoingRequest *ongoingMonitorRequest

	// requestedUpscale, if not nil, stores the most recent *unresolved* upscaling requested by the
	// vm-monitor, along with the time at which it occurred.
	requestedUpscale *requestedUpscale

	// deniedDownscale, if not nil, stores the result of the latest denied /downscale request.
	deniedDownscale *deniedDownscale

	// approved stores the most recent Resources associated with either (a) an accepted downscale
	// request, or (b) a successful upscale notification.
	approved *api.Resources

	// downscaleFailureAt, if not nil, stores the time at which a downscale request most recently
	// failed (where "failed" means that some unexpected error occurred, not that it was merely
	// denied).
	downscaleFailureAt *time.Time
	// upscaleFailureAt, if not nil, stores the time at which an upscale request most recently
	// failed
	upscaleFailureAt *time.Time
}

func (ms *monitorState) active() bool {
	return ms.approved != nil
}

type ongoingMonitorRequest struct {
	kind      monitorRequestKind
	requested api.Resources
}

type monitorRequestKind string

const (
	monitorRequestKindDownscale monitorRequestKind = "downscale"
	monitorRequestKindUpscale   monitorRequestKind = "upscale"
)

type requestedUpscale struct {
	at        time.Time
	base      api.Resources
	requested api.MoreResources
}

type deniedDownscale struct {
	at        time.Time
	current   api.Resources
	requested api.Resources
}

type neonvmState struct {
	lastSuccess *api.Resources
	// ongoingRequested, if not nil, gives the resources requested
	ongoingRequested *api.Resources
	requestFailedAt  *time.Time
}

func (ns *neonvmState) ongoingRequest() bool {
	return ns.ongoingRequested != nil
}

func NewState(vm api.VmInfo, config Config) *State {
	return &State{
		config: config,
		debug:  false,
		vm:     vm,
		plugin: pluginState{
			alive:          false,
			ongoingRequest: false,
			computeUnit:    nil,
			lastRequest:    nil,
			lastFailureAt:  nil,
			permit:         nil,
		},
		monitor: monitorState{
			ongoingRequest:     nil,
			requestedUpscale:   nil,
			deniedDownscale:    nil,
			approved:           nil,
			downscaleFailureAt: nil,
			upscaleFailureAt:   nil,
		},
		neonvm: neonvmState{
			lastSuccess:      nil,
			ongoingRequested: nil,
			requestFailedAt:  nil,
		},
		metrics: nil,
	}
}

func (s *State) info(msg string, fields ...zap.Field) {
	if s.config.Log.Info != nil {
		s.config.Log.Info(msg, fields...)
	}
}

func (s *State) warn(msg string /* , fields ...zap.Field */) {
	if s.config.Log.Warn != nil {
		s.config.Log.Warn(msg /* , fields... */)
	}
}

func (s *State) warnf(msg string, args ...any) {
	s.warn(fmt.Sprintf(msg, args...))
}

// NextActions is used to implement the state machine. It's a pure function that *just* indicates
// what the executor should do.
func (s *State) NextActions(now time.Time) ActionSet {
	var actions ActionSet

	desiredResources, calcDesiredResourcesWait := s.DesiredResourcesFromMetricsOrRequestedUpscaling(now)
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
	if s.plugin.ongoingRequest {
		pluginRequested = &s.plugin.lastRequest.resources
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
	// var monitorUpscaleRequestResources api.Resources

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

func (s *State) calculatePluginAction(
	now time.Time,
	desiredResources api.Resources,
) (*ActionPluginRequest, *time.Duration) {
	logFailureReason := func(reason string) {
		s.warnf("Wanted to make a request to the scheduler plugin, but %s", reason)
	}

	// additional resources we want to request OR previous downscaling we need to inform the plugin of
	// NOTE: only valid if s.plugin.permit != nil AND there's no ongoing NeonVM request.
	requestResources := s.clampResources(
		s.vm.Using(),
		desiredResources,
		ptr(s.vm.Using()), // don't decrease below VM using (decrease happens *before* telling the plugin)
		nil,               // but any increase is ok
	)
	// resources if we're just informing the plugin of current resource usage.
	currentResources := s.vm.Using()
	if s.neonvm.ongoingRequested != nil {
		// include any ongoing NeonVM request, because we're already using that.
		currentResources = currentResources.Max(*s.neonvm.ongoingRequested)
	}

	// We want to make a request to the scheduler plugin if:
	//  1. it's been long enough since the previous request (so we're obligated by PluginRequestTick); or
	//  2.a. we want to request resources / inform it of downscale;
	//    b. there isn't any ongoing, conflicting request; and
	//    c. we haven't recently been denied these resources
	var timeUntilNextRequestTick time.Duration
	if s.plugin.lastRequest != nil {
		timeUntilNextRequestTick = s.config.PluginRequestTick - now.Sub(s.plugin.lastRequest.at)
	}

	timeForRequest := timeUntilNextRequestTick <= 0 && s.plugin.alive

	var timeUntilRetryBackoffExpires time.Duration
	requestPreviouslyDenied := !s.plugin.ongoingRequest &&
		s.plugin.lastRequest != nil &&
		s.plugin.permit != nil &&
		s.plugin.lastRequest.resources.HasFieldGreaterThan(*s.plugin.permit)
	if requestPreviouslyDenied {
		timeUntilRetryBackoffExpires = s.plugin.lastRequest.at.Add(s.config.PluginDeniedRetryWait).Sub(now)
	}

	waitingOnRetryBackoff := timeUntilRetryBackoffExpires > 0

	// changing the resources we're requesting from the plugin
	wantToRequestNewResources := s.plugin.lastRequest != nil && s.plugin.permit != nil &&
		requestResources != *s.plugin.permit
	// ... and this isn't a duplicate (or, at least it's been long enough)
	shouldRequestNewResources := wantToRequestNewResources && !waitingOnRetryBackoff

	permittedRequestResources := requestResources
	if !shouldRequestNewResources {
		permittedRequestResources = currentResources
	}

	// Can't make a request if the plugin isn't active/alive
	if !s.plugin.alive {
		if timeForRequest || shouldRequestNewResources {
			logFailureReason("there isn't one active right now")
		}
		return nil, nil
	}

	// Can't make a duplicate request
	if s.plugin.ongoingRequest {
		// ... but if the desired request is different from what we would be making,
		// then it's worth logging
		if s.plugin.lastRequest.resources != permittedRequestResources {
			logFailureReason("there's already an ongoing request for different resources")
		}
		return nil, nil
	}

	// Can't make a request if we failed too recently
	if s.plugin.lastFailureAt != nil {
		timeUntilFailureBackoffExpires := s.plugin.lastFailureAt.Add(s.config.PluginRetryWait).Sub(now)
		if timeUntilFailureBackoffExpires > 0 {
			logFailureReason("previous request failed too recently")
			return nil, &timeUntilFailureBackoffExpires
		}
	}

	// At this point, all that's left is either making the request, or saying to wait.
	// The rest of the complication is just around accurate logging.
	if timeForRequest || shouldRequestNewResources {
		return &ActionPluginRequest{
			LastPermit: s.plugin.permit,
			Target:     permittedRequestResources,
			Metrics:    s.metrics,
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

func (s *State) calculateNeonVMAction(
	now time.Time,
	desiredResources api.Resources,
	pluginRequested *api.Resources,
	pluginRequestedPhase string,
) (*ActionNeonVMRequest, *time.Duration) {
	// clamp desiredResources to what we're allowed to make a request for
	desiredResources = s.clampResources(
		s.vm.Using(),                       // current: what we're using already
		desiredResources,                   // target: desired resources
		ptr(s.monitorApprovedLowerBound()), // lower bound: downscaling that the monitor has approved
		ptr(s.pluginApprovedUpperBound()),  // upper bound: upscaling that the plugin has approved
	)

	// If we're already using the desired resources, then no need to make a request
	if s.vm.Using() == desiredResources {
		return nil, nil
	}

	conflictingPluginRequest := pluginRequested != nil && pluginRequested.HasFieldLessThan(desiredResources)

	if !s.neonvm.ongoingRequest() && !conflictingPluginRequest {
		// We *should* be all clear to make a request; not allowed to make one if we failed too
		// recently
		if s.neonvm.requestFailedAt != nil {
			timeUntilFailureBackoffExpires := s.neonvm.requestFailedAt.Add(s.config.NeonVMRetryWait).Sub(now)
			if timeUntilFailureBackoffExpires > 0 {
				s.warn("Wanted to make a request to NeonVM API, but recent request failed too recently")
				return nil, &timeUntilFailureBackoffExpires
			}
		}

		return &ActionNeonVMRequest{
			Current: s.vm.Using(),
			Target:  desiredResources,
		}, nil
	} else {
		var reqs []string
		if s.plugin.ongoingRequest {
			reqs = append(reqs, fmt.Sprintf("plugin request %s", pluginRequestedPhase))
		}
		if s.neonvm.ongoingRequest() && *s.neonvm.ongoingRequested != desiredResources {
			reqs = append(reqs, "NeonVM request (for different resources) ongoing")
		}

		if len(reqs) != 0 {
			s.warnf("Wanted to make a request to NeonVM API, but there's already %s", strings.Join(reqs, " and "))
		}

		return nil, nil
	}
}

func (s *State) calculateMonitorUpscaleAction(
	now time.Time,
	desiredResources api.Resources,
) (*ActionMonitorUpscale, *time.Duration) {
	// can't do anything if we don't have an active connection to the vm-monitor
	if !s.monitor.active() {
		return nil, nil
	}

	requestResources := s.clampResources(
		*s.monitor.approved,      // current: last resources we got the OK from the monitor on
		s.vm.Using(),             // target: what the VM is currently using
		ptr(*s.monitor.approved), // don't decrease below what the monitor is currently set to (this is an *upscale* request)
		ptr(desiredResources.Max(*s.monitor.approved)), // don't increase above desired resources
	)

	// Check validity of the request that we would send, before sending it
	if requestResources.HasFieldLessThan(*s.monitor.approved) {
		panic(fmt.Errorf(
			"resources for vm-monitor upscaling are less than what was last approved: %+v has field less than %+v",
			requestResources,
			*s.monitor.approved,
		))
	}

	wantToDoRequest := requestResources != *s.monitor.approved
	if !wantToDoRequest {
		return nil, nil
	}

	// Can't make another request if there's already one ongoing
	if s.monitor.ongoingRequest != nil {
		var requestDescription string
		if s.monitor.ongoingRequest.kind == monitorRequestKindUpscale && s.monitor.ongoingRequest.requested != requestResources {
			requestDescription = "upscale request (for different resources)"
		} else if s.monitor.ongoingRequest.kind == monitorRequestKindDownscale {
			requestDescription = "downscale request"
		}

		if requestDescription != "" {
			s.warnf("Wanted to send vm-monitor upscale request, but waiting on ongoing %s", requestDescription)
		}
		return nil, nil
	}

	// Can't make another request if we failed too recently:
	if s.monitor.upscaleFailureAt != nil {
		timeUntilFailureBackoffExpires := s.monitor.upscaleFailureAt.Add(s.config.MonitorRetryWait).Sub(now)
		if timeUntilFailureBackoffExpires > 0 {
			s.warn("Wanted to send vm-monitor upscale request, but failed too recently")
			return nil, &timeUntilFailureBackoffExpires
		}
	}

	// Otherwise, we can make the request:
	return &ActionMonitorUpscale{
		Current: *s.monitor.approved,
		Target:  requestResources,
	}, nil
}

func (s *State) calculateMonitorDownscaleAction(
	now time.Time,
	desiredResources api.Resources,
	plannedUpscaleRequest bool,
) (*ActionMonitorDownscale, *time.Duration) {
	// can't do anything if we don't have an active connection to the vm-monitor
	if !s.monitor.active() {
		if desiredResources.HasFieldLessThan(s.vm.Using()) {
			s.warn("Wanted to send vm-monitor downscale request, but there's no active connection")
		}
		return nil, nil
	}

	requestResources := s.clampResources(
		*s.monitor.approved,      // current: what the monitor is already aware of
		desiredResources,         // target: what we'd like the VM to be using
		nil,                      // lower bound: any decrease is fine
		ptr(*s.monitor.approved), // upper bound: don't increase (this is only downscaling!)
	)

	// Check validity of the request that we would send, before sending it
	if requestResources.HasFieldGreaterThan(*s.monitor.approved) {
		panic(fmt.Errorf(
			"resources for vm-monitor downscaling are greater than what was last approved: %+v has field greater than %+v",
			requestResources,
			*s.monitor.approved,
		))
	}

	wantToDoRequest := requestResources != *s.monitor.approved
	if !wantToDoRequest {
		return nil, nil
	}

	// Can't make another request if there's already one ongoing (or if an upscaling request is
	// planned)
	if plannedUpscaleRequest {
		s.warn("Wanted to send vm-monitor downscale request, but waiting on other planned upscale request")
		return nil, nil
	} else if s.monitor.ongoingRequest != nil {
		var requestDescription string
		if s.monitor.ongoingRequest.kind == monitorRequestKindDownscale && s.monitor.ongoingRequest.requested != requestResources {
			requestDescription = "downscale request (for different resources)"
		} else if s.monitor.ongoingRequest.kind == monitorRequestKindUpscale {
			requestDescription = "upscale request"
		}

		if requestDescription != "" {
			s.warnf("Wanted to send vm-monitor downscale request, but waiting on other ongoing %s", requestDescription)
		}
		return nil, nil
	}

	// Can't make another request if we failed too recently:
	if s.monitor.downscaleFailureAt != nil {
		timeUntilFailureBackoffExpires := s.monitor.downscaleFailureAt.Add(s.config.MonitorRetryWait).Sub(now)
		if timeUntilFailureBackoffExpires > 0 {
			s.warn("Wanted to send vm-monitor downscale request but failed too recently")
			return nil, &timeUntilFailureBackoffExpires
		}
	}

	// Can't make another request if we were denied too recently:
	if s.monitor.deniedDownscale != nil {
		timeUntilDeniedDownscaleBackoffExpires := s.monitor.deniedDownscale.at.Add(s.config.MonitorDownscaleFollowUpWait).Sub(now)
		if timeUntilDeniedDownscaleBackoffExpires > 0 {
			s.warn("Wanted to send vm-monitor downscale request but too soon after previously denied downscaling")
			return nil, &timeUntilDeniedDownscaleBackoffExpires
		}
	}

	// Can't make another request if a recent request for resources less than or equal to the
	// proposed request was denied. In general though, this should be handled by
	// DesiredResourcesFromMetricsOrRequestedUpscaling, so it's we're better off panicking here.
	if s.timeUntilDeniedDownscaleExpired(now) > 0 && !s.monitor.deniedDownscale.requested.HasFieldLessThan(requestResources) {
		panic(errors.New(
			"Wanted to send vm-monitor downscale request, but too soon after previously denied downscaling that should have been handled earlier",
		))
	}

	// Nothing else to check, we're good to make the request
	return &ActionMonitorDownscale{
		Current: *s.monitor.approved,
		Target:  requestResources,
	}, nil
}

func (s *State) scalingConfig() api.ScalingConfig {
	if s.vm.ScalingConfig != nil {
		return *s.vm.ScalingConfig
	} else {
		return s.config.DefaultScalingConfig
	}
}

func (s *State) DesiredResourcesFromMetricsOrRequestedUpscaling(now time.Time) (api.Resources, func(ActionSet) *time.Duration) {
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

	// if we don't know what the compute unit is, don't do anything.
	if s.plugin.computeUnit == nil {
		s.warn("Can't determine desired resources because compute unit hasn't been set yet")
		return s.vm.Using(), nil
	}

	var goalCU uint32
	if s.metrics != nil {
		// For CPU:
		// Goal compute unit is at the point where (CPUs) × (LoadAverageFractionTarget) == (load
		// average),
		// which we can get by dividing LA by LAFT, and then dividing by the number of CPUs per CU
		goalCPUs := float64(s.metrics.LoadAverage1Min) / s.scalingConfig().LoadAverageFractionTarget
		cpuGoalCU := uint32(math.Round(goalCPUs / s.plugin.computeUnit.VCPU.AsFloat64()))

		// For Mem:
		// Goal compute unit is at the point where (Mem) * (MemoryUsageFractionTarget) == (Mem Usage)
		// We can get the desired memory allocation in bytes by dividing MU by MUFT, and then convert
		// that to CUs
		//
		// NOTE: use uint64 for calculations on bytes as uint32 can overflow
		memGoalBytes := uint64(math.Round(float64(s.metrics.MemoryUsageBytes) / s.scalingConfig().MemoryUsageFractionTarget))
		bytesPerCU := uint64(int64(s.plugin.computeUnit.Mem) * s.vm.Mem.SlotSize.Value())
		memGoalCU := uint32(memGoalBytes / bytesPerCU)

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
		reqCU := s.requiredCUForRequestedUpscaling(*s.plugin.computeUnit, *s.monitor.requestedUpscale)
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
		reqCU := s.requiredCUForDeniedDownscale(*s.plugin.computeUnit, s.monitor.deniedDownscale.requested)
		if reqCU > initialGoalCU {
			deniedDownscaleAffectedResult = true
			goalCU = util.Max(goalCU, reqCU)
		}
	}

	// resources for the desired "goal" compute units
	var goalResources api.Resources

	// If there's no constraints and s.metrics is nil, then we'll end up with goalCU = 0.
	// But if we have no metrics, we'd prefer to keep things as-is, rather than scaling down.
	if s.metrics == nil && goalCU == 0 {
		goalResources = s.vm.Using()
	} else {
		goalResources = s.plugin.computeUnit.Mul(uint16(goalCU))
	}

	// bound goalResources by the minimum and maximum resource amounts for the VM
	result := goalResources.Min(s.vm.Max()).Max(s.vm.Min())

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
		if !result.HasFieldGreaterThan(s.monitor.deniedDownscale.requested) {
			// This can only happen if s.vm.Max() is less than goalResources, because otherwise this
			// would have been factored into goalCU, affecting goalResources. Hence, the warning.
			s.warn("Can't decrease desired resources to within VM maximum because of vm-monitor previously denied downscale request")
		}
		preMaxResult := result
		result = result.Max(s.minRequiredResourcesForDeniedDownscale(*s.plugin.computeUnit, *s.monitor.deniedDownscale))
		if result != preMaxResult {
			deniedDownscaleAffectedResult = true
		}
	}

	// Check that the result is sound.
	//
	// With the current (naive) implementation, this is trivially ok. In future versions, it might
	// not be so simple, so it's good to have this integrity check here.
	if !deniedDownscaleAffectedResult && result.HasFieldGreaterThan(s.vm.Max()) {
		panic(fmt.Errorf(
			"produced invalid desired state: result has field greater than max. this = %+v", *s,
		))
	} else if result.HasFieldLessThan(s.vm.Min()) {
		panic(fmt.Errorf(
			"produced invalid desired state: result has field less than min. this = %+v", *s,
		))
	}

	calculateWaitTime := func(actions ActionSet) *time.Duration {
		var waiting bool
		waitTime := time.Duration(int64(1<<63 - 1)) // time.Duration is an int64. As an "unset" value, use the maximum.

		if deniedDownscaleAffectedResult && actions.MonitorDownscale == nil && s.monitor.ongoingRequest == nil {
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

	s.info("Calculated desired resources", zap.Object("current", s.vm.Using()), zap.Object("target", result))

	return result, calculateWaitTime
}

func (s *State) timeUntilRequestedUpscalingExpired(now time.Time) time.Duration {
	if s.monitor.requestedUpscale != nil {
		return s.monitor.requestedUpscale.at.Add(s.config.MonitorRequestedUpscaleValidPeriod).Sub(now)
	} else {
		return 0
	}
}

// NB: we could just use s.plugin.computeUnit or s.monitor.requestedUpscale from inside the
// function, but those are sometimes nil. This way, it's clear that it's the caller's responsibility
// to ensure that the values are non-nil.
func (s *State) requiredCUForRequestedUpscaling(computeUnit api.Resources, requestedUpscale requestedUpscale) uint32 {
	var required uint32
	requested := requestedUpscale.requested
	base := requestedUpscale.base

	// note: 1 + floor(x / M) gives the minimum integer value greater than x / M.

	if requested.Cpu {
		required = util.Max(required, 1+uint32(base.VCPU/computeUnit.VCPU))
	}
	if requested.Memory {
		required = util.Max(required, 1+uint32(base.Mem/computeUnit.Mem))
	}

	return required
}

func (s *State) timeUntilDeniedDownscaleExpired(now time.Time) time.Duration {
	if s.monitor.deniedDownscale != nil {
		return s.monitor.deniedDownscale.at.Add(s.config.MonitorDeniedDownscaleCooldown).Sub(now)
	} else {
		return 0
	}
}

// NB: like requiredCUForRequestedUpscaling, we make the caller provide the values so that it's
// more clear that it's the caller's responsibility to ensure the values are non-nil.
func (s *State) requiredCUForDeniedDownscale(computeUnit, deniedResources api.Resources) uint32 {
	// note: floor(x / M) + 1 gives the minimum integer value greater than x / M.
	requiredFromCPU := 1 + uint32(deniedResources.VCPU/computeUnit.VCPU)
	requiredFromMem := 1 + uint32(deniedResources.Mem/computeUnit.Mem)

	return util.Max(requiredFromCPU, requiredFromMem)
}

func (s *State) minRequiredResourcesForDeniedDownscale(computeUnit api.Resources, denied deniedDownscale) api.Resources {
	// for each resource, increase the value by one CU's worth, but not greater than the value we
	// were at while attempting to downscale.
	//
	// phrasing it like this cleanly handles some subtle edge cases when denied.current isn't a
	// multiple of the compute unit.
	return api.Resources{
		VCPU: util.Min(denied.current.VCPU, computeUnit.VCPU*vmapi.MilliCPU(1+uint32(denied.requested.VCPU/computeUnit.VCPU))),
		Mem:  util.Min(denied.current.Mem, computeUnit.Mem*(1+uint16(denied.requested.Mem/computeUnit.Mem))),
	}
}

// clampResources uses the directionality of the difference between s.vm.Using() and desired to
// clamp the desired resources with the upper *or* lower bound
func (s *State) clampResources(
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

func (s *State) monitorApprovedLowerBound() api.Resources {
	if s.monitor.approved != nil {
		return *s.monitor.approved
	} else {
		return s.vm.Using()
	}
}

func (s *State) pluginApprovedUpperBound() api.Resources {
	if s.plugin.permit != nil {
		return *s.plugin.permit
	} else {
		return s.vm.Using()
	}
}

//////////////////////////////////////////
// PUBLIC FUNCTIONS TO UPDATE THE STATE //
//////////////////////////////////////////

// Debug sets s.debug = enabled. This method is exclusively meant to be used in tests, to make it
// easier to enable print debugging only for a single call to NextActions, via s.warn() or otherwise.
func (s *State) Debug(enabled bool) {
	s.debug = enabled
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
	vm.SetUsing(s.vm.Using())
	s.vm = vm
}

func (s *State) UpdateMetrics(metrics api.Metrics) {
	s.metrics = &metrics
}

// PluginHandle provides write access to the scheduler plugin pieces of an UpdateState
type PluginHandle struct {
	s *State
}

func (s *State) Plugin() PluginHandle {
	return PluginHandle{s}
}

func (h PluginHandle) NewScheduler() {
	h.s.plugin = pluginState{
		alive:          true,
		ongoingRequest: false,
		computeUnit:    h.s.plugin.computeUnit, // Keep the previous scheduler's CU unless told otherwise
		lastRequest:    nil,
		lastFailureAt:  nil,
		permit:         h.s.plugin.permit, // Keep this; trust the previous scheduler.
	}
}

func (h PluginHandle) SchedulerGone() {
	h.s.plugin = pluginState{
		alive:          false,
		ongoingRequest: false,
		computeUnit:    h.s.plugin.computeUnit,
		lastRequest:    nil,
		lastFailureAt:  nil,
		permit:         h.s.plugin.permit, // Keep this; trust the previous scheduler.
	}
}

func (h PluginHandle) StartingRequest(now time.Time, resources api.Resources) {
	h.s.plugin.lastRequest = &pluginRequested{
		at:        now,
		resources: resources,
	}
	h.s.plugin.ongoingRequest = true
}

func (h PluginHandle) RequestFailed(now time.Time) {
	h.s.plugin.ongoingRequest = false
	h.s.plugin.lastFailureAt = &now
}

func (h PluginHandle) RequestSuccessful(now time.Time, resp api.PluginResponse) (_err error) {
	h.s.plugin.ongoingRequest = false
	defer func() {
		if _err != nil {
			h.s.plugin.lastFailureAt = &now
		}
	}()

	if err := resp.Permit.ValidateNonZero(); err != nil {
		return fmt.Errorf("Invalid permit: %w", err)
	}
	if err := resp.ComputeUnit.ValidateNonZero(); err != nil {
		return fmt.Errorf("Invalid compute unit: %w", err)
	}

	// Errors from resp in connection with the prior request
	if resp.Permit.HasFieldGreaterThan(h.s.plugin.lastRequest.resources) {
		return fmt.Errorf(
			"Permit has resources greater than request (%+v vs. %+v)",
			resp.Permit, h.s.plugin.lastRequest.resources,
		)
	}

	// Errors from resp in connection with the prior request AND the VM state
	if vmUsing := h.s.vm.Using(); resp.Permit.HasFieldLessThan(vmUsing) {
		return fmt.Errorf("Permit has resources less than VM (%+v vs %+v)", resp.Permit, vmUsing)
	}

	// All good - set everything.

	h.s.plugin.computeUnit = &resp.ComputeUnit
	h.s.plugin.permit = &resp.Permit
	return nil
}

// MonitorHandle provides write access to the vm-monitor pieces of an UpdateState
type MonitorHandle struct {
	s *State
}

func (s *State) Monitor() MonitorHandle {
	return MonitorHandle{s}
}

func (h MonitorHandle) Reset() {
	h.s.monitor = monitorState{
		ongoingRequest:     nil,
		requestedUpscale:   nil,
		deniedDownscale:    nil,
		approved:           nil,
		downscaleFailureAt: nil,
		upscaleFailureAt:   nil,
	}
}

func (h MonitorHandle) Active(active bool) {
	if active {
		approved := h.s.vm.Using()
		h.s.monitor.approved = &approved // TODO: this is racy
	} else {
		h.s.monitor.approved = nil
	}
}

func (h MonitorHandle) UpscaleRequested(now time.Time, resources api.MoreResources) {
	h.s.monitor.requestedUpscale = &requestedUpscale{
		at:        now,
		base:      *h.s.monitor.approved,
		requested: resources,
	}
}

func (h MonitorHandle) StartingUpscaleRequest(now time.Time, resources api.Resources) {
	h.s.monitor.ongoingRequest = &ongoingMonitorRequest{
		kind:      monitorRequestKindUpscale,
		requested: resources,
	}
	h.s.monitor.upscaleFailureAt = nil
}

func (h MonitorHandle) UpscaleRequestSuccessful(now time.Time) {
	h.s.monitor.approved = &h.s.monitor.ongoingRequest.requested
	h.s.monitor.ongoingRequest = nil
}

func (h MonitorHandle) UpscaleRequestFailed(now time.Time) {
	h.s.monitor.ongoingRequest = nil
	h.s.monitor.upscaleFailureAt = &now
}

func (h MonitorHandle) StartingDownscaleRequest(now time.Time, resources api.Resources) {
	h.s.monitor.ongoingRequest = &ongoingMonitorRequest{
		kind:      monitorRequestKindDownscale,
		requested: resources,
	}
	h.s.monitor.downscaleFailureAt = nil
}

func (h MonitorHandle) DownscaleRequestAllowed(now time.Time) {
	h.s.monitor.approved = &h.s.monitor.ongoingRequest.requested
	h.s.monitor.ongoingRequest = nil
}

// Downscale request was successful but the monitor denied our request.
func (h MonitorHandle) DownscaleRequestDenied(now time.Time) {
	h.s.monitor.deniedDownscale = &deniedDownscale{
		at:        now,
		current:   *h.s.monitor.approved,
		requested: h.s.monitor.ongoingRequest.requested,
	}
	h.s.monitor.ongoingRequest = nil
}

func (h MonitorHandle) DownscaleRequestFailed(now time.Time) {
	h.s.monitor.ongoingRequest = nil
	h.s.monitor.downscaleFailureAt = &now
}

type NeonVMHandle struct {
	s *State
}

func (s *State) NeonVM() NeonVMHandle {
	return NeonVMHandle{s}
}

func (h NeonVMHandle) StartingRequest(now time.Time, resources api.Resources) {
	// FIXME: add time to ongoing request info (or maybe only in RequestFailed?)
	h.s.neonvm.ongoingRequested = &resources
}

func (h NeonVMHandle) RequestSuccessful(now time.Time) {
	if h.s.neonvm.ongoingRequested == nil {
		panic("received NeonVM().RequestSuccessful() update without ongoing request")
	}

	resources := *h.s.neonvm.ongoingRequested

	// FIXME: This is actually incorrect; we shouldn't trust that the VM has already been updated
	// just because the request completed. It takes longer for the reconcile cycle(s) to make the
	// necessary changes.
	// See the comments in (*State).UpdatedVM() for more info.
	h.s.vm.Cpu.Use = resources.VCPU
	h.s.vm.Mem.Use = resources.Mem

	h.s.neonvm.ongoingRequested = nil
}

func (h NeonVMHandle) RequestFailed(now time.Time) {
	h.s.neonvm.ongoingRequested = nil
	h.s.neonvm.requestFailedAt = &now
}
