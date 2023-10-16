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

func (c *ExecutorCore) withLock(with func(*core.State)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	with(c.core)
}

func (c *ExecutorCore) update(with func(*core.State)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.updates.Broadcast()
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

// UpdateMetrics calls (*core.State).UpdateMetrics() on the inner core.State and runs withLock while
// holding the lock.
func (c ExecutorCoreUpdater) UpdateMetrics(metrics api.Metrics, withLock func()) {
	c.core.update(func(state *core.State) {
		state.UpdateMetrics(metrics)
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
