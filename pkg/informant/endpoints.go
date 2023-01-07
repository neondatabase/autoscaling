package informant

// This file contains the high-level handlers for various HTTP endpoints

import (
	"fmt"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
)

// State is the global state of the informant
type State struct {
	agents *AgentSet
}

// NewState instantiates a new State object
func NewState(agents *AgentSet) *State {
	return &State{
		agents: agents,
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
func (s *State) TryDownscale(target *api.RawResources) (*bool, int, error) {
	// Currently, the implementation always returns that it's ok downscaling.
	ok := true
	return &ok, 200, nil
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
