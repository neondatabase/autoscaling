package executor

import (
	"context"
	"time"

	"go.uber.org/zap"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
)

type NeonVMInterface interface {
	Request(
		_ context.Context,
		_ *zap.Logger,
		current, target api.Resources,
		targetRevision vmv1.RevisionWithTime,
	) error
}

func (c *ExecutorCoreWithClients) DoNeonVMRequests(ctx context.Context, logger *zap.Logger) {
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
		if last.actions.NeonVMRequest == nil {
			continue // nothing to do; wait until the state changes.
		}

		var startTime time.Time
		action := *last.actions.NeonVMRequest

		if updated := c.updateIfActionsUnchanged(last, func(state *core.State) {
			logger.Info("Starting NeonVM request", zap.Object("action", action))
			startTime = time.Now()
			state.NeonVM().StartingRequest(startTime, action.Target)
		}); !updated {
			continue // state has changed, retry.
		}

		endTime := time.Now()
		targetRevision := action.TargetRevision.WithTime(endTime)
		err := c.clients.NeonVM.Request(ctx, ifaceLogger, action.Current, action.Target, targetRevision)

		logFields := []zap.Field{zap.Object("action", action), zap.Duration("duration", endTime.Sub(startTime))}

		c.update(func(state *core.State) {
			if err != nil {
				logger.Error("NeonVM request failed", append(logFields, zap.Error(err))...)
				state.NeonVM().RequestFailed(endTime)
			} else /* err == nil */ {
				logger.Info("NeonVM request successful", logFields...)
				state.NeonVM().RequestSuccessful(endTime)
			}
		})
	}
}
