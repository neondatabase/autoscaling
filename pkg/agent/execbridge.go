package agent

// Implementations of the interfaces used by & defined in pkg/agent/executor
//
// This file is essentially the bridge between 'runner.go' and 'executor/'

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/agent/executor"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

var (
	_ executor.PluginInterface    = (*execPluginInterface)(nil)
	_ executor.NeonVMInterface    = (*execNeonVMInterface)(nil)
	_ executor.InformantInterface = (*execInformantInterface)(nil)
)

/////////////////////////////////////////////////////////////
// Scheduler Plugin -related interfaces and implementation //
/////////////////////////////////////////////////////////////

type execPluginInterface struct {
	runner *Runner
	core   *executor.ExecutorCore
}

func makePluginInterface(r *Runner, core *executor.ExecutorCore) *execPluginInterface {
	return &execPluginInterface{runner: r, core: core}
}

// EmptyID implements executor.PluginInterface
func (iface *execPluginInterface) EmptyID() string {
	return "<none>"
}

// RequestLock implements executor.PluginInterface
func (iface *execPluginInterface) RequestLock() util.ChanMutex {
	return iface.runner.requestLock
}

// GetHandle implements executor.PluginInterface
func (iface *execPluginInterface) GetHandle() executor.PluginHandle {
	scheduler := iface.runner.scheduler.Load()

	if scheduler == nil {
		return nil
	}

	return &execPluginHandle{
		runner:    iface.runner,
		scheduler: scheduler,
	}
}

type execPluginHandle struct {
	runner    *Runner
	scheduler *Scheduler
}

// ID implements executor.PluginHandle
func (h *execPluginHandle) ID() string {
	return string(h.scheduler.info.UID)
}

// Request implements executor.PluginHandle
func (h *execPluginHandle) Request(
	ctx context.Context,
	logger *zap.Logger,
	lastPermit *api.Resources,
	target api.Resources,
	metrics *api.Metrics,
) (*api.PluginResponse, error) {
	if lastPermit != nil {
		h.runner.recordResourceChange(*lastPermit, target, h.runner.global.metrics.schedulerRequestedChange)
	}

	resp, err := h.scheduler.DoRequest(ctx, logger, target, metrics)

	if err != nil && lastPermit != nil {
		h.runner.recordResourceChange(*lastPermit, target, h.runner.global.metrics.schedulerApprovedChange)
	}

	return resp, err
}

/////////////////////////////////////////////////
// NeonVM-related interface and implementation //
/////////////////////////////////////////////////

type execNeonVMInterface struct {
	runner *Runner
}

func makeNeonVMInterface(r *Runner) *execNeonVMInterface {
	return &execNeonVMInterface{runner: r}
}

// RequestLock implements executor.NeonVMInterface
func (iface *execNeonVMInterface) RequestLock() util.ChanMutex {
	return iface.runner.requestLock
}

// Request implements executor.NeonVMInterface
func (iface *execNeonVMInterface) Request(ctx context.Context, logger *zap.Logger, current, target api.Resources) error {
	iface.runner.recordResourceChange(current, target, iface.runner.global.metrics.neonvmRequestedChange)

	err := iface.runner.doNeonVMRequest(ctx, target)
	if err != nil {
		return fmt.Errorf("Error making VM patch request: %w", err)
	}

	return nil
}

////////////////////////////////////////////////////
// Informant-related interface and implementation //
////////////////////////////////////////////////////

type execInformantInterface struct {
	runner *Runner
	core   *executor.ExecutorCore
}

func makeInformantInterface(r *Runner, core *executor.ExecutorCore) *execInformantInterface {
	return &execInformantInterface{runner: r, core: core}
}

// EmptyID implements executor.InformantInterface
func (iface *execInformantInterface) EmptyID() string {
	return "<none>"
}

func (iface *execInformantInterface) GetHandle() executor.InformantHandle {
	server := iface.runner.server.Load()

	if server == nil || server.ExitStatus() != nil {
		return nil
	}

	return &execInformantHandle{server: server}
}

type execInformantHandle struct {
	server *InformantServer
}

func (h *execInformantHandle) ID() string {
	return h.server.desc.AgentID.String()
}

func (h *execInformantHandle) RequestLock() util.ChanMutex {
	return h.server.requestLock
}

func (h *execInformantHandle) Downscale(
	ctx context.Context,
	logger *zap.Logger,
	current api.Resources,
	target api.Resources,
) (*api.DownscaleResult, error) {
	// Check validity of the message we're sending
	if target.HasFieldGreaterThan(current) {
		innerMsg := fmt.Errorf("%+v has field greater than %+v", target, current)
		panic(fmt.Errorf("(*execInformantHandle).Downscale() called with target greater than current: %w", innerMsg))
	}

	h.server.runner.recordResourceChange(current, target, h.server.runner.global.metrics.informantRequestedChange)

	result, err := h.server.Downscale(ctx, logger, target)

	if err != nil && result.Ok {
		h.server.runner.recordResourceChange(current, target, h.server.runner.global.metrics.informantApprovedChange)
	}

	return result, err
}

func (h *execInformantHandle) Upscale(ctx context.Context, logger *zap.Logger, current, target api.Resources) error {
	// Check validity of the message we're sending
	if target.HasFieldLessThan(current) {
		innerMsg := fmt.Errorf("%+v has field less than %+v", target, current)
		panic(fmt.Errorf("(*execInformantHandle).Upscale() called with target less than current: %w", innerMsg))
	}

	h.server.runner.recordResourceChange(current, target, h.server.runner.global.metrics.informantRequestedChange)

	err := h.server.Upscale(ctx, logger, target)

	if err != nil {
		h.server.runner.recordResourceChange(current, target, h.server.runner.global.metrics.informantApprovedChange)
	}

	return err
}
