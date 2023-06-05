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

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/informant"
	"github.com/neondatabase/autoscaling/pkg/util"
)

const minSubProcessRestartInterval = 5 * time.Second

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	ctx = srv.SetShutdownSignal(ctx) // allows workers to cause a shutdown
	ctx = srv.WithOrchestrator(ctx)  // creates and starts an orchestrator
	ctx = srv.SetBaseContext(ctx)    // sets a context for starting async work in request scopes

	orca := srv.GetOrchestrator(ctx)

	defer func() {
		if err := orca.Service().Wait(); err != nil {
			klog.Fatal("failed to shut down service", err)
		}
	}()

	buildInfo := util.GetBuildInfo()
	klog.Infof("buildInfo.GitInfo:   %s", buildInfo.GitInfo)
	klog.Infof("buildInfo.GoVersion: %s", buildInfo.GoVersion)

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
		var args []string
		var cleanupHooks []func()

		if pgConnStr != invalidArgValue {
			args = append(args, "-pgconnstr", pgConnStr)
		}
		if cgroupName != invalidArgValue {
			args = append(args, "-cgroup", cgroupName)
			cleanupHooks = append(cleanupHooks, func() {
				klog.Infof("vm-informant cleanup hook: making sure cgroup %q is thawed", cgroupName)
				manager, err := cgroup2.Load(fmt.Sprint("/", cgroupName))
				if err != nil {
					klog.Warningf("Error making cgroup handler: %s", err)
					return
				}
				if err := manager.Thaw(); err != nil {
					klog.Warningf("Error thawing cgroup: %s", err)
				}
			})
		}

		runRestartOnFailure(ctx, args, cleanupHooks)
		closer := srv.GetShutdownSignal(ctx)
		// this cancels the process' underlying context
		closer()
		// this drops to the defer that waits for all services to shutdown
		// will run now.
		return
	}

	var stateOpts []informant.NewStateOpts

	if cgroupName != invalidArgValue {
		cgroupConfig := informant.DefaultCgroupConfig
		klog.Infof("Selected cgroup %q, starting handler with config %+v", cgroupName, cgroupConfig)
		cgroup, err := informant.NewCgroupManager(cgroupName)
		if err != nil {
			klog.Fatalf("Error starting cgroup handler for cgroup name %q: %s", cgroupName, err)
		}

		stateOpts = append(stateOpts, informant.WithCgroup(cgroup, cgroupConfig))
	} else {
		klog.Infof("No cgroup selected")
	}

	if pgConnStr != invalidArgValue {
		fileCacheConfig := informant.DefaultFileCacheConfig
		klog.Infof("Selected postgres file cache, connstr = %q and config = %+v", pgConnStr, fileCacheConfig)
		stateOpts = append(stateOpts, informant.WithPostgresFileCache(pgConnStr, fileCacheConfig))
	} else {
		klog.Infof("No postgres file cache selected")
	}

	agents := informant.NewAgentSet()
	state, err := informant.NewState(agents, informant.DefaultStateConfig, stateOpts...)
	if err != nil {
		klog.Fatalf("Error starting informant.NewState: %s", err)
	}

	mux := http.NewServeMux()
	util.AddHandler("", mux, "/register", http.MethodPost, "AgentDesc", state.RegisterAgent)
	util.AddHandler("", mux, "/health-check", http.MethodPut, "AgentIdentification", state.HealthCheck)
	// FIXME: /downscale and /upscale should have the AgentID in the request body
	util.AddHandler("", mux, "/downscale", http.MethodPut, "SignedRawResources", state.TryDownscale)
	util.AddHandler("", mux, "/upscale", http.MethodPut, "SignedRawResources", state.NotifyUpscale)
	util.AddHandler("", mux, "/unregister", http.MethodDelete, "AgentDesc", state.UnregisterAgent)

	addr := "0.0.0.0:10301"
	klog.Infof("Starting server at %s", addr)

	// we create an http service and add it to the orchestrator,
	// which will start it and manage its lifecycle.
	if err := orca.Add(srv.HTTP("vm-informant-api", 5*time.Second, &http.Server{Addr: addr, Handler: mux})); err != nil {
		klog.Fatalf("failed to add informant api server: %s", err)
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
func runRestartOnFailure(ctx context.Context, args []string, cleanupHooks []func()) {
	selfPath := os.Args[0]
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		startTime := time.Now()
		sig := make(chan struct{})
		var exitMode string

		func() {
			pctx, pcancel := context.WithCancel(context.Background())
			defer pcancel()

			cmd := exec.Command(selfPath, args...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			klog.Infof("Running vm-informant with args %+v", args)
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
							klog.Warningf("could not signal vm-informant process: %v", err)
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
				klog.Errorf("vm-informant exited with error: %v", err)
			} else {
				klog.Warningf("vm-informant exited without error. This should not happen.")
			}

			for _, h := range cleanupHooks {
				h()
			}
		}()

		select {
		case <-ctx.Done():
			klog.Infof("vm-informant: received signal")
			return
		case <-sig:
			dur := time.Since(startTime)
			if dur < minSubProcessRestartInterval {
				// drain the timer before resetting it, required by Timer.Reset::
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(minSubProcessRestartInterval - dur)

				klog.Infof("vm-informant %s. respecting minimum wait of %s", exitMode, minSubProcessRestartInterval)
				select {
				case <-ctx.Done():
					klog.Infof("vm-informant restart loop: received termination signal")
					return
				case <-timer.C:
					continue
				}
			}

			klog.Infof("vm-informant restarting immediately")
			continue
		}
	}
}
