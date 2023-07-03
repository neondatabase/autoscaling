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

type InformantInterface interface {
	EmptyID() string
	GetHandle() InformantHandle
}

type InformantHandle interface {
	ID() string
	RequestLock() util.ChanMutex
	Downscale(_ context.Context, _ *zap.Logger, current, target api.Resources) (*api.DownscaleResult, error)
	Upscale(_ context.Context, _ *zap.Logger, current, target api.Resources) error
}

func (c *ExecutorCoreWithClients) DoInformantDownscales(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver = c.updates.NewReceiver()
		requestLock util.ChanMutex         = util.NewChanMutex()
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

	// meant to be called while holding c's lock
	idUnchanged := func(current string) bool {
		if h := c.clients.Informant.GetHandle(); h != nil {
			return current == h.ID()
		} else {
			return current == c.clients.Informant.EmptyID()
		}
	}

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
		if last.actions.InformantDownscale == nil {
			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				// NB: don't .Awake(); allow that to be handled at the top of the loop.
				continue
			}
		}

		action := *last.actions.InformantDownscale

		informant := c.clients.Informant.GetHandle()

		if informant != nil {
			requestLock = informant.RequestLock()

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
		}

		var startTime time.Time
		c.update(func(state *core.State) {
			logger.Info("Starting informant downscale request", zap.Any("action", action))
			startTime = time.Now()
			state.Informant().StartingDownscaleRequest(startTime)
		})

		result, err := doSingleInformantDownscaleRequest(ctx, ifaceLogger, informant, action)
		endTime := time.Now()

		c.update(func(state *core.State) {
			unchanged := idUnchanged(informant.ID())
			logFields := []zap.Field{
				zap.Any("action", action),
				zap.Duration("duration", endTime.Sub(startTime)),
				zap.Bool("unchanged", unchanged),
			}

			if err != nil {
				logger.Error("Informant downscale request failed", append(logFields, zap.Error(err))...)
				if unchanged {
					state.Informant().DownscaleRequestFailed(endTime)
				}
				return
			}

			logFields = append(logFields, zap.Any("response", result))

			if !result.Ok {
				logger.Warn("Informant denied downscale", logFields...)
				if unchanged {
					state.Informant().DownscaleRequestDenied(endTime, action.Target)
				}
			} else {
				logger.Info("Informant approved downscale", logFields...)
				if unchanged {
					state.Informant().DownscaleRequestAllowed(endTime, action.Target)
				}
			}
		})
	}
}

func doSingleInformantDownscaleRequest(
	ctx context.Context,
	logger *zap.Logger,
	iface InformantHandle,
	action core.ActionInformantDownscale,
) (*api.DownscaleResult, error) {
	if iface == nil {
		return nil, errors.New("No currently active informant")
	}

	return iface.Downscale(ctx, logger, action.Current, action.Target)
}

func (c *ExecutorCoreWithClients) DoInformantUpscales(ctx context.Context, logger *zap.Logger) {
	var (
		updates     util.BroadcastReceiver = c.updates.NewReceiver()
		requestLock util.ChanMutex         = util.NewChanMutex()
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

	// meant to be called while holding c's lock
	idUnchanged := func(current string) bool {
		if h := c.clients.Informant.GetHandle(); h != nil {
			return current == h.ID()
		} else {
			return current == c.clients.Informant.EmptyID()
		}
	}

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
		if last.actions.InformantUpscale == nil {
			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				// NB: don't .Awake(); allow that to be handled at the top of the loop.
				continue
			}
		}

		action := *last.actions.InformantUpscale

		informant := c.clients.Informant.GetHandle()

		if informant != nil {
			requestLock = informant.RequestLock()

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
		}

		var startTime time.Time
		c.update(func(state *core.State) {
			logger.Info("Starting informant upscale request", zap.Any("action", action))
			startTime = time.Now()
			state.Informant().StartingUpscaleRequest(startTime)
		})

		err := doSingleInformantUpscaleRequest(ctx, ifaceLogger, informant, action)
		endTime := time.Now()

		c.update(func(state *core.State) {
			unchanged := idUnchanged(informant.ID())
			logFields := []zap.Field{
				zap.Any("action", action),
				zap.Duration("duration", endTime.Sub(startTime)),
				zap.Bool("unchanged", unchanged),
			}

			if err != nil {
				logger.Error("Informant upscale request failed", append(logFields, zap.Error(err))...)
				if unchanged {
					state.Informant().UpscaleRequestFailed(endTime)
				}
				return
			}

			logger.Info("Informant upscale request successful", logFields...)
			if unchanged {
				state.Informant().UpscaleRequestSuccessful(endTime, action.Target)
			}
		})
	}
}

func doSingleInformantUpscaleRequest(
	ctx context.Context,
	logger *zap.Logger,
	iface InformantHandle,
	action core.ActionInformantUpscale,
) error {
	if iface == nil {
		return errors.New("No currently active informant")
	}

	return iface.Upscale(ctx, logger, action.Current, action.Target)
}
