package agent

// Implementations of the interfaces used by & defined in pkg/agent/executor
//
// This file is essentially the bridge between 'runner.go' and 'executor/',
// connecting the latter to the actual implementations in the former.

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
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
}

func makePluginInterface(r *Runner) *execPluginInterface {
	return &execPluginInterface{runner: r}
}

// scalingResponseType indicates type of scaling response from the scheduler plugin
type scalingResponseType string

const (
	scalingResponseTypeDenied            = "denied"
	scalingResponseTypeApproved          = "approved"
	scalingResponseTypePartiallyApproved = "partiallyApproved"
	scalingResponseTypeFailed            = "failed"
)

// Request implements executor.PluginInterface
func (iface *execPluginInterface) Request(
	ctx context.Context,
	logger *zap.Logger,
	lastPermit *api.Resources,
	target api.Resources,
	metrics *api.Metrics,
) (*api.PluginResponse, error) {
	if lastPermit != nil {
		iface.runner.recordResourceChange(*lastPermit, target, iface.runner.global.metrics.schedulerRequestedChange)
	}

	resp, err := iface.runner.DoSchedulerRequest(ctx, logger, target, lastPermit, metrics)

	if err == nil && lastPermit != nil {
		iface.runner.recordResourceChange(*lastPermit, resp.Permit, iface.runner.global.metrics.schedulerApprovedChange)
	}

	responseType := func() scalingResponseType {
		if err != nil { // request is failed
			return scalingResponseTypeFailed
		}
		if resp.Permit == target { // request is fully approved by the scheduler
			return scalingResponseTypeApproved
		}
		if lastPermit != nil && *lastPermit != resp.Permit { // request is partially approved by the scheduler
			return scalingResponseTypePartiallyApproved
		}
		return scalingResponseTypeDenied // scheduler denied the request
	}()
	// update VM metrics
	switch responseType {
	case scalingResponseTypePartiallyApproved:
		iface.runner.global.metrics.scalingPartialApprovalsTotal.WithLabelValues(directionValueInc).Inc()
	case scalingResponseTypeDenied:
		iface.runner.global.metrics.scalingFullDeniesTotal.WithLabelValues(directionValueInc).Inc()
	default:
	}

	iface.runner.status.update(iface.runner.global, func(ps podStatus) podStatus {
		// update podStatus metrics on failures
		switch responseType {
		case scalingResponseTypeDenied, scalingResponseTypeFailed:
			ps.failedSchedulerRequestCounter.Inc()
		default:
		}

		return ps
	})

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
func (iface *execNeonVMInterface) Request(
	ctx context.Context,
	logger *zap.Logger,
	current, target api.Resources,
	targetRevision vmv1.RevisionWithTime,
) error {
	iface.runner.recordResourceChange(current, target, iface.runner.global.metrics.neonvmRequestedChange)

	err := iface.runner.doNeonVMRequest(ctx, target, targetRevision)
	if err != nil {
		iface.runner.status.update(iface.runner.global, func(ps podStatus) podStatus {
			ps.failedNeonVMRequestCounter.Inc()
			return ps
		})
		return fmt.Errorf("error making VM patch request: %w", err)
	}

	return nil
}

////////////////////////////////////////////////////
// Monitor-related interface and implementation //
////////////////////////////////////////////////////

type execMonitorInterface struct {
	runner     *Runner
	core       *executor.ExecutorCore
	generation *executor.StoredGenerationNumber
}

func makeMonitorInterface(
	r *Runner,
	core *executor.ExecutorCore,
	generation *executor.StoredGenerationNumber,
) *execMonitorInterface {
	return &execMonitorInterface{runner: r, core: core, generation: generation}
}

func (iface *execMonitorInterface) CurrentGeneration() executor.GenerationNumber {
	return iface.generation.Get()
}

// GetHandle implements executor.MonitorInterface, and MUST only be called while holding the
// executor's lock.
//
// The locking requirement is why we're able to get away with an "unsynchronized" read of the value
// in the runner. For more, see the documentation on Runner.monitor.
func (iface *execMonitorInterface) GetHandle() executor.MonitorHandle {
	monitor := iface.runner.monitor

	if monitor == nil /* || monitor.dispatcher.Exited() */ {
		// NB: we can't check if dispatcher.Exited() because otherwise we might return nil when the
		// executor is told to make a request, because Exited() is not synchronized with changes to
		// the executor state.
		return nil
	}

	return &execMonitorHandle{
		runner:  iface.runner,
		monitor: monitor,
	}
}

type execMonitorHandle struct {
	runner  *Runner
	monitor *monitorInfo
}

func (h *execMonitorHandle) Generation() executor.GenerationNumber {
	return h.monitor.generation
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

	result, err := doMonitorDownscale(ctx, logger, h.monitor.dispatcher, target)

	if err == nil {
		if result.Ok {
			h.runner.recordResourceChange(current, target, h.runner.global.metrics.monitorApprovedChange)
		}
	} else {
		h.runner.status.update(h.runner.global, func(ps podStatus) podStatus {
			ps.failedMonitorRequestCounter.Inc()
			h.runner.global.metrics.scalingFullDeniesTotal.WithLabelValues(directionValueDec).Inc()
			return ps
		})
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

	err := doMonitorUpscale(ctx, logger, h.monitor.dispatcher, target)

	if err == nil {
		h.runner.recordResourceChange(current, target, h.runner.global.metrics.monitorApprovedChange)
	} else {
		h.runner.status.update(h.runner.global, func(ps podStatus) podStatus {
			ps.failedMonitorRequestCounter.Inc()
			return ps
		})
	}

	return err
}
