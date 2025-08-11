package executor

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
)

type PluginInterface interface {
	Request(_ context.Context, _ *zap.Logger, lastPermit *api.Resources, target api.Resources, _ *api.Metrics) (*api.PluginResponse, error)
}

func (c *ExecutorCoreWithClients) DoPluginRequests(ctx context.Context, logger *zap.Logger) {
	var (
		updates     = c.updates.NewReceiver()
		ifaceLogger = logger.Named("client")
	)

	for {
		// Wait until the state's changed, or we're done.
		select {
		case <-ctx.Done():
			return
		case <-updates.Wait():
			updates.Awake()
		}

		last := c.getActions()
		if last.actions.PluginRequest == nil {
			continue // nothing to do; wait until the state changes.
		}

		var startTime time.Time
		action := *last.actions.PluginRequest

		if updated := c.updateIfActionsUnchanged(last, func(state *core.State) {
			logger.Info("Starting plugin request", zap.Object("action", action))
			startTime = time.Now()
			state.Plugin().StartingRequest(startTime, action.Target)
		}); !updated {
			continue // state has changed, retry.
		}

		resp, err := c.clients.Plugin.Request(ctx, ifaceLogger, action.LastPermit, action.Target, action.Metrics)
		endTime := time.Now()

		c.update(func(state *core.State) {
			logFields := []zap.Field{
				zap.Object("action", action),
				zap.Duration("duration", endTime.Sub(startTime)),
			}

			if err != nil {
				logger.Error("Plugin request failed", append(logFields, zap.Error(err))...)
				state.Plugin().RequestFailed(endTime)
			} else {
				logFields = append(logFields, zap.Any("response", resp))
				logger.Info("Plugin request successful", logFields...)
				if err := state.Plugin().RequestSuccessful(endTime, action.TargetRevision, *resp); err != nil {
					logger.Error("Plugin response validation failed", append(logFields, zap.Error(err))...)
				}
			}
		})
	}
}
