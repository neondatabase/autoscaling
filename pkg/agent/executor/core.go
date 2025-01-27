package executor

// Consumers of pkg/agent/core, implementing the "executors" for each type of action. These are
// wrapped up into a single ExecutorCore type, which exposes some methods for the various executors.
//
// The executors use various abstract interfaces for the scheduler plugin / NeonVM / vm-monitor, and
// are defined in exec_*.go. The implementations of those interfaces are defined in execbridge.go.
//
// Each of the methods to modify ExecutorCore take 'withLock' as a callback that runs while the lock
// is held. In general, this is used for logging, so that the log output strictly matches the
// ordering of the changes to the underlying core.State, which should help with debugging.
//
// For more, see pkg/agent/ARCHITECTURE.md.

import (
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type Config struct {
	// OnNextActions is called each time the ExecutorCore calls (*core.State).NextActions() on the
	// inner state object.
	//
	// In practice, this value is set to a callback that increments a metric.
	OnNextActions func()

	Core core.Config
}

type ExecutorCore struct {
	mu sync.Mutex

	stateLogger *zap.Logger

	core *core.State

	actions       *timedActions
	lastActionsID timedActionsID
	onNextActions func()

	updates *util.Broadcaster
}

type ClientSet struct {
	Plugin  PluginInterface
	NeonVM  NeonVMInterface
	Monitor MonitorInterface
}

func NewExecutorCore(stateLogger *zap.Logger, vm api.VmInfo, config Config) *ExecutorCore {
	return &ExecutorCore{
		mu:            sync.Mutex{},
		stateLogger:   stateLogger,
		core:          core.NewState(vm, config.Core),
		actions:       nil, // (*ExecutorCore).getActions() checks if this is nil
		lastActionsID: -1,
		onNextActions: config.OnNextActions,
		updates:       util.NewBroadcaster(),
	}
}

// ExecutorCoreWithClients wraps ExecutorCore with the various
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

// timedActions stores the core.ActionSet in ExecutorCore alongside a unique ID
type timedActions struct {
	// id stores a unique ID associated with the cached actions, so that we can use optimistic
	// locking to make sure we're never taking an action that is not the *current* recommendation,
	// because otherwise guaranteeing correctness of core.State is really difficult.
	//
	// id is exclusively used by (*ExecutorCore).updateIfActionsUnchanged().
	id      timedActionsID
	actions core.ActionSet
}

type timedActionsID int64

// fetch the currently cached actions, or recalculate if they've since been invalidated
func (c *ExecutorCore) getActions() timedActions {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.actions == nil {
		id := c.lastActionsID + 1
		c.onNextActions()

		// NOTE: Even though we cache the actions generated using time.Now(), it's *generally* ok.
		now := time.Now()
		c.stateLogger.Debug("Recalculating ActionSet", zap.Time("now", now), zap.Any("state", c.core.Dump()))
		c.actions = &timedActions{id: id, actions: c.core.NextActions(now)}
		c.lastActionsID = id
		c.stateLogger.Debug("New ActionSet", zap.Time("now", now), zap.Any("actions", c.actions.actions))
	}

	return *c.actions
}

func (c *ExecutorCore) update(with func(*core.State)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.updates.Broadcast()
	c.actions = nil
	with(c.core)
}

// updateIfActionsUnchanged is like update, but if the actions have been changed, then the function
// is not called and this returns false.
//
// Otherwise, if the actions are up-to-date, then this is equivalent to c.update(with), and returns true.
func (c *ExecutorCore) updateIfActionsUnchanged(actions timedActions, with func(*core.State)) (updated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if actions.id != c.lastActionsID {
		return false
	}

	c.updates.Broadcast()
	c.actions = nil
	with(c.core)
	return true
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

// UpdateSystemMetrics calls (*core.State).UpdateSystemMetrics() on the inner core.State and runs
// withLock while holding the lock.
func (c ExecutorCoreUpdater) UpdateSystemMetrics(metrics core.SystemMetrics, withLock func()) {
	c.core.update(func(state *core.State) {
		state.UpdateSystemMetrics(metrics)
		withLock()
	})
}

// UpdateLFCMetrics calls (*core.State).UpdateLFCMetrics() on the inner core.State and runs withLock
// while holding the lock.
func (c ExecutorCoreUpdater) UpdateLFCMetrics(metrics core.LFCMetrics, withLock func()) {
	c.core.update(func(state *core.State) {
		state.UpdateLFCMetrics(metrics)
		withLock()
	})
}

// UpdatedVM calls (*core.State).UpdatedVM() on the inner core.State and runs withLock while
// holding the lock.
func (c ExecutorCoreUpdater) UpdatedVM(vm api.VmInfo, withLock func()) {
	c.core.update(func(state *core.State) {
		state.UpdatedVM(vm)
		withLock()
	})
}

// ResetMonitor calls (*core.State).Monitor().Reset() on the inner core.State and runs withLock
// while holding the lock.
func (c ExecutorCoreUpdater) ResetMonitor(withLock func()) {
	c.core.update(func(state *core.State) {
		state.Monitor().Reset()
		withLock()
	})
}

// ScaleRequested calls (*core.State).Monitor().ScaleRequested(...) on the inner core.State and
// runs withLock while holding the lock.
func (c ExecutorCoreUpdater) ScaleRequested(resources api.Allocation, withLock func()) {
	c.core.update(func(state *core.State) {
		state.Monitor().ScaleRequested(time.Now(), resources)
		withLock()
	})
}

// UpscaleRequested calls (*core.State).Monitor().UpscaleRequested(...) on the inner core.State and
// runs withLock while holding the lock.
func (c ExecutorCoreUpdater) UpscaleRequested(resources api.MoreResources, withLock func()) {
	c.core.update(func(state *core.State) {
		state.Monitor().UpscaleRequested(time.Now(), resources)
		withLock()
	})
}

// MonitorActive calls (*core.State).Monitor().Active(...) on the inner core.State and runs withLock
// while holding the lock.
func (c ExecutorCoreUpdater) MonitorActive(active bool, withLock func()) {
	c.core.update(func(state *core.State) {
		state.Monitor().Active(active)
		withLock()
	})
}
