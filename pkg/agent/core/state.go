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
// vm-monitor is strictly synchonous. As such, there's some subtle logic to make sure that we're
// not violating our own guarantees.
//
// ---
// ¹ https://github.com/neondatabase/autoscaling/issues/23
// ² https://github.com/neondatabase/autoscaling/issues/350

import (
	"fmt"
	"math"
	"strings"
	"time"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// Config represents some of the static configuration underlying the decision-making of State
type Config struct {
	// DefaultScalingConfig is just copied from the global autoscaler-agent config.
	// If the VM's ScalingConfig is nil, we use this field instead.
	DefaultScalingConfig api.ScalingConfig

	// PluginRequestTick gives the period at which we should be making requests to the scheduler
	// plugin, even if nothing's changed.
	PluginRequestTick time.Duration

	// MonitorDeniedDownscaleCooldown gives the time we must wait between making duplicate
	// downscale requests to the vm-monitor where the previous failed.
	MonitorDeniedDownscaleCooldown time.Duration

	// MonitorRetryWait gives the amount of time to wait to retry after a *failed* request.
	MonitorRetryWait time.Duration

	// Warn provides an outlet for (*State).Next() to give warnings about conditions that are
	// impeding its ability to execute. (e.g. "wanted to do X but couldn't because of Y")
	Warn func(string, ...any) `json:"-"`
}

// State holds all of the necessary internal state for a VM in order to make scaling
// decisions
type State struct {
	// ANY CHANGED FIELDS MUST BE UPDATED IN dump.go AS WELL

	config Config

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
	// active is true iff the agent is currently "confirmed" and not "suspended" by the monitor.
	// Otherwise, we shouldn't be making any kind of scaling requests.
	active bool

	ongoingRequest *ongoingMonitorRequest

	// requestedUpscale, if not nil, stores the most recent *unresolved* upscaling requested by the
	// vm-monitor, along with the time at which it occurred.
	requestedUpscale *requestedUpscale

	// deniedDownscale, if not nil, stores the result of the lastest denied /downscale request.
	deniedDownscale *deniedDownscale

	// approved stores the most recent Resources associated with either (a) an accepted downscale
	// request, or (b) a successful upscale notification.
	approved *api.Resources

	downscaleFailureAt *time.Time
	upscaleFailureAt   *time.Time
}

type ongoingMonitorRequest struct {
	kind monitorRequestKind
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

func NewState(vm api.VmInfo, config Config) *State {
	return &State{
		config: config,
		vm:     vm,
		plugin: pluginState{
			alive:          false,
			ongoingRequest: false,
			computeUnit:    nil,
			lastRequest:    nil,
			permit:         nil,
		},
		monitor: monitorState{
			active:             false,
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

// NextActions is used to implement the state machine. It's a pure function that *just* indicates
// what the executor should do.
func (s *State) NextActions(now time.Time) ActionSet {
	var actions ActionSet

	using := s.vm.Using()

	desiredResources := s.DesiredResourcesFromMetricsOrRequestedUpscaling(now)

	desiredResourcesApprovedByMonitor := s.boundResourcesByMonitorApproved(desiredResources)
	desiredResourcesApprovedByPlugin := s.boundResourcesByPluginApproved(desiredResources)
	// NB: monitor approved provides a lower bound
	approvedDesiredResources := desiredResourcesApprovedByPlugin.Max(desiredResourcesApprovedByMonitor)

	ongoingNeonVMRequest := s.neonvm.ongoingRequested != nil

	var requestForPlugin api.Resources
	if s.plugin.permit == nil {
		// If we haven't yet gotten a proper plugin response, then we aren't allowed to ask for
		// anything beyond our current usage.
		requestForPlugin = using
	} else {
		// ... Otherwise, we should:
		//  1. "inform" the plugin of any downscaling since the previous permit
		//  2. "request" any desired upscaling relative to to the previous permit
		// with (2) taking priority over (1), if there's any conflicts.
		requestForPlugin = desiredResources.Max(using) // ignore "desired" downscaling with .Max(using)
	}

	// We want to make a request to the scheduler plugin if:
	//  1. we've waited long enough since the previous request; or
	//  2.a. we want to request resources / inform it of downscale; and
	//    b. there isn't any ongoing, conflicting request
	timeForNewPluginRequest := s.plugin.lastRequest == nil || now.Sub(s.plugin.lastRequest.at) >= s.config.PluginRequestTick
	shouldUpdatePlugin := s.plugin.lastRequest != nil &&
		// "we haven't tried requesting *these* resources from it yet, or we can retry requesting"
		(s.plugin.lastRequest.resources != requestForPlugin || timeForNewPluginRequest) &&
		!ongoingNeonVMRequest

	if !s.plugin.ongoingRequest && (timeForNewPluginRequest || shouldUpdatePlugin) && s.plugin.alive {
		if !shouldUpdatePlugin {
			// If we shouldn't "update" the plugin, then just inform it about the current resources
			// and metrics.
			actions.PluginRequest = &ActionPluginRequest{
				LastPermit: s.plugin.permit,
				Target:     using,
				Metrics:    s.metrics,
			}
		} else {
			// ... Otherwise, we should try requesting something new form it.
			actions.PluginRequest = &ActionPluginRequest{
				LastPermit: s.plugin.permit,
				Target:     desiredResourcesApprovedByMonitor,
				Metrics:    s.metrics,
			}
		}
	} else if timeForNewPluginRequest || shouldUpdatePlugin {
		if s.plugin.alive {
			s.config.Warn("Wanted to make a request to the plugin, but there's already one ongoing")
		} else {
			s.config.Warn("Wanted to make a request to the plugin, but there isn't one active right now")
		}
	}

	// We want to make a request to NeonVM if we've been approved for a change in resources that
	// we're not currently using.
	if approvedDesiredResources != using {
		// ... but we can't make one if there's already a request ongoing, either via the NeonVM API
		// or to the scheduler plugin, because they require taking out the request lock.
		if !ongoingNeonVMRequest && !s.plugin.ongoingRequest {
			actions.NeonVMRequest = &ActionNeonVMRequest{
				Current: using,
				Target:  approvedDesiredResources,
			}
		} else {
			var reqs []string
			if s.plugin.ongoingRequest {
				reqs = append(reqs, "plugin request")
			}
			if ongoingNeonVMRequest && *s.neonvm.ongoingRequested != approvedDesiredResources {
				reqs = append(reqs, "NeonVM request (for different resources)")
			}

			if len(reqs) != 0 {
				s.config.Warn("Wanted to make a request to NeonVM API, but there's already %s ongoing", strings.Join(reqs, " and "))
			}
		}
	}

	// We should make an upscale request to the monitor if we've upscaled and the monitor
	// doesn't know about it.
	wantMonitorUpscaleRequest := s.monitor.approved != nil && *s.monitor.approved != desiredResources.Max(*s.monitor.approved)
	// However, we may need to wait before retrying (or for any ongoing requests to finish)
	makeMonitorUpscaleRequest := wantMonitorUpscaleRequest &&
		s.monitor.active &&
		s.monitor.ongoingRequest == nil &&
		(s.monitor.upscaleFailureAt == nil ||
			now.Sub(*s.monitor.upscaleFailureAt) >= s.config.MonitorRetryWait)
	if wantMonitorUpscaleRequest {
		if makeMonitorUpscaleRequest {
			actions.MonitorUpscale = &ActionMonitorUpscale{
				Current: *s.monitor.approved,
				Target:  desiredResources.Max(*s.monitor.approved),
			}
		} else if !s.monitor.active {
			s.config.Warn("Wanted to send informant upscale request, but not active")
		} else if s.monitor.ongoingRequest != nil && s.monitor.ongoingRequest.kind != monitorRequestKindUpscale {
			s.config.Warn("Wanted to send informant upscale request, but waiting other ongoing %s request", s.monitor.ongoingRequest.kind)
		} else if s.monitor.ongoingRequest == nil {
			s.config.Warn("Wanted to send informant upscale request, but waiting on retry rate limit")
		}
	}

	// We should make a downscale request to the monitor if we want to downscale but haven't been
	// approved for it.
	var resourcesForMonitorDownscale api.Resources
	if s.monitor.approved != nil {
		resourcesForMonitorDownscale = desiredResources.Min(*s.monitor.approved)
	} else {
		resourcesForMonitorDownscale = desiredResources.Min(using)
	}
	wantMonitorDownscaleRequest := s.monitor.approved != nil && *s.monitor.approved != resourcesForMonitorDownscale
	if s.monitor.approved == nil && resourcesForMonitorDownscale != using {
		s.config.Warn("Wanted to send informant downscale request, but haven't yet gotten information about its resources")
	}
	// However, we may need to wait before retrying (or for any ongoing requests to finish)
	makeMonitorDownscaleRequest := wantMonitorDownscaleRequest &&
		s.monitor.active &&
		s.monitor.ongoingRequest == nil &&
		(s.monitor.deniedDownscale == nil ||
			s.monitor.deniedDownscale.requested != desiredResources.Min(using) ||
			now.Sub(s.monitor.deniedDownscale.at) >= s.config.MonitorDeniedDownscaleCooldown) &&
		(s.monitor.downscaleFailureAt == nil ||
			now.Sub(*s.monitor.downscaleFailureAt) >= s.config.MonitorRetryWait)

	if wantMonitorDownscaleRequest {
		if makeMonitorDownscaleRequest {
			actions.MonitorDownscale = &ActionMonitorDownscale{
				Current: *s.monitor.approved,
				Target:  resourcesForMonitorDownscale,
			}
		} else if !s.monitor.active {
			s.config.Warn("Wanted to send informant downscale request, but not active")
		} else if s.monitor.ongoingRequest != nil && s.monitor.ongoingRequest.kind != monitorRequestKindDownscale {
			s.config.Warn("Wanted to send informant downscale request, but waiting on other ongoing %s request", s.monitor.ongoingRequest.kind)
		} else if s.monitor.ongoingRequest == nil {
			s.config.Warn("Wanted to send informant downscale request, but waiting on retry rate limit")
		}
	}

	// --- and that's all the request types! ---

	// If there's anything waiting, we should also note how long we should wait for.
	// There's two components we could be waiting on: the scheduler plugin, and the vm-monitor.
	maximumDuration := time.Duration(int64(uint64(1)<<63 - 1))
	requiredWait := maximumDuration

	// We always need to periodically send messages to the plugin. If actions.PluginRequest == nil,
	// we know that either:
	//
	//   (a) s.plugin.lastRequestAt != nil (otherwise timeForNewPluginRequest == true); or
	//   (b) s.plugin.ongoingRequest == true (the only reason why we wouldn't've exited earlier)
	//
	// So we actually only need to explicitly wait if there's not an ongoing request - otherwise
	// we'll be notified anyways when the request is done.
	if actions.PluginRequest == nil && s.plugin.alive && !s.plugin.ongoingRequest {
		requiredWait = util.Min(requiredWait, now.Sub(s.plugin.lastRequest.at))
	}

	// For the vm-monitor:
	// if we wanted to make EITHER a downscale or upscale request, but we previously couldn't
	// because of retry timeouts, we should wait for s.config.MonitorRetryWait before trying
	// again.
	// OR if we wanted to downscale but got denied, we should wait for
	// s.config.MonitorDownscaleCooldown before retrying.
	if s.monitor.ongoingRequest == nil {
		// Retry upscale on failure
		if wantMonitorUpscaleRequest && s.monitor.upscaleFailureAt != nil {
			if wait := now.Sub(*s.monitor.upscaleFailureAt); wait >= s.config.MonitorRetryWait {
				requiredWait = util.Min(requiredWait, wait)
			}
		}
		// Retry downscale on failure
		if wantMonitorDownscaleRequest && s.monitor.downscaleFailureAt != nil {
			if wait := now.Sub(*s.monitor.downscaleFailureAt); wait >= s.config.MonitorRetryWait {
				requiredWait = util.Min(requiredWait, wait)
			}
		}
		// Retry downscale if denied
		if wantMonitorDownscaleRequest && s.monitor.deniedDownscale != nil && resourcesForMonitorDownscale == s.monitor.deniedDownscale.requested {
			if wait := now.Sub(s.monitor.deniedDownscale.at); wait >= s.config.MonitorDeniedDownscaleCooldown {
				requiredWait = util.Min(requiredWait, wait)
			}
		}
	}

	// If we're waiting on anything, add the action.
	if requiredWait != maximumDuration {
		actions.Wait = &ActionWait{Duration: requiredWait}
	}

	return actions
}

func (s *State) scalingConfig() api.ScalingConfig {
	if s.vm.ScalingConfig != nil {
		return *s.vm.ScalingConfig
	} else {
		return s.config.DefaultScalingConfig
	}
}

func (s *State) DesiredResourcesFromMetricsOrRequestedUpscaling(now time.Time) api.Resources {
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
		s.config.Warn("Can't determine desired resources because compute unit hasn't been set yet")
		return s.vm.Using()
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

	// Update goalCU based on any requested upscaling or downscaling that was previously denied
	goalCU = util.Max(goalCU, s.requiredCUForRequestedUpscaling(*s.plugin.computeUnit))
	deniedDownscaleInEffect := s.deniedDownscaleInEffect(now)
	if deniedDownscaleInEffect {
		goalCU = util.Max(goalCU, s.requiredCUForDeniedDownscale(*s.plugin.computeUnit, s.monitor.deniedDownscale.requested))
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
			s.config.Warn("Can't decrease desired resources to within VM maximum because of vm-monitor previously denied downscale request")
		}
		result = result.Max(s.minRequiredResourcesForDeniedDownscale(*s.plugin.computeUnit, *s.monitor.deniedDownscale))
	}

	// Check that the result is sound.
	//
	// With the current (naive) implementation, this is trivially ok. In future versions, it might
	// not be so simple, so it's good to have this integrity check here.
	if !deniedDownscaleInEffect && result.HasFieldGreaterThan(s.vm.Max()) {
		panic(fmt.Errorf(
			"produced invalid desired state: result has field greater than max. this = %+v", *s,
		))
	} else if result.HasFieldLessThan(s.vm.Min()) {
		panic(fmt.Errorf(
			"produced invalid desired state: result has field less than min. this = %+v", *s,
		))
	}

	return result
}

// NB: we could just use s.plugin.computeUnit, but that's sometimes nil. This way, it's clear that
// it's the caller's responsibility to ensure that s.plugin.computeUnit != nil.
func (s *State) requiredCUForRequestedUpscaling(computeUnit api.Resources) uint32 {
	if s.monitor.requestedUpscale == nil {
		return 0
	}

	var required uint32
	requested := s.monitor.requestedUpscale.requested

	// note: 1 + floor(x / M) gives the minimum integer value greater than x / M.

	if requested.Cpu {
		required = util.Max(required, 1+uint32(s.vm.Cpu.Use/computeUnit.VCPU))
	}
	if requested.Memory {
		required = util.Max(required, 1+uint32(s.vm.Mem.Use/computeUnit.Mem))
	}

	return required
}

func (s *State) deniedDownscaleInEffect(now time.Time) bool {
	return s.monitor.deniedDownscale != nil &&
		// Previous denied downscaling attempts are in effect until the cooldown expires
		now.Before(s.monitor.deniedDownscale.at.Add(s.config.MonitorDeniedDownscaleCooldown))
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
	var res api.Resources

	if denied.requested.VCPU < denied.current.VCPU {
		// increase the value by one CU's worth
		res.VCPU = computeUnit.VCPU * vmapi.MilliCPU(1+uint32(denied.requested.VCPU/computeUnit.VCPU))
	}

	if denied.requested.Mem < denied.current.Mem {
		res.Mem = computeUnit.Mem * (1 + uint16(denied.requested.Mem/computeUnit.Mem))
	}

	return res
}

func (s *State) boundResourcesByMonitorApproved(resources api.Resources) api.Resources {
	var lowerBound api.Resources
	if s.monitor.approved != nil {
		lowerBound = *s.monitor.approved
	} else {
		lowerBound = s.vm.Using()
	}
	return resources.Max(lowerBound)
}

func (s *State) boundResourcesByPluginApproved(resources api.Resources) api.Resources {
	var upperBound api.Resources
	if s.plugin.permit != nil {
		upperBound = *s.plugin.permit
	} else {
		upperBound = s.vm.Using()
	}
	return resources.Min(upperBound)
}

//////////////////////////////////////////
// PUBLIC FUNCTIONS TO UPDATE THE STATE //
//////////////////////////////////////////

func (s *State) UpdatedVM(vm api.VmInfo) {
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
		computeUnit:    nil,
		lastRequest:    nil,
		permit:         h.s.plugin.permit, // Keep this; trust the previous scheduler.
	}
}

func (h PluginHandle) SchedulerGone() {
	h.s.plugin = pluginState{
		alive:          false,
		ongoingRequest: false,
		computeUnit:    h.s.plugin.computeUnit,
		lastRequest:    nil,
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
}

func (h PluginHandle) RequestSuccessful(now time.Time, resp api.PluginResponse) error {
	h.s.plugin.ongoingRequest = false

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
		active:             false,
		ongoingRequest:     nil,
		requestedUpscale:   nil,
		deniedDownscale:    nil,
		approved:           nil,
		downscaleFailureAt: nil,
		upscaleFailureAt:   nil,
	}
}

func (h MonitorHandle) Active(active bool) {
	h.s.monitor.active = active
}

func (h MonitorHandle) UpscaleRequested(now time.Time, resources api.MoreResources) {
	h.s.monitor.requestedUpscale = &requestedUpscale{
		at:        now,
		base:      h.s.vm.Using(), // TODO: this is racy (maybe the resources were different when the monitor originally made the request)
		requested: resources,
	}
}

func (h MonitorHandle) StartingUpscaleRequest(now time.Time) {
	h.s.monitor.ongoingRequest = &ongoingMonitorRequest{kind: monitorRequestKindUpscale}
	h.s.monitor.upscaleFailureAt = nil
}

func (h MonitorHandle) UpscaleRequestSuccessful(now time.Time, resources api.Resources) {
	h.s.monitor.ongoingRequest = nil
	h.s.monitor.approved = &resources
}

func (h MonitorHandle) UpscaleRequestFailed(now time.Time) {
	h.s.monitor.ongoingRequest = nil
	h.s.monitor.upscaleFailureAt = &now
}

func (h MonitorHandle) StartingDownscaleRequest(now time.Time) {
	h.s.monitor.ongoingRequest = &ongoingMonitorRequest{kind: monitorRequestKindDownscale}
	h.s.monitor.downscaleFailureAt = nil
}

func (h MonitorHandle) DownscaleRequestAllowed(now time.Time, requested api.Resources) {
	h.s.monitor.ongoingRequest = nil
	h.s.monitor.approved = &requested
	h.s.monitor.deniedDownscale = nil
}

// Downscale request was successful but the monitor denied our request.
func (h MonitorHandle) DownscaleRequestDenied(now time.Time, current, requested api.Resources) {
	h.s.monitor.ongoingRequest = nil
	h.s.monitor.deniedDownscale = &deniedDownscale{
		at:        now,
		current:   current,
		requested: requested,
	}
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
	h.s.vm.Cpu.Use = resources.VCPU
	h.s.vm.Mem.Use = resources.Mem

	h.s.neonvm.ongoingRequested = nil
}

func (h NeonVMHandle) RequestFailed(now time.Time) {
	h.s.neonvm.ongoingRequested = nil
}
