package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/tychoish/fun/pubsub"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/task"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type MainRunner struct {
	EnvArgs    EnvArgs
	Config     *Config
	KubeClient *kubernetes.Clientset
	VMClient   *vmclient.Clientset
}

func (r MainRunner) Run(tm task.Manager) error {
	ctx := tm.Context()
	immediateContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	podEvents := make(chan podEvent)

	buildInfo := util.GetBuildInfo()
	klog.Infof("buildInfo.GitInfo:   %s", buildInfo.GitInfo)
	klog.Infof("buildInfo.NeonVM:    %s", buildInfo.NeonVM)
	klog.Infof("buildInfo.GoVersion: %s", buildInfo.GoVersion)

	klog.Info("Starting pod watcher")
	podWatchStore, err := startPodWatcher(tm, r.Config, r.KubeClient, r.EnvArgs.K8sNodeName, podEvents)
	if err != nil {
		return fmt.Errorf("Error starting pod watcher: %w", err)
	}
	klog.Info("Pod watcher started")
	_ = tm.OnShutdown(immediateContext, task.Infallible(podWatchStore.Stop))

	klog.Info("Starting VM watcher")
	vmWatchStore, err := startVMWatcher(tm, r.VMClient, r.EnvArgs.K8sNodeName)
	if err != nil {
		return fmt.Errorf("Error starting VM watcher: %w", err)
	}
	_ = tm.OnShutdown(immediateContext, task.Infallible(vmWatchStore.Stop))
	klog.Info("VM watcher started")

	broker := pubsub.NewBroker[watchEvent](tm.Context(), pubsub.BrokerOptions{})
	_ = tm.OnShutdown(immediateContext, func(ctx context.Context) error {
		broker.Stop()
		broker.Wait(ctx)
		return ctx.Err()
	})

	schedulerStore, err := startSchedulerWatcher(tm, RunnerLogger{"Scheduler Watcher: "}, r.KubeClient, broker, r.Config.Scheduler.SchedulerName)
	if err != nil {
		return fmt.Errorf("starting scheduler watch server: %w", err)
	}
	_ = tm.OnShutdown(immediateContext, task.Infallible(schedulerStore.Stop))

	if r.Config.Billing != nil {
		klog.Info("Starting billing metrics collector")
		tm.WithPanicHandler(task.LogPanicAndShutdown(tm, makeShutdownContext)).
			Spawn("billing-metrics-collector", func(subTm task.Manager) {
				RunBillingMetricsCollector(subTm, r.Config.Billing, r.EnvArgs.K8sNodeName, vmWatchStore)
			})
	}

	globalState := r.newAgentState(r.EnvArgs.K8sPodIP, broker, schedulerStore)
	_ = tm.OnShutdown(immediateContext, task.Infallible(func() { globalState.Stop(tm) }))

	klog.Info("Entering main loop")
	for {
		select {
		case <-ctx.Done():
			klog.Infof("Shutting down")
			if err := tm.Shutdown(makeShutdownContext()); err != nil {
				klog.Errorf("Error during shutdown: %s", err)
			}
			return nil
		case event := <-podEvents:
			globalState.handleEvent(tm, event)
		}
	}
}
