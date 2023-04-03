package agent

import (
	"context"
	"fmt"

	"github.com/tychoish/fun/pubsub"
	"github.com/tychoish/fun/srv"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type MainRunner struct {
	EnvArgs    EnvArgs
	Config     *Config
	KubeClient *kubernetes.Clientset
	VMClient   *vmclient.Clientset
}

func (r MainRunner) Run(ctx context.Context) error {
	vmEvents := make(chan vmEvent)

	buildInfo := util.GetBuildInfo()
	klog.Infof("buildInfo.GitInfo:   %s", buildInfo.GitInfo)
	klog.Infof("buildInfo.GoVersion: %s", buildInfo.GoVersion)

	klog.Info("Starting VM watcher")
	vmWatchStore, err := startVMWatcher(ctx, r.Config, r.VMClient, r.EnvArgs.K8sNodeName, vmEvents)
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

	if r.Config.Billing != nil {
		klog.Info("Starting billing metrics collector")
		// TODO: catch panics here, bubble those into a clean-ish shutdown.
		storeForNode := util.NewIndexedWatchStore(vmWatchStore, NewVMNodeIndex(r.EnvArgs.K8sNodeName))
		go RunBillingMetricsCollector(ctx, r.Config.Billing, storeForNode)
	}

	globalState, err := r.newAgentState(ctx, r.EnvArgs.K8sPodIP, broker, schedulerStore)
	if err != nil {
		return fmt.Errorf("Error creating global state: %w", err)
	}

	if r.Config.DumpState != nil {
		klog.Info("Starting 'dump state' server")
		if err := globalState.StartDumpStateServer(ctx, r.Config.DumpState); err != nil {
			return fmt.Errorf("Error starting dump state server: %w", err)
		}
	}

	klog.Info("Entering main loop")
	for {
		select {
		case <-ctx.Done():
			vmWatchStore.Stop()
			schedulerStore.Stop()

			// Remove anything else from podEvents
		loop:
			for {
				select {
				case <-vmEvents:
				default:
					break loop
				}
			}

			globalState.Stop()
			return nil
		case event := <-vmEvents:
			globalState.handleEvent(ctx, event)
		}
	}
}
