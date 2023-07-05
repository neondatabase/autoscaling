package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/tychoish/fun/srv"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/informant"
	"github.com/neondatabase/autoscaling/pkg/util"
)

const minSubProcessRestartInterval = 5 * time.Second

func main() {
	cfg := zap.NewProductionConfig()
	cfg.Level.SetLevel(zap.DebugLevel)
	logger := zap.Must(cfg.Build()).Named("vm-informant")
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

	var autoRestart bool
	flag.BoolVar(&autoRestart, "auto-restart", false, "Automatically cleanup and restart on failure or exit")

	flag.Parse()

	// If we were asked to restart on failure, handle that separately:
	if autoRestart {
		logger = logger.Named("parent")

		var args []string
		var cleanupHooks []func()
		runRestartOnFailure(ctx, logger, args, cleanupHooks)
		closer := srv.GetShutdownSignal(ctx)
		// this cancels the process' underlying context
		closer()
		// this drops to the defer that waits for all services to shutdown
		// will run now.
		return
	}

	state, err := informant.NewState(logger)
	if err != nil {
		logger.Fatal("Error creating informant state", zap.Error(err))
	}
	logger.Info("Created informant state.", zap.Any("state", state))

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
	if err := orca.Add(srv.HTTP("vm-informant-api", 5*time.Second, &http.Server{Addr: addr, Handler: mux})); err != nil {
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
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(minSubProcessRestartInterval - dur)

				logger.Info(
					"Child vm-informant failed, respecting minimum delay before restart",
					zap.Duration("delay", minSubProcessRestartInterval),
				)
				select {
				case <-ctx.Done():
					logger.Info("Received shutdown signal while delaying before restart", zap.Duration("delay", minSubProcessRestartInterval))
					return
				case <-timer.C:
					continue
				}
			}

			logger.Info("Restarting child vm-informant immediately")
			continue
		}
	}
}
