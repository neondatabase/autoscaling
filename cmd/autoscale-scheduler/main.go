package main

import (
	"context"
	"time"

	"k8s.io/klog/v2"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"

	"github.com/neondatabase/autoscaling/pkg/plugin"
	"github.com/neondatabase/autoscaling/pkg/task"
)

// all of the juicy bits are defined in pkg/plugin/

func main() {
	tm := task.NewRootTaskManager("autoscale-scheduler")
	shutdownErrorHandler := task.LogFatalError("Error during shutdown: %w")

	tm = tm.WithShutdownErrorHandler(shutdownErrorHandler)
	tm = tm.WithContext(context.WithValue(context.Background(), task.ManagerKey, tm))
	tm = tm.WithPanicHandler(task.LogPanicAndShutdown(tm, makeShutdownContext))
	tm.ShutdownOnSigterm(makeShutdownContext)
	defer func() {
		ctx, cancel := makeShutdownContext()
		defer cancel()
		if err := tm.Shutdown(ctx); err != nil {
			_ = shutdownErrorHandler(err)
		}
	}()

	command := app.NewSchedulerCommand(app.WithPlugin(plugin.Name, plugin.NewAutoscaleEnforcerPlugin(tm.Context())))
	err := command.ExecuteContext(tm.Context())
	if err != nil {
		klog.Fatalf("%s", err)
	}
}

func makeShutdownContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Second)
}
