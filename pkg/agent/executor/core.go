package executor

// Consumers of pkg/agent/core, implementing the "executors" for each type of action. These are
// wrapped up into a single ExecutorCore type, which exposes some methods for the various executors.
//
// The executors use various abstract interfaces for the scheudler / NeonVM / informant. The
// implementations of those interfaces are defiend in ifaces.go

import (
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type Config = core.Config

type ExecutorCore struct {
	mu sync.Mutex

	stateLogger *zap.Logger

	core    *core.State
	actions *timedActions

	updates *util.Broadcaster
}

type ClientSet struct {
	Plugin  PluginInterface
	NeonVM  NeonVMInterface
	Monitor MonitorInterface
}

func NewExecutorCore(stateLogger *zap.Logger, vm api.VmInfo, config core.Config) *ExecutorCore {
	return &ExecutorCore{
		mu:          sync.Mutex{},
		stateLogger: stateLogger,
		core:        core.NewState(vm, config),
		actions:     nil, // (*ExecutorCore).getActions() checks if this is nil
		updates:     util.NewBroadcaster(),
	}
}

type ExecutorCoreWithClients struct {
	*ExecutorCore

	clients ClientSet
}

func (c *ExecutorCore) WithClients(clients ClientSet) ExecutorCoreWithClients {
	return ExecutorCoreWithClients{
		ExecutorCore: c,
		clients:      clients,
	}
}

type timedActions struct {
	calculatedAt time.Time
	actions      core.ActionSet
}

func (c *ExecutorCore) getActions() timedActions {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.actions == nil {
		// NOTE: Even though we cache the actions generated using time.Now(), it's *generally* ok.
		now := time.Now()
		c.stateLogger.Info("Recalculating ActionSet", zap.Time("now", now), zap.Any("state", c.core.Dump()))
		c.actions = &timedActions{calculatedAt: now, actions: c.core.NextActions(now)}
		c.stateLogger.Info("New ActionSet", zap.Time("now", now), zap.Any("actions", c.actions.actions))
	}

	return *c.actions
}

func (c *ExecutorCore) update(with func(*core.State)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// NB: We broadcast the update *before* calling with() because this gets us nicer ordering
	// guarantees in some cases.
	c.updates.Broadcast()
	c.actions = nil
	with(c.core)
}

// may change in the future
type StateDump = core.StateDump

// StateDump copies and returns the current state inside the executor
func (c *ExecutorCore) StateDump() StateDump {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.core.Dump()
}

// Updater returns a handle on the object used for making external changes to the ExecutorCore,
// beyond what's provided by the various client (ish) interfaces
func (c *ExecutorCore) Updater() ExecutorCoreUpdater {
	return ExecutorCoreUpdater{c}
}

// ExecutorCoreUpdater provides a common interface for external changes to the ExecutorCore
type ExecutorCoreUpdater struct {
	core *ExecutorCore
}

func (c ExecutorCoreUpdater) UpdateMetrics(metrics api.Metrics, withLock func()) {
	c.core.update(func(state *core.State) {
		state.UpdateMetrics(metrics)
		withLock()
	})
}

func (c ExecutorCoreUpdater) UpdatedVM(vm api.VmInfo, withLock func()) {
	c.core.update(func(state *core.State) {
		state.UpdatedVM(vm)
		withLock()
	})
}

// NewScheduler updates the inner state, calling (*core.State).Plugin().NewScheduler()
func (c ExecutorCoreUpdater) NewScheduler(withLock func()) {
	c.core.update(func(state *core.State) {
		state.Plugin().NewScheduler()
		withLock()
	})
}

// SchedulerGone updates the inner state, calling (*core.State).Plugin().SchedulerGone()
func (c ExecutorCoreUpdater) SchedulerGone(withLock func()) {
	c.core.update(func(state *core.State) {
		state.Plugin().SchedulerGone()
		withLock()
	})
}

func (c ExecutorCoreUpdater) ResetMonitor(withLock func()) {
	c.core.update(func(state *core.State) {
		state.Monitor().Reset()
		withLock()
	})
}

func (c ExecutorCoreUpdater) UpscaleRequested(resources api.MoreResources, withLock func()) {
	c.core.update(func(state *core.State) {
		state.Monitor().UpscaleRequested(time.Now(), resources)
		withLock()
	})
}

func (c ExecutorCoreUpdater) MonitorActive(active bool, withLock func()) {
	c.core.update(func(state *core.State) {
		state.Monitor().Active(active)
		withLock()
	})
}
