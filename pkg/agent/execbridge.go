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
)

var (
	_ executor.PluginInterface  = (*execPluginInterface)(nil)
	_ executor.NeonVMInterface  = (*execNeonVMInterface)(nil)
	_ executor.MonitorInterface = (*execMonitorInterface)(nil)
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
// Monitor-related interface and implementation //
////////////////////////////////////////////////////

type execMonitorInterface struct {
	runner *Runner
	core   *executor.ExecutorCore
}

func makeMonitorInterface(r *Runner, core *executor.ExecutorCore) *execMonitorInterface {
	return &execMonitorInterface{runner: r, core: core}
}

// EmptyID implements executor.MonitorInterface
func (iface *execMonitorInterface) EmptyID() string {
	return "<none>"
}

func (iface *execMonitorInterface) GetHandle() executor.MonitorHandle {
	dispatcher := iface.runner.monitor.Load()

	if dispatcher == nil || dispatcher.Exited() {
		return nil
	}

	return &execMonitorHandle{
		runner:     iface.runner,
		dispatcher: dispatcher,
	}
}

type execMonitorHandle struct {
	runner     *Runner
	dispatcher *Dispatcher
}

func (h *execMonitorHandle) ID() string {
	panic("todo")
}

func (h *execMonitorHandle) Downscale(
	ctx context.Context,
	logger *zap.Logger,
	current api.Resources,
	target api.Resources,
) (*api.DownscaleResult, error) {
	// Check validity of the message we're sending
	if target.HasFieldGreaterThan(current) {
		innerMsg := fmt.Errorf("%+v has field greater than %+v", target, current)
		panic(fmt.Errorf("(*execMonitorHandle).Downscale() called with target greater than current: %w", innerMsg))
	}

	h.runner.recordResourceChange(current, target, h.runner.global.metrics.monitorRequestedChange)

	result, err := doMonitorDownscale(ctx, logger, h.dispatcher, target)

	if err != nil && result.Ok {
		h.runner.recordResourceChange(current, target, h.runner.global.metrics.monitorApprovedChange)
	}

	return result, err
}

func (h *execMonitorHandle) Upscale(ctx context.Context, logger *zap.Logger, current, target api.Resources) error {
	// Check validity of the message we're sending
	if target.HasFieldLessThan(current) {
		innerMsg := fmt.Errorf("%+v has field less than %+v", target, current)
		panic(fmt.Errorf("(*execMonitorHandle).Upscale() called with target less than current: %w", innerMsg))
	}

	h.runner.recordResourceChange(current, target, h.runner.global.metrics.monitorRequestedChange)

	err := doMonitorUpscale(ctx, logger, h.dispatcher, target)

	if err != nil {
		h.runner.recordResourceChange(current, target, h.runner.global.metrics.monitorApprovedChange)
	}

	return err
}
