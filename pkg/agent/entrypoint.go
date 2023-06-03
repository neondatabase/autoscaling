package agent

import (
	"context"
	"fmt"

	"github.com/tychoish/fun/pubsub"
	"github.com/tychoish/fun/srv"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/agent/billing"
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

	watchMetrics := watch.NewMetrics("autoscaling_agent_watchers")

	klog.Info("Starting VM watcher")
	vmWatchStore, err := startVMWatcher(ctx, r.Config, r.VMClient, watchMetrics, r.EnvArgs.K8sNodeName, pushToQueue)
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
	schedulerStore, err := schedwatch.StartSchedulerWatcher(ctx, watcherLogger, r.KubeClient, watchMetrics, broker, r.Config.Scheduler.SchedulerName)
	if err != nil {
		return fmt.Errorf("starting scheduler watch server: %w", err)
	}
	defer schedulerStore.Stop()

	globalState, promReg := r.newAgentState(r.EnvArgs.K8sPodIP, broker, schedulerStore)
	watchMetrics.MustRegister(promReg)

	if r.Config.Billing != nil {
		klog.Info("Starting billing metrics collector")
		storeForNode := watch.NewIndexedStore(vmWatchStore, billing.NewVMNodeIndex(r.EnvArgs.K8sNodeName))

		metrics := billing.NewPromMetrics()
		metrics.MustRegister(promReg)

		// TODO: catch panics here, bubble those into a clean-ish shutdown.
		go billing.RunBillingMetricsCollector(ctx, r.Config.Billing, storeForNode, metrics)
	}

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
				// treat context canceled as a "normal" exit (because it is)
				return nil
			}

			klog.Errorf("vmEventQueue returned error: %s", err)
			return err
		}
		globalState.handleEvent(ctx, event)
	}
}
