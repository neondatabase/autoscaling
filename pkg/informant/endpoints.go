package informant

// This file contains the high-level handlers for various HTTP endpoints

import (
	"errors"
	"fmt"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// State is the global state of the informant
type State struct {
	agents *AgentSet
	cgroup *CgroupState
}

// NewStateOpts are individual options provided to NewState
type NewStateOpts struct {
	setFields func(*State)
	post      func(*State) error
}

// NewState instantiates a new State object, starting whatever background processes might be
// required
//
// Optional configuration may be provided by NewStateOpts.
//
// When WithCgroup is provided, the State object will handle "memory high" events for the cgroup,
// and
func NewState(agents *AgentSet, opts ...NewStateOpts) (*State, error) {
	s := &State{
		agents: agents,
		cgroup: nil,
	}
	for _, opt := range opts {
		opt.setFields(s)
	}
	for _, opt := range opts {
		if err := opt.post(s); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// WithCgroup creates a NewStateOpts that sets its CgroupHandler
//
// This function will panic if the provided CgroupConfig is invalid.
func WithCgroup(cgm *CgroupManager, config CgroupConfig) NewStateOpts {
	if config.OOMBufferBytes == 0 {
		panic("invalid CgroupConfig: OOMBufferBytes == 0")
	} else if config.MaxUpscaleWaitMillis == 0 {
		panic("invalid CgroupConfig: MaxUpscaleWaitMillis == 0")
	}

	return NewStateOpts{
		setFields: func(s *State) {
			if s.cgroup != nil {
				panic("WithCgroupHandler option provided more than once")
			}

			upscaleEventsSendr, upscaleEventsRecvr := util.NewCondChannelPair()
			s.cgroup = &CgroupState{
				mgr:                cgm,
				config:             config,
				upscaleEventsSendr: upscaleEventsSendr,
				upscaleEventsRecvr: upscaleEventsRecvr,
			}
		},
		post: func(s *State) error {
			// FIXME: This is technically racy across restarts. The sequence would be:
			//  1. Respond "ok" to a downscale request
			//  2. Restart
			//  3. Read system memory
			//  4. Get downscaled (as approved earlier)
			// A potential way to fix this would be writing to a file to record approved downscale
			// operations.
			if err := s.cgroup.setMemoryHigh(); err != nil {
				return fmt.Errorf("Error setting initial cgroup memory.high: %w", err)
			}
			go s.cgroup.handleCgroupSignalsLoop(config)
			return nil
		},
	}
}

// RegisterAgent registers a new or updated autoscaler-agent
//
// Returns: body (if successful), status code, error (if unsuccessful)
func (s *State) RegisterAgent(info *api.AgentDesc) (*api.InformantDesc, int, error) {
	protoVersion, status, err := s.agents.RegisterNewAgent(info)
	if err != nil {
		return nil, status, err
	}

	desc := api.InformantDesc{
		ProtoVersion: protoVersion,
		MetricsMethod: api.InformantMetricsMethod{
			Prometheus: &api.MetricsMethodPrometheus{Port: PrometheusPort},
		},
	}

	return &desc, 200, nil
}

// TryDownscale tries to downscale the VM's current resource usage, returning whether the proposed
// amount is ok
//
// Returns: body (if successful), status code and error (if unsuccessful)
func (s *State) TryDownscale(target *api.RawResources) (*api.DownscaleResult, int, error) {
	// If we aren't interacting with a cgroup, then we don't need to do anything.
	if s.cgroup == nil {
		return &api.DownscaleResult{Ok: true, Status: "No action taken (no cgroup enabled)"}, 200, nil
	}

	requestedMem := uint64(target.Memory.Value())
	newMemHigh := s.cgroup.config.calculateMemoryHighValue(requestedMem)
	ok, status, err := func() (bool, string, error) {
		s.cgroup.updateLock.Lock()
		defer s.cgroup.updateLock.Unlock()

		mib := float64(1 << 20) // 1 MiB = 2^20 bytes

		current, err := s.cgroup.getCurrentMemory()
		if err != nil {
			return false, "", fmt.Errorf("Error fetching getting cgroup memory: %w", err)
		}

		// For an explanation, refer to the documentation of CgroupConfig.MemoryHighBufferBytes
		//
		// TODO: this should be a method on (*CgroupConfig).
		if newMemHigh < current+s.cgroup.config.MemoryHighBufferBytes {

			verdict := "Calculated memory.high too low"
			status := fmt.Sprintf(
				"%s: %v MiB (new high) < %v MiB (current usage) + %v MiB (buffer)",
				verdict,
				float64(newMemHigh)/mib, float64(current)/mib,
				float64(s.cgroup.config.MemoryHighBufferBytes)/mib,
			)

			return false, status, nil
		}

		// Downscale:
		//
		// TODO: see similar note above. We shouldn't call methods on s.cgroup.mgr from here.
		if err := s.cgroup.mgr.SetHighMem(newMemHigh); err != nil {
			return false, "", fmt.Errorf("Error setting cgroup memory.high: %w", err)
		}

		status := fmt.Sprintf("Set cgroup memory.high down to %v MiB", float64(newMemHigh)/mib)
		return true, status, nil
	}()

	if err != nil {
		klog.Errorf("Internal error handling downscale request: %s", err)
		return nil, 500, errors.New("Internal error")
	}

	return &api.DownscaleResult{Ok: ok, Status: status}, 200, nil
}

// NotifyUpscale signals that the VM's resource usage has been increased to the new amount
//
// Returns: body (if successful), status code and error (if unsuccessful)
func (s *State) NotifyUpscale(newResources *api.RawResources) (*struct{}, int, error) {
	s.agents.ReceivedUpscale()
	return &struct{}{}, 200, nil
}

// UnregisterAgent unregisters the autoscaler-agent given by info, if it is currently registered
//
// If a different autoscaler-agent is currently registered, this method will do nothing.
//
// Returns: body (if successful), status code and error (if unsuccessful)
func (s *State) UnregisterAgent(info *api.AgentDesc) (*api.UnregisterAgent, int, error) {
	agent, ok := s.agents.Get(info.AgentID)
	if !ok {
		return nil, 404, fmt.Errorf("No agent with ID %q", info.AgentID)
	} else if agent.serverAddr != info.ServerAddr {
		// On our side, log the address we're expecting, but don't give that to the client
		klog.Warningf(
			"Agent serverAddr is incorrect, got %q but expected %q",
			info.ServerAddr, agent.serverAddr,
		)
		return nil, 400, fmt.Errorf("Agent serverAddr is incorrect, got %q", info.ServerAddr)
	}

	wasActive := agent.EnsureUnregistered()
	return &api.UnregisterAgent{WasActive: wasActive}, 200, nil
}
