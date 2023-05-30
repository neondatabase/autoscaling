package agent

import (
	"context"
	"fmt"

	"github.com/tychoish/fun/pubsub"
	"github.com/tychoish/fun/srv"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/agent/schedwatch"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

type MainRunner struct {
	EnvArgs    EnvArgs
	Config     *Config
	KubeClient *kubernetes.Clientset
	VMClient   *vmclient.Clientset
}

func (r MainRunner) Run(ctx context.Context) error {
	buildInfo := util.GetBuildInfo()
	klog.Infof("buildInfo.GitInfo:   %s", buildInfo.GitInfo)
	klog.Infof("buildInfo.GoVersion: %s", buildInfo.GoVersion)

	vmEventQueue := pubsub.NewUnlimitedQueue[vmEvent]()
	defer vmEventQueue.Close()
	pushToQueue := func(ev vmEvent) {
		if err := vmEventQueue.Add(ev); err != nil {
			klog.Warningf("error adding vmEvent %+v to queue: %s", ev, err)
		}
	}

	klog.Info("Starting VM watcher")
	vmWatchStore, err := startVMWatcher(ctx, r.Config, r.VMClient, r.EnvArgs.K8sNodeName, pushToQueue)
	if err != nil {
		return fmt.Errorf("Error starting VM watcher: %w", err)
	}
	defer vmWatchStore.Stop()
	klog.Info("VM watcher started")

	broker := pubsub.NewBroker[schedwatch.WatchEvent](ctx, pubsub.BrokerOptions{})
	if err := srv.GetOrchestrator(ctx).Add(srv.Broker(broker)); err != nil {
		return err
	}

	watcherLogger := util.PrefixLogger{Prefix: "Scheduler Watcher: "}
	schedulerStore, err := schedwatch.StartSchedulerWatcher(ctx, watcherLogger, r.KubeClient, broker, r.Config.Scheduler.SchedulerName)
	if err != nil {
		return fmt.Errorf("starting scheduler watch server: %w", err)
	}
	defer schedulerStore.Stop()

	if r.Config.Billing != nil {
		klog.Info("Starting billing metrics collector")
		// TODO: catch panics here, bubble those into a clean-ish shutdown.
		storeForNode := watch.NewIndexedStore(vmWatchStore, NewVMNodeIndex(r.EnvArgs.K8sNodeName))
		go RunBillingMetricsCollector(ctx, r.Config.Billing, storeForNode)
	}

	globalState, promReg := r.newAgentState(r.EnvArgs.K8sPodIP, broker, schedulerStore)
	if err := util.StartPrometheusMetricsServer(ctx, 9100, promReg); err != nil {
		return fmt.Errorf("Error starting prometheus metrics server: %w", err)
	}

	if r.Config.DumpState != nil {
		klog.Info("Starting 'dump state' server")
		if err := globalState.StartDumpStateServer(ctx, r.Config.DumpState); err != nil {
			return fmt.Errorf("Error starting dump state server: %w", err)
		}
	}

	klog.Info("Entering main loop")
	for {
		event, err := vmEventQueue.Wait(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			} else {
				klog.Errorf("vmEventQueue returned error: %s", err)
				return err
			}
		}
		globalState.handleEvent(ctx, event)
	}
}
