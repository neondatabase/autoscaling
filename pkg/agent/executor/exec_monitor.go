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
	EmptyID() string
	GetHandle() MonitorHandle
}

type MonitorHandle interface {
	ID() string
	Downscale(_ context.Context, _ *zap.Logger, current, target api.Resources) (*api.DownscaleResult, error)
	Upscale(_ context.Context, _ *zap.Logger, current, target api.Resources) error
}

func (c *ExecutorCoreWithClients) DoMonitorDownscales(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver = c.updates.NewReceiver()
		ifaceLogger *zap.Logger            = logger.Named("client")
	)

	// meant to be called while holding c's lock
	idUnchanged := func(current string) bool {
		if h := c.clients.Monitor.GetHandle(); h != nil {
			return current == h.ID()
		} else {
			return current == c.clients.Monitor.EmptyID()
		}
	}

	last := c.getActions()
	for {
		// Always receive an update if there is one. This helps with reliability (better guarantees
		// about not missing updates) and means that the switch statements can be simpler.
		select {
		case <-updates.Wait():
			updates.Awake()
			last = c.getActions()
		default:
		}

		// Wait until we're supposed to make a request.
		if last.actions.MonitorDownscale == nil {
			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				// NB: don't .Awake(); allow that to be handled at the top of the loop.
				continue
			}
		}

		action := *last.actions.MonitorDownscale

		monitor := c.clients.Monitor.GetHandle()

		var startTime time.Time
		c.update(func(state *core.State) {
			logger.Info("Starting vm-monitor downscale request", zap.Any("action", action))
			startTime = time.Now()
			state.Monitor().StartingDownscaleRequest(startTime, action.Target)
		})

		result, err := doSingleMonitorDownscaleRequest(ctx, ifaceLogger, monitor, action)
		endTime := time.Now()

		c.update(func(state *core.State) {
			unchanged := idUnchanged(monitor.ID())
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

func doSingleMonitorDownscaleRequest(
	ctx context.Context,
	logger *zap.Logger,
	iface MonitorHandle,
	action core.ActionMonitorDownscale,
) (*api.DownscaleResult, error) {
	if iface == nil {
		return nil, errors.New("No currently active vm-monitor connection")
	}

	return iface.Downscale(ctx, logger, action.Current, action.Target)
}

func (c *ExecutorCoreWithClients) DoMonitorUpscales(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver = c.updates.NewReceiver()
		ifaceLogger *zap.Logger            = logger.Named("client")
	)

	// meant to be called while holding c's lock
	idUnchanged := func(current string) bool {
		if h := c.clients.Monitor.GetHandle(); h != nil {
			return current == h.ID()
		} else {
			return current == c.clients.Monitor.EmptyID()
		}
	}

	last := c.getActions()
	for {
		// Always receive an update if there is one. This helps with reliability (better guarantees
		// about not missing updates) and means that the switch statements can be simpler.
		select {
		case <-updates.Wait():
			updates.Awake()
			last = c.getActions()
		default:
		}

		// Wait until we're supposed to make a request.
		if last.actions.MonitorUpscale == nil {
			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				// NB: don't .Awake(); allow that to be handled at the top of the loop.
				continue
			}
		}

		action := *last.actions.MonitorUpscale

		monitor := c.clients.Monitor.GetHandle()

		var startTime time.Time
		c.update(func(state *core.State) {
			logger.Info("Starting vm-monitor upscale request", zap.Any("action", action))
			startTime = time.Now()
			state.Monitor().StartingUpscaleRequest(startTime, action.Target)
		})

		err := doSingleMonitorUpscaleRequest(ctx, ifaceLogger, monitor, action)
		endTime := time.Now()

		c.update(func(state *core.State) {
			unchanged := idUnchanged(monitor.ID())
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

func doSingleMonitorUpscaleRequest(
	ctx context.Context,
	logger *zap.Logger,
	iface MonitorHandle,
	action core.ActionMonitorUpscale,
) error {
	if iface == nil {
		return errors.New("No currently active vm-monitor connection")
	}

	return iface.Upscale(ctx, logger, action.Current, action.Target)
}
