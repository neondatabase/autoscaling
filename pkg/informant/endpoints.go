package informant

// This file contains the high-level handlers for various HTTP endpoints

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// State is the global state of the informant
type State struct {
	agents     *AgentSet
	dispatcher *Dispatcher
	requests   <-chan struct{}
}

func NewState(logger *zap.Logger) (state State, _ error) {
	logger.Info("Creating new agent-set.")
	agents := NewAgentSet(logger)
	requests := make(chan struct{})
	logger.Info("Creating new dispatcher.")
	disp, err := NewDispatcher("ws://127.0.0.1:10369", logger, requests)
	if err != nil {
		return state, err
	}

	logger.Info("Spawning goroutine to run dispatcher.")
	// Start the dispatcher
	go disp.run()

	// Listen for upscale notifications
	logger.Info("Spawning goroutine to listen for upscale requests.")
	go func() {
		for {
			<-requests
			agents.RequestUpscale(agents.baseLogger)
		}
	}()

	return State{agents: agents, dispatcher: &disp, requests: requests}, nil
}

// RegisterAgent registers a new or updated autoscaler-agent
//
// Returns: body (if successful), status code, error (if unsuccessful)
func (s *State) RegisterAgent(ctx context.Context, logger *zap.Logger, info *api.AgentDesc) (*api.InformantDesc, int, error) {
	logger = logger.With(agentZapField(info.AgentID, info.ServerAddr))

	protoVersion, status, err := s.agents.RegisterNewAgent(logger, info)
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

// HealthCheck is a dummy endpoint that allows the autoscaler-agent to check that (a) the informant
// is up and running, and (b) the agent is still registered.
//
// Returns: body (if successful), status code, error (if unsuccessful)
func (s *State) HealthCheck(ctx context.Context, logger *zap.Logger, info *api.AgentIdentification) (*api.InformantHealthCheckResp, int, error) {
	agent, ok := s.agents.Get(info.AgentID)
	if !ok {
		return nil, 404, fmt.Errorf("No Agent with ID %s registered", info.AgentID)
	} else if !agent.protoVersion.AllowsHealthCheck() {
		return nil, 400, fmt.Errorf("health checks are not supported in protocol version %v", agent.protoVersion)
	}

	return &api.InformantHealthCheckResp{}, 200, nil
}

// TryDownscale tries to downscale the VM's current resource usage, returning whether the proposed
// amount is ok.
//
// Returns: body (if successful), status code and error (if unsuccessful)
func (s *State) TryDownscale(ctx context.Context, logger *zap.Logger, target *api.AgentResourceMessage) (*api.DownscaleResult, int, error) {
	cpu := uint64(target.Data.Cpu.Value())
	mem := uint64(target.Data.Memory.Value())

	logger.Info("Sending try downscale.",
		zap.Uint64("cpu", cpu),
		zap.Uint64("mem", mem),
	)
	currentId := s.agents.CurrentIdStr()
	incomingId := target.Data.Id.AgentID.String()

	// First verify agent's authenticity before doing anything.
	// Note: if the current agent is nil, its id string will be "<nil>", which
	// does not match any valid UUID
	if incomingId != currentId {
		return nil, 400, fmt.Errorf("Agent ID %s is not the active Agent", incomingId)
	}

	tx, rx := util.Oneshot[MonitorResult]()

	s.dispatcher.Call(
		ctx,
		Request{
			RequestUpscale: nil,
			NotifyUpscale:  nil,
			TryDownscale: &Resources{
				Cpu: cpu,
				Mem: mem,
			},
		},
		tx,
	)

	// Wait for result
	res := rx.Recv().Result

	return res.Into(), 200, nil
}

// NotifyUpscale signals that the VM's resource usage has been increased to the new amount
//
// Returns: body (if successful), status code and error (if unsuccessful)
func (s *State) NotifyUpscale(
	ctx context.Context,
	logger *zap.Logger,
	newResources *api.AgentResourceMessage,
) (*struct{}, int, error) {
	// FIXME: we shouldn't just trust what the agent says
	//
	// Because of race conditions like in <https://github.com/neondatabase/autoscaling/issues/23>,
	// it's possible for us to receive a notification on /upscale *before* NeonVM actually adds the
	// memory.
	//
	// So until the race condition described in #23 is fixed, we have to just trust that the agent
	// is telling the truth, *especially because it might not be*.
	cpu := uint64(newResources.Data.Cpu.Value())
	mem := uint64(newResources.Data.Memory.Value())

	logger.Info("Sending NotifyUpscale to monitor",
		zap.Uint64("cpu", cpu),
		zap.Uint64("mem", mem),
	)

	currentId := s.agents.CurrentIdStr()
	incomingId := newResources.Data.Id.AgentID.String()

	// First verify agent's authenticity before doing anything.
	// Note: if the current agent is nil, its id string will be "<nil>", which
	// does not match any valid UUID
	if incomingId != currentId {
		return nil, 400, fmt.Errorf("Agent ID %s is not the active Agent", incomingId)
	}

	s.agents.ReceivedUpscale()

	tx, rx := util.Oneshot[MonitorResult]()
	s.dispatcher.Call(
		ctx,
		Request{
			RequestUpscale: nil,
			NotifyUpscale: &Resources{
				Cpu: cpu,
				Mem: mem,
			},
			TryDownscale: nil,
		},
		tx,
	)

	// Wait for result
	res := rx.Recv().Confirmation

	return &res, 200, nil
}

// UnregisterAgent unregisters the autoscaler-agent given by info, if it is currently registered
//
// If a different autoscaler-agent is currently registered, this method will do nothing.
//
// Returns: body (if successful), status code and error (if unsuccessful)
func (s *State) UnregisterAgent(ctx context.Context, logger *zap.Logger, info *api.AgentDesc) (*api.UnregisterAgent, int, error) {
	agent, ok := s.agents.Get(info.AgentID)
	if !ok {
		return nil, 404, fmt.Errorf("No agent with ID %q", info.AgentID)
	} else if agent.serverAddr != info.ServerAddr {
		// On our side, log the address we're expecting, but don't give that to the client
		logger.Warn(fmt.Sprintf(
			"Agent serverAddr is incorrect, got %q but expected %q",
			info.ServerAddr, agent.serverAddr,
		))
		return nil, 400, fmt.Errorf("Agent serverAddr is incorrect, got %q", info.ServerAddr)
	}

	wasActive := agent.EnsureUnregistered(logger)
	return &api.UnregisterAgent{WasActive: wasActive}, 200, nil
}
