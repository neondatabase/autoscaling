package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/tychoish/fun/srv"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/informant"
	"github.com/neondatabase/autoscaling/pkg/util"
)

const minSubProcessRestartInterval = 5 * time.Second

func main() {
	logger := zap.Must(zap.NewProduction()).Named("vm-informant")
	defer logger.Sync() //nolint:errcheck // what are we gonna do, log something about it?

	logger.Info("", zap.Any("buildInfo", util.GetBuildInfo()))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	ctx = srv.SetShutdownSignal(ctx) // allows workers to cause a shutdown
	ctx = srv.WithOrchestrator(ctx)  // creates and starts an orchestrator
	ctx = srv.SetBaseContext(ctx)    // sets a context for starting async work in request scopes

	orca := srv.GetOrchestrator(ctx)

	defer func() {
		if err := orca.Service().Wait(); err != nil {
			logger.Panic("Failed to shut down service", zap.Error(err))
		}
	}()

	// Below, we want to be able to distinguish between absence of flags and presence of empty
	// flags. The only way we can reliably do this is by setting defaults to a sentinel value that
	// isn't possible to create otherwise. In this case, it's a string containing a null byte, which
	// cannot be provided (due to C's null-terminated strings).
	invalidArgValue := "\x00"

	var cgroupName string
	var autoRestart bool
	var pgConnStr string
	flag.StringVar(&cgroupName, "cgroup", invalidArgValue, "Sets the cgroup to monitor (optional)")
	flag.BoolVar(&autoRestart, "auto-restart", false, "Automatically cleanup and restart on failure or exit")
	flag.StringVar(&pgConnStr, "pgconnstr", invalidArgValue, "Sets the postgres connection string to enable file cache (optional)")

	flag.Parse()

	// If we were asked to restart on failure, handle that separately:
	if autoRestart {
		logger = logger.Named("parent")

		var args []string
		var cleanupHooks []func()

		if pgConnStr != invalidArgValue {
			args = append(args, "-pgconnstr", pgConnStr)
		}
		if cgroupName != invalidArgValue {
			args = append(args, "-cgroup", cgroupName)
			cleanupHooks = append(cleanupHooks, func() {
				logger.Info("cleanup hook: making sure cgroup is thawed", zap.String("cgroup", cgroupName))
				manager, err := cgroup2.Load(fmt.Sprint("/", cgroupName))
				if err != nil {
					logger.Error("Error making cgroup handler", zap.Error(err))
					return
				}
				if err := manager.Thaw(); err != nil {
					logger.Error("Error thawing cgroup", zap.Error(err))
				}
			})
		}

		runRestartOnFailure(ctx, logger, args, cleanupHooks)
		closer := srv.GetShutdownSignal(ctx)
		// this cancels the process' underlying context
		closer()
		// this drops to the defer that waits for all services to shutdown
		// will run now.
		return
	}

	var stateOpts []informant.NewStateOpts

	if cgroupName != invalidArgValue {
		logger := logger.With(zap.String("cgroup", cgroupName))

		cgroupConfig := informant.DefaultCgroupConfig
		logger.Info("Selected cgroup, starting handler", zap.Any("config", cgroupConfig))
		cgroup, err := informant.NewCgroupManager(logger.Named("cgroup").Named("manager"), cgroupName)
		if err != nil {
			logger.Fatal("Error starting cgroup handler", zap.Error(err))
		}

		stateOpts = append(stateOpts, informant.WithCgroup(cgroup, cgroupConfig))
	} else {
		logger.Info("No cgroup selected")
	}

	if pgConnStr != invalidArgValue {
		logger := logger.With(zap.String("fileCacheConnstr", pgConnStr))

		fileCacheConfig := informant.DefaultFileCacheConfig
		logger.Info("Selected postgres file cache", zap.Any("config", fileCacheConfig))
		stateOpts = append(stateOpts, informant.WithPostgresFileCache(pgConnStr, fileCacheConfig))
	} else {
		logger.Info("No postgres file cache selected")
	}

	agents := informant.NewAgentSet(logger)
	state, err := informant.NewState(logger, agents, informant.DefaultStateConfig, stateOpts...)
	if err != nil {
		logger.Fatal("Error starting informant.NewState", zap.Error(err))
	}

	mux := http.NewServeMux()
	hl := logger.Named("handle")
	util.AddHandler(hl, mux, "/register", http.MethodPost, "AgentDesc", state.RegisterAgent)
	util.AddHandler(hl, mux, "/health-check", http.MethodPut, "AgentIdentification", state.HealthCheck)
	util.AddHandler(hl, mux, "/downscale", http.MethodPut, "AgentResourceMessage", state.TryDownscale)
	util.AddHandler(hl, mux, "/upscale", http.MethodPut, "AgentResourceMessage", state.NotifyUpscale)
	util.AddHandler(hl, mux, "/unregister", http.MethodDelete, "AgentDesc", state.UnregisterAgent)

	addr := "0.0.0.0:10301"
	hl.Info("Starting server", zap.String("addr", addr))

	// we create an http service and add it to the orchestrator,
	// which will start it and manage its lifecycle.
	apisrv := srv.HTTP("vm-informant-api", 5*time.Second, &http.Server{Addr: addr, Handler: mux})
	apisrv.ErrorHandler.Set(func(err error) {
		if err != nil {
			cancel()
		}
	})
	if err := orca.Add(apisrv); err != nil {
		logger.Fatal("Failed to add API server", zap.Error(err))
	}

	// we drop to the defers now, which will block until the signal
	// handler is called.
}

// runRestartOnFailure repeatedly calls this binary with the same flags, but with 'auto-restart'
// removed.
//
// We execute ourselves as a subprocess so that it's possible to appropriately cleanup after
// termination by various signals (or an unhandled panic!). This is worthwhile because we *really*
// don't want to leave the cgroup frozen while waiting to restart.
func runRestartOnFailure(ctx context.Context, logger *zap.Logger, args []string, cleanupHooks []func()) {
	selfPath := os.Args[0]
	timer := time.NewTimer(0)
	defer timer.Stop()
	var alreadyReceivedFromTimer bool

	for {
		startTime := time.Now()
		sig := make(chan struct{})

		func() {
			pctx, pcancel := context.WithCancel(context.Background())
			defer pcancel()

			cmd := exec.Command(selfPath, args...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			logger.Info("Starting child vm-informant", zap.Any("args", args))
			err := cmd.Start()
			if err == nil {
				go func() {
					defer close(sig)

					select {
					case <-pctx.Done():
						return
					case <-ctx.Done():
						if pctx.Err() != nil {
							// the process has already returned
							// and we don't need to signal it
							return
						}
						if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
							logger.Warn("Could not signal child vm-informant process", zap.Error(err))
						}
					}
				}()

				// this is blocking, but we should
				// have killed the process in the
				// wait goroutine, or the process would
				// return normally.
				err = cmd.Wait()
				// stop the goroutine above, as the
				// process has already returned.
				pcancel()
			}

			if err != nil {
				logger.Error("Child vm-informant exited with error", zap.Error(err))
			} else {
				logger.Warn("Child vm-informant exited without error. This should not happen")
			}

			for _, h := range cleanupHooks {
				h()
			}
		}()

		select {
		case <-ctx.Done():
			logger.Info("Received shutdown signal")
			return
		case <-sig:
			dur := time.Since(startTime)
			if dur < minSubProcessRestartInterval {
				// drain the timer before resetting it, required by Timer.Reset::
				if !timer.Stop() && !alreadyReceivedFromTimer {
					<-timer.C
				}
				timer.Reset(minSubProcessRestartInterval - dur)
				alreadyReceivedFromTimer = false

				logger.Info(
					"Child vm-informant failed, respecting minimum delay before restart",
					zap.Duration("delay", minSubProcessRestartInterval),
				)
				select {
				case <-ctx.Done():
					logger.Info("Received shutdown signal while delaying before restart", zap.Duration("delay", minSubProcessRestartInterval))
					return
				case <-timer.C:
					alreadyReceivedFromTimer = true
					continue
				}
			}

			logger.Info("Restarting child vm-informant immediately")
			continue
		}
	}
}
