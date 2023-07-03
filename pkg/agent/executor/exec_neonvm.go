package executor

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type NeonVMInterface interface {
	RequestLock() util.ChanMutex
	Request(_ context.Context, _ *zap.Logger, current, target api.Resources) error
}

func (c *ExecutorCoreWithClients) DoNeonVMRequests(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver = c.updates.NewReceiver()
		requestLock util.ChanMutex         = c.clients.NeonVM.RequestLock()
		ifaceLogger *zap.Logger            = logger.Named("client")
	)

	holdingRequestLock := false
	releaseRequestLockIfHolding := func() {
		if holdingRequestLock {
			requestLock.Unlock()
			holdingRequestLock = false
		}
	}
	defer releaseRequestLockIfHolding()

	last := c.getActions()
	for {
		releaseRequestLockIfHolding()

		// Always receive an update if there is one. This helps with reliability (better guarantees
		// about not missing updates) and means that the switch statements can be simpler.
		select {
		case <-updates.Wait():
			updates.Awake()
			last = c.getActions()
		default:
		}

		// Wait until we're supposed to make a request.
		if last.actions.NeonVMRequest == nil {
			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				// NB: don't .Awake(); allow that to be handled at the top of the loop.
				continue
			}
		}

		action := *last.actions.NeonVMRequest

		// Try to acquire the request lock, but if something happens while we're waiting, we'll
		// abort & retry on the next loop iteration (or maybe not, if last.actions changed).
		select {
		case <-ctx.Done():
			return
		case <-updates.Wait():
			// NB: don't .Awake(); allow that to be handled at the top of the loop.
			continue
		case <-requestLock.WaitLock():
			holdingRequestLock = true
		}

		var startTime time.Time
		c.update(func(state *core.State) {
			logger.Info("Starting NeonVM request", zap.Any("action", action))
			startTime = time.Now()
			state.NeonVM().StartingRequest(startTime, action.Target)
		})

		err := c.clients.NeonVM.Request(ctx, ifaceLogger, action.Current, action.Target)
		endTime := time.Now()
		logFields := []zap.Field{zap.Any("action", action), zap.Duration("duration", endTime.Sub(startTime))}

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
