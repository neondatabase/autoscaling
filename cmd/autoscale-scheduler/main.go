package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"k8s.io/klog/v2"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"

	"github.com/neondatabase/autoscaling/pkg/plugin"
	"github.com/neondatabase/autoscaling/pkg/task"
)

// all of the juicy bits are defined in pkg/plugin/

func main() {
	tm := task.NewRootTaskManager("autoscale-scheduler")
	tm.ShutdownOnSigterm()
	tm = tm.WithContext(context.WithValue(context.Background(), task.ManagerKey, tm))

	command := app.NewSchedulerCommand(app.WithPlugin(plugin.Name, plugin.NewAutoscaleEnforcerPlugin(tm.Context())))
	err := command.ExecuteContext(tm.Context())
	if err != nil {
		klog.Errorf("%s", err)
	}
	if shutdownErr := tm.Shutdown(shutdownContext()); shutdownErr != nil {
		shutdownErr = fmt.Errorf("Error shutting down: %w", shutdownErr)
		klog.Errorf("%s", shutdownErr)
		if err != nil {
			err = shutdownErr
		}
	}

	if err != nil {
		os.Exit(1)
	}
}

func shutdownContext() context.Context {
	// it's ok to leak here; this only gets called once
	ctx, _ := context.WithTimeout(context.Background(), 2*time.Second) //nolint:govet // see above
	return ctx
}
