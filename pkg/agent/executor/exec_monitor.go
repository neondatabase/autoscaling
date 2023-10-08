package executor

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type MonitorInterface interface {
	CurrentGeneration() GenerationNumber
	GetHandle() MonitorHandle
}

type MonitorHandle interface {
	Generation() GenerationNumber
	Downscale(_ context.Context, _ *zap.Logger, current, target api.Resources) (*api.DownscaleResult, error)
	Upscale(_ context.Context, _ *zap.Logger, current, target api.Resources) error
}

func (c *ExecutorCoreWithClients) DoMonitorDownscales(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver = c.updates.NewReceiver()
		ifaceLogger *zap.Logger            = logger.Named("client")
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
			logger.Info("Starting vm-monitor downscale request", zap.Any("action", action))
			startTime = time.Now()
			monitorIface = c.clients.Monitor.GetHandle()
			state.Monitor().StartingDownscaleRequest(startTime, action.Target)
		}); !updated {
			continue // state has changed, retry.
		}

		var result *api.DownscaleResult
		var err error

		if monitorIface != nil {
			result, err = monitorIface.Downscale(ctx, ifaceLogger, action.Current, action.Target)
		} else {
			err = errors.New("No currently active vm-monitor connection")
		}
		endTime := time.Now()

		c.update(func(state *core.State) {
			unchanged := generationUnchanged(monitorIface)
			logFields := []zap.Field{
				zap.Any("action", action),
				zap.Duration("duration", endTime.Sub(startTime)),
				zap.Bool("unchanged", unchanged),
			}

			if err != nil {
				logger.Error("vm-monitor downscale request failed", append(logFields, zap.Error(err))...)
				if unchanged {
					state.Monitor().DownscaleRequestFailed(endTime)
				}
				return
			}

			logFields = append(logFields, zap.Any("response", result))

			if !result.Ok {
				logger.Warn("vm-monitor denied downscale", logFields...)
				if unchanged {
					state.Monitor().DownscaleRequestDenied(endTime)
				}
			} else {
				logger.Info("vm-monitor approved downscale", logFields...)
				if unchanged {
					state.Monitor().DownscaleRequestAllowed(endTime)
				}
			}
		})
	}
}

func (c *ExecutorCoreWithClients) DoMonitorUpscales(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver = c.updates.NewReceiver()
		ifaceLogger *zap.Logger            = logger.Named("client")
	)

	// must be called while holding c's lock
	generationUnchanged := func(since MonitorHandle) bool {
		return since.Generation() == c.clients.Plugin.CurrentGeneration()
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
			logger.Info("Starting vm-monitor upscale request", zap.Any("action", action))
			startTime = time.Now()
			monitorIface = c.clients.Monitor.GetHandle()
			state.Monitor().StartingUpscaleRequest(startTime, action.Target)
		}); !updated {
			continue // state has changed, retry.
		}

		var err error
		if monitorIface != nil {
			err = monitorIface.Upscale(ctx, ifaceLogger, action.Current, action.Target)
		} else {
			err = errors.New("No currently active vm-monitor connection")
		}
		endTime := time.Now()

		c.update(func(state *core.State) {
			unchanged := generationUnchanged(monitorIface)
			logFields := []zap.Field{
				zap.Any("action", action),
				zap.Duration("duration", endTime.Sub(startTime)),
				zap.Bool("unchanged", unchanged),
			}

			if err != nil {
				logger.Error("vm-monitor upscale request failed", append(logFields, zap.Error(err))...)
				if unchanged {
					state.Monitor().UpscaleRequestFailed(endTime)
				}
				return
			}

			logger.Info("vm-monitor upscale request successful", logFields...)
			if unchanged {
				state.Monitor().UpscaleRequestSuccessful(endTime)
			}
		})
	}
}
