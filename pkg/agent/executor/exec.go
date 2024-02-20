package executor

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type PluginInterface interface {
	Request(_ context.Context, _ *zap.Logger, lastPermit *api.Resources, target api.Resources, _ *api.Metrics) (*api.PluginResponse, error)
}

type NeonVMInterface interface {
	Request(_ context.Context, _ *zap.Logger, current, target api.Resources) error
}

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

type SpawnFunc = func(_ context.Context, _ *zap.Logger, requestType string, f func(context.Context, *zap.Logger))

func (c *ExecutorCoreWithClients) DoExecutors(
	ctx context.Context,
	spawn SpawnFunc,
	stateLogger *zap.Logger,
	clientLogger func(requestType string) *zap.Logger,
	onNextActions func(),
) {
	var (
		updates       util.BroadcastReceiver = c.updates.NewReceiver()
		pluginLogger  *zap.Logger            = clientLogger("plugin")
		neonvmLogger  *zap.Logger            = clientLogger("neonvm")
		monitorLogger *zap.Logger            = clientLogger("monitor")
		ongoing       *ongoingRequests       = &ongoingRequests{
			plugin:  false,
			monitor: nil,
			neonvm:  false,
		}
	)

	timer := time.NewTimer(0)
	resetTimer := func(d time.Duration) {
		timer.Stop()
		// Clear the timer's channel if there is something, but don't block if there isn't
		select {
		case <-timer.C:
		default:
		}
		timer.Reset(d)
	}

	for {
		// With the actions, make whatever requests are required:
		var wait *core.ActionWait
		c.withLock(func(s *core.State) {
			now := time.Now()
			stateLogger.Debug("Calculating ActionSet", zap.Time("now", now), zap.Any("state", s.Dump()))
			onNextActions()
			actions := s.NextActions(now)
			stateLogger.Debug("New ActionSet", zap.Time("now", now), zap.Any("actions", actions))

			wait = actions.Wait

			if actions.PluginRequest != nil {
				c.trySpawnPluginRequest(ctx, pluginLogger, s, *actions.PluginRequest, ongoing, spawn)
			}

			if actions.NeonVMRequest != nil {
				c.trySpawnNeonVMRequest(ctx, neonvmLogger, s, *actions.NeonVMRequest, ongoing, spawn)
			}

			if actions.MonitorUpscale != nil {
				c.trySpawnMonitorUpscaleRequest(ctx, monitorLogger, s, *actions.MonitorUpscale, ongoing, spawn)
			} else if actions.MonitorDownscale != nil {
				c.trySpawnMonitorDownscaleRequest(ctx, monitorLogger, s, *actions.MonitorDownscale, ongoing, spawn)
			}
		})

		var waitCh <-chan time.Time
		if wait != nil {
			resetTimer(wait.Duration)
			waitCh = timer.C
		}

		// Wait until the state's changed, the timer's expired, or the Runner is done
		select {
		case <-updates.Wait():
			updates.Awake()
		case <-waitCh:
		case <-ctx.Done():
			return
		}

		// Prioritize context being done over anything else:
		select {
		case <-ctx.Done():
			return
		default:
		}

	}
}

type ongoingRequests struct {
	plugin  bool
	monitor *GenerationNumber
	neonvm  bool
}

func ongoingRequestErr(requestType string) error {
	return fmt.Errorf("core.State asked for %s request, but there's already one ongoing", requestType)
}

func nilHandleErr(requestType string, method string) error {
	return fmt.Errorf(
		"core.State asked for %s request, but %s is nil, so it should be disabled",
		requestType,
		method,
	)
}

func (c *ExecutorCoreWithClients) trySpawnPluginRequest(
	ctx context.Context,
	logger *zap.Logger,
	state *core.State,
	action core.ActionPluginRequest,
	ongoing *ongoingRequests,
	spawn SpawnFunc,
) {
	startTime := time.Now()
	logger.Info("Starting plugin request", zap.Time("now", startTime), zap.Object("action", action))

	state.Plugin().StartingRequest(startTime, action.Target)
	ongoing.plugin = true

	spawn(ctx, logger, "plugin", func(ctx context.Context, logger *zap.Logger) {
		resp, err := c.clients.Plugin.Request(ctx, logger, action.LastPermit, action.Target, action.Metrics)
		endTime := time.Now()

		c.withLock(func(state *core.State) {
			ongoing.plugin = false

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
				if err := state.Plugin().RequestSuccessful(endTime, *resp); err != nil {
					logger.Error("Plugin response validation failed", append(logFields, zap.Error(err))...)
				}
			}
		})
	})
}

func (c *ExecutorCoreWithClients) trySpawnNeonVMRequest(
	ctx context.Context,
	logger *zap.Logger,
	state *core.State,
	action core.ActionNeonVMRequest,
	ongoing *ongoingRequests,
	spawn SpawnFunc,
) {
	if ongoing.neonvm {
		panic(ongoingRequestErr("NeonVM"))
	}

	startTime := time.Now()
	logger.Info("Starting NeonVM request", zap.Time("now", startTime), zap.Object("action", action))

	state.NeonVM().StartingRequest(startTime, action.Target)
	ongoing.neonvm = true

	spawn(ctx, logger, "neonvm", func(ctx context.Context, logger *zap.Logger) {
		err := c.clients.NeonVM.Request(ctx, logger, action.Current, action.Target)
		endTime := time.Now()
		logFields := []zap.Field{zap.Object("action", action), zap.Duration("duration", endTime.Sub(startTime))}

		c.withLock(func(state *core.State) {
			ongoing.neonvm = false
			if err != nil {
				logger.Error("NeonVM request failed", append(logFields, zap.Error(err))...)
				state.NeonVM().RequestFailed(endTime)
			} else /* err == nil */ {
				logger.Info("NeonVM request successful", logFields...)
				state.NeonVM().RequestSuccessful(endTime)
			}
		})
	})
}

func (c *ExecutorCoreWithClients) trySpawnMonitorUpscaleRequest(
	ctx context.Context,
	logger *zap.Logger,
	state *core.State,
	action core.ActionMonitorUpscale,
	ongoing *ongoingRequests,
	spawn SpawnFunc,
) {
	iface := c.clients.Monitor.GetHandle()
	if iface == nil {
		panic(nilHandleErr("vm-monitor upscale", "Monitor.GetHandle()"))
	}

	gen := iface.Generation()

	if oldGen := ongoing.monitor; oldGen != nil && *oldGen == gen {
		panic(ongoingRequestErr("vm-monitor upscale"))
	}

	startTime := time.Now()
	logger.Info("Starting vm-monitor upscale request", zap.Time("now", startTime), zap.Object("action", action))

	state.Monitor().StartingUpscaleRequest(startTime, action.Target)

	spawn(ctx, logger, "vm-monitor upscale", func(ctx context.Context, logger *zap.Logger) {
		err := iface.Upscale(ctx, logger, action.Current, action.Target)
		endTime := time.Now()

		c.withLock(func(state *core.State) {
			unchanged := c.clients.Monitor.CurrentGeneration() == gen
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
					ongoing.monitor = nil
					state.Monitor().UpscaleRequestFailed(endTime)
				} else {
					warnSkipBecauseChanged()
				}
				return
			}

			logger.Info("vm-monitor upscale request successful", logFields...)
			if unchanged {
				ongoing.monitor = nil
				state.Monitor().UpscaleRequestSuccessful(endTime)
			} else {
				warnSkipBecauseChanged()
			}
		})
	})
}

func (c *ExecutorCoreWithClients) trySpawnMonitorDownscaleRequest(
	ctx context.Context,
	logger *zap.Logger,
	state *core.State,
	action core.ActionMonitorDownscale,
	ongoing *ongoingRequests,
	spawn SpawnFunc,
) {
	iface := c.clients.Monitor.GetHandle()
	if iface == nil {
		panic(nilHandleErr("vm-monitor downscale", "Monitor.GetHandle()"))
	}

	gen := iface.Generation()

	if oldGen := ongoing.monitor; oldGen != nil && *oldGen == gen {
		panic(ongoingRequestErr("vm-monitor downscale"))
	}

	startTime := time.Now()
	logger.Info("Starting vm-monitor downscale request", zap.Time("now", startTime), zap.Object("action", action))

	state.Monitor().StartingDownscaleRequest(startTime, action.Target)

	spawn(ctx, logger, "vm-monitor downscale", func(ctx context.Context, logger *zap.Logger) {
		result, err := iface.Downscale(ctx, logger, action.Current, action.Target)
		endTime := time.Now()

		c.withLock(func(state *core.State) {
			unchanged := c.clients.Monitor.CurrentGeneration() == gen
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
					ongoing.monitor = nil
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
					ongoing.monitor = nil
					state.Monitor().DownscaleRequestDenied(endTime)
				} else {
					warnSkipBecauseChanged()
				}
			} else {
				logger.Info("vm-monitor approved downscale", logFields...)
				if unchanged {
					ongoing.monitor = nil
					state.Monitor().DownscaleRequestAllowed(endTime)
				} else {
					warnSkipBecauseChanged()
				}
			}
		})
	})
}
