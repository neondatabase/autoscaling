package executor

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
)

type MonitorInterface interface {
	CurrentGeneration() GenerationNumber
	// GetHandle fetches a stable handle for the current monitor, or nil if there is not one.
	// This method MUST NOT be called unless holding the executor's lock.
	GetHandle() MonitorHandle
}

type MonitorHandle interface {
	Generation() GenerationNumber
	Downscale(_ context.Context, _ *zap.Logger, current, target api.Resources) (*api.DownscaleResult, error)
	Upscale(_ context.Context, _ *zap.Logger, current, target api.Resources) error
}

func (c *ExecutorCoreWithClients) DoMonitorDownscales(ctx context.Context, logger *zap.Logger) {
	var (
		updates     = c.updates.NewReceiver()
		ifaceLogger = logger.Named("client")
	)

	// must be called while holding c's lock
	generationUnchanged := func(since MonitorHandle) bool {
		return since.Generation() == c.clients.Monitor.CurrentGeneration()
	}

	for {
		// Wait until the state's changed, or we're done.
		select {
		case <-ctx.Done():
			return
		case <-updates.Wait():
			updates.Awake()
		}

		last := c.getActions()
		if last.actions.MonitorDownscale == nil {
			continue // nothing to do; wait until the state changes.
		}

		var startTime time.Time
		var monitorIface MonitorHandle
		action := *last.actions.MonitorDownscale

		if updated := c.updateIfActionsUnchanged(last, func(state *core.State) {
			logger.Info("Starting vm-monitor downscale request", zap.Object("action", action))
			startTime = time.Now()
			monitorIface = c.clients.Monitor.GetHandle()
			state.Monitor().StartingDownscaleRequest(startTime, action.Target)

			if monitorIface == nil {
				panic(errors.New(
					"core.State asked for vm-monitor downscale request, but Monitor.GetHandle() is nil, so it should be disabled",
				))
			}
		}); !updated {
			continue // state has changed, retry.
		}

		result, err := monitorIface.Downscale(ctx, ifaceLogger, action.Current, action.Target)
		endTime := time.Now()

		c.update(func(state *core.State) {
			unchanged := generationUnchanged(monitorIface)
			logFields := []zap.Field{
				zap.Object("action", action),
				zap.Duration("duration", endTime.Sub(startTime)),
				zap.Bool("unchanged", unchanged),
			}

			warnSkipBecauseChanged := func() {
				logger.Warn("Skipping state update after vm-monitor downscale request because MonitorHandle changed")
			}

			if err != nil {
				logger.Error("vm-monitor downscale request failed", append(logFields, zap.Error(err))...)
				if unchanged {
					state.Monitor().DownscaleRequestFailed(endTime)
				} else {
					warnSkipBecauseChanged()
				}
				return
			}

			logFields = append(logFields, zap.Any("response", result))

			if !result.Ok {
				logger.Warn("vm-monitor denied downscale", logFields...)
				if unchanged {
					state.Monitor().DownscaleRequestDenied(endTime, action.TargetRevision)
				} else {
					warnSkipBecauseChanged()
				}
			} else {
				logger.Info("vm-monitor approved downscale", logFields...)
				if unchanged {
					state.Monitor().DownscaleRequestAllowed(endTime, action.TargetRevision)
				} else {
					warnSkipBecauseChanged()
				}
			}
		})
	}
}

func (c *ExecutorCoreWithClients) DoMonitorUpscales(ctx context.Context, logger *zap.Logger) {
	var (
		updates     = c.updates.NewReceiver()
		ifaceLogger = logger.Named("client")
	)

	// must be called while holding c's lock
	generationUnchanged := func(since MonitorHandle) bool {
		return since.Generation() == c.clients.Monitor.CurrentGeneration()
	}

	for {
		// Wait until the state's changed, or we're done.
		select {
		case <-ctx.Done():
			return
		case <-updates.Wait():
			updates.Awake()
		}

		last := c.getActions()
		if last.actions.MonitorUpscale == nil {
			continue // nothing to do; wait until the state changes.
		}

		var startTime time.Time
		var monitorIface MonitorHandle
		action := *last.actions.MonitorUpscale

		if updated := c.updateIfActionsUnchanged(last, func(state *core.State) {
			logger.Info("Starting vm-monitor upscale request", zap.Object("action", action))
			startTime = time.Now()
			monitorIface = c.clients.Monitor.GetHandle()
			state.Monitor().StartingUpscaleRequest(startTime, action.Target)

			if monitorIface == nil {
				panic(errors.New(
					"core.State asked for vm-monitor upscale request, but Monitor.GetHandle() is nil, so it should be disabled",
				))
			}
		}); !updated {
			continue // state has changed, retry.
		}

		err := monitorIface.Upscale(ctx, ifaceLogger, action.Current, action.Target)
		endTime := time.Now()

		c.update(func(state *core.State) {
			unchanged := generationUnchanged(monitorIface)
			logFields := []zap.Field{
				zap.Object("action", action),
				zap.Duration("duration", endTime.Sub(startTime)),
				zap.Bool("unchanged", unchanged),
			}

			warnSkipBecauseChanged := func() {
				logger.Warn("Skipping state update after vm-monitor upscale request because MonitorHandle changed")
			}

			if err != nil {
				logger.Error("vm-monitor upscale request failed", append(logFields, zap.Error(err))...)
				if unchanged {
					state.Monitor().UpscaleRequestFailed(endTime)
				} else {
					warnSkipBecauseChanged()
				}
				return
			}

			logger.Info("vm-monitor upscale request successful", logFields...)
			if unchanged {
				state.Monitor().UpscaleRequestSuccessful(endTime)
			} else {
				warnSkipBecauseChanged()
			}
		})
	}
}
