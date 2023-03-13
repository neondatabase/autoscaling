package agent

import (
	"context"
	"fmt"

	"github.com/tychoish/fun/pubsub"
	"github.com/tychoish/fun/srv"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type MainRunner struct {
	EnvArgs    EnvArgs
	Config     *Config
	KubeClient *kubernetes.Clientset
	VMClient   *vmclient.Clientset
}

func (r MainRunner) Run(ctx context.Context) error {
	podEvents := make(chan podEvent)

	buildInfo := util.GetBuildInfo()
	klog.Infof("buildInfo.GitInfo:   %s", buildInfo.GitInfo)
	klog.Infof("buildInfo.NeonVM:    %s", buildInfo.NeonVM)
	klog.Infof("buildInfo.GoVersion: %s", buildInfo.GoVersion)

	klog.Info("Starting pod watcher")
	podWatchStore, err := startPodWatcher(ctx, r.Config, r.KubeClient, r.EnvArgs.K8sNodeName, podEvents)
	if err != nil {
		return fmt.Errorf("Error starting pod watcher: %w", err)
	}
	klog.Info("Pod watcher started")

	klog.Info("Starting VM watcher")
	vmWatchStore, err := startVMWatcher(ctx, r.VMClient, r.EnvArgs.K8sNodeName)
	if err != nil {
		return fmt.Errorf("Error starting VM watcher: %w", err)
	}
	klog.Info("VM watcher started")

	broker := pubsub.NewBroker[watchEvent](ctx, pubsub.BrokerOptions{})
	if err := srv.GetOrchestrator(ctx).Add(srv.Broker(broker)); err != nil {
		return err
	}

	schedulerStore, err := startSchedulerWatcher(ctx, RunnerLogger{"Scheduler Watcher: "}, r.KubeClient, broker, r.Config.Scheduler.SchedulerName)
	if err != nil {
		return fmt.Errorf("starting scheduler watch server: %w", err)
	}

	if r.Config.Billing.Enabled {
		klog.Info("Starting billing metrics collector")
		// TODO: catch panics here, bubble those into a clean-ish shutdown.
		go RunBillingMetricsCollector(ctx, r.Config.Billing, r.EnvArgs.K8sNodeName, vmWatchStore)
	}

	globalState := r.newAgentState(r.EnvArgs.K8sPodIP, broker, schedulerStore)

	klog.Info("Entering main loop")
	for {
		select {
		case <-ctx.Done():
			podWatchStore.Stop()
			vmWatchStore.Stop()
			schedulerStore.Stop()

			// Remove anything else from podEvents
		loop:
			for {
				select {
				case <-podEvents:
				default:
					break loop
				}
			}

			globalState.Stop()
			return nil
		case event := <-podEvents:
			globalState.handleEvent(ctx, event)
		}
	}
}
