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

type PluginInterface interface {
	EmptyID() string
	GetHandle() PluginHandle
}

type PluginHandle interface {
	ID() string
	Request(_ context.Context, _ *zap.Logger, lastPermit *api.Resources, target api.Resources, _ *api.Metrics) (*api.PluginResponse, error)
}

func (c *ExecutorCoreWithClients) DoPluginRequests(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver = c.updates.NewReceiver()
		ifaceLogger *zap.Logger            = logger.Named("client")
	)

	idUnchanged := func(current string) bool {
		if h := c.clients.Plugin.GetHandle(); h != nil {
			return current == h.ID()
		} else {
			return current == c.clients.Plugin.EmptyID()
		}
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
		if last.actions.PluginRequest == nil {
			continue // nothing to do; wait until the state changes.
		}

		var startTime time.Time
		var pluginIface PluginHandle
		action := *last.actions.PluginRequest

		if updated := c.updateIfActionsUnchanged(last, func(state *core.State) {
			logger.Info("Starting plugin request", zap.Any("action", action))
			startTime = time.Now()
			pluginIface = c.clients.Plugin.GetHandle()
			state.Plugin().StartingRequest(startTime, action.Target)
		}); !updated {
			continue // state has changed, retry.
		}

		var resp *api.PluginResponse
		var err error

		if pluginIface != nil {
			resp, err = pluginIface.Request(ctx, ifaceLogger, action.LastPermit, action.Target, action.Metrics)
		} else {
			err = errors.New("No currently enabled plugin handle")
		}
		endTime := time.Now()

		c.update(func(state *core.State) {
			unchanged := idUnchanged(pluginIface.ID())
			logFields := []zap.Field{
				zap.Any("action", action),
				zap.Duration("duration", endTime.Sub(startTime)),
				zap.Bool("unchanged", unchanged),
			}

			if err != nil {
				logger.Error("Plugin request failed", append(logFields, zap.Error(err))...)
				if unchanged {
					state.Plugin().RequestFailed(endTime)
				}
			} else {
				logFields = append(logFields, zap.Any("response", resp))
				logger.Info("Plugin request successful", logFields...)
				if unchanged {
					if err := state.Plugin().RequestSuccessful(endTime, *resp); err != nil {
						logger.Error("Plugin response validation failed", append(logFields, zap.Error(err))...)
					}
				}
			}
		})
	}
}
