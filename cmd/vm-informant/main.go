package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/containerd/cgroups/v3/cgroup2"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/informant"
	"github.com/neondatabase/autoscaling/pkg/task"
	"github.com/neondatabase/autoscaling/pkg/util"
)

func main() {
	buildInfo := util.GetBuildInfo()
	klog.Infof("buildInfo.GitInfo:   %s", buildInfo.GitInfo)
	klog.Infof("buildInfo.GoVersion: %s", buildInfo.GoVersion)

	// Realistically, we want to be able to distinguish between --cgroup="" and absence of the flag,
	// if only to be able to provide better errors on invalid cgroup names.
	//
	// Because cgroup names *could* be any valid path fragment, we include a null byte in the string
	// to distinguish between --cgroup=... and its absence, because null bytes aren't valid in paths.
	noCgroup := "invalid\x00CgroupName"
	var cgroupName string
	var autoRestart bool
	flag.StringVar(&cgroupName, "cgroup", noCgroup, "Sets the cgroup to monitor (optional)")
	flag.BoolVar(&autoRestart, "auto-restart", false, "Automatically cleanup and restart on failure or exit")

	flag.Parse()

	// If we were asked to restart on failure, handle that separately:
	if autoRestart {
		var args []string
		var cleanupHooks []func()
		if cgroupName != noCgroup {
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

		runRestartOnFailure(args, cleanupHooks)
		return
	}

	var stateOpts []informant.NewStateOpts

	if cgroupName != noCgroup {
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

	tm := task.NewRootManager("vm-informant").WithPanicHandler(task.LogPanicAndExit)
	tm.ShutdownOnSigterm(informant.MakeShutdownContext)
	defer func() {
		ctx, cancel := informant.MakeShutdownContext()
		defer cancel()
		if err := tm.Shutdown(ctx); err != nil {
			klog.Fatalf("Error shutting down: %s", err)
		}
	}()

	agents := informant.NewAgentSet(tm)
	state, err := informant.NewState(agents, stateOpts...)
	if err != nil {
		klog.Fatalf("Error starting informant.NewState: %s", err)
	}

	mux := http.NewServeMux()
	util.AddHandler(tm, "", mux, "/register", http.MethodPost, "AgentDesc", state.RegisterAgent)
	// FIXME: /downscale and /upscale should have the AgentID in the request body
	util.AddHandler(tm, "", mux, "/downscale", http.MethodPut, "RawResources", state.TryDownscale)
	util.AddHandler(tm, "", mux, "/upscale", http.MethodPut, "RawResources", state.NotifyUpscale)
	util.AddHandler(tm, "", mux, "/unregister", http.MethodDelete, "AgentDesc", state.UnregisterAgent)

	server := http.Server{Addr: "0.0.0.0:10301", Handler: mux}
	_ = tm.OnShutdown(context.TODO(), task.WrapOnError("Error shutting down HTTP server: %w", server.Shutdown))

	klog.Infof("Starting server at %s", server.Addr)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		klog.Infof("Server ended.")
	} else {
		klog.Fatalf("Server failed: %s", err)
	}
}

// runRestartOnFailure repeatedly calls this binary with the same flags, but with 'auto-restart'
// removed.
//
// We execute ourselves as a subprocess so that it's possible to appropriately cleanup after
// termination by various signals (or an unhandled panic!). This is worthwhile because we *really*
// don't want to leave the cgroup frozen while waiting to restart.
func runRestartOnFailure(args []string, cleanupHooks []func()) {
	selfPath := os.Args[0]

	minWaitDuration := time.Second * 5

	for {
		startTime := time.Now()

		cmd := exec.CommandContext(context.TODO(), selfPath, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()

		var exitMode string

		if err != nil {
			// lint note: the linter's worried about wrapped errors being incorrect with switch, but
			// this is cleaner than the alternative (via errors.As) and it's still correct because
			// exec.Command.Run() explicitly mentions ExitError.
			switch err.(type) { //nolint:errorlint // see above.
			case *exec.ExitError:
				exitMode = "failed"
				klog.Errorf("vm-informant exited with: %v", err)
			default:
				exitMode = "failed to start"
				klog.Errorf("error running vm-informant: %v", err)
			}
		} else {
			exitMode = ""
			klog.Warningf("vm-informant exited without error. This should not happen.")
		}

		for _, h := range cleanupHooks {
			h()
		}

		dur := time.Since(startTime)
		if dur < minWaitDuration {
			klog.Infof("vm-informant %s. respecting minimum wait of %s", exitMode, minWaitDuration)
			time.Sleep(minWaitDuration - dur)
		} else {
			klog.Infof("vm-informant restarting immediately")
		}
	}
}
