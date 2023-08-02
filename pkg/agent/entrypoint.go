package agent

import (
	"context"
	"fmt"

	"github.com/tychoish/fun/pubsub"
	"github.com/tychoish/fun/srv"
	"go.uber.org/zap"

	"k8s.io/client-go/kubernetes"

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

func (r MainRunner) Run(logger *zap.Logger, ctx context.Context) error {
	informantServer, err := StartHttpMuxServer(logger, r.Config.Informant.CallbackPort)
	if err != nil {
		return fmt.Errorf("Error starting muxed informant server: %w", err)
	}

	vmEventQueue := pubsub.NewUnlimitedQueue[vmEvent]()
	defer vmEventQueue.Close()
	pushToQueue := func(ev vmEvent) {
		if err := vmEventQueue.Add(ev); err != nil {
			logger.Warn("Failed to add vmEvent to queue", zap.Object("event", ev), zap.Error(err))
		}
	}

	watchMetrics := watch.NewMetrics("autoscaling_agent_watchers")

	logger.Info("Starting VM watcher")
	vmWatchStore, err := startVMWatcher(ctx, logger, r.Config, r.VMClient, watchMetrics, r.EnvArgs.K8sNodeName, pushToQueue)
	if err != nil {
		return fmt.Errorf("Error starting VM watcher: %w", err)
	}
	defer vmWatchStore.Stop()
	logger.Info("VM watcher started")

	broker := pubsub.NewBroker[schedwatch.WatchEvent](ctx, pubsub.BrokerOptions{})
	if err := srv.GetOrchestrator(ctx).Add(srv.Broker(broker)); err != nil {
		return err
	}

	schedulerStore, err := schedwatch.StartSchedulerWatcher(ctx, logger, r.KubeClient, watchMetrics, broker, r.Config.Scheduler.SchedulerName)
	if err != nil {
		return fmt.Errorf("Starting scheduler watch server: %w", err)
	}
	defer schedulerStore.Stop()

	globalState, promReg := r.newAgentState(logger, r.EnvArgs.K8sPodIP, broker, schedulerStore, informantServer)
	watchMetrics.MustRegister(promReg)

	if r.Config.Billing != nil {
		logger.Info("Starting billing metrics collector")
		storeForNode := watch.NewIndexedStore(vmWatchStore, billing.NewVMNodeIndex(r.EnvArgs.K8sNodeName))

		metrics := billing.NewPromMetrics()
		metrics.MustRegister(promReg)

		// TODO: catch panics here, bubble those into a clean-ish shutdown.
		go billing.RunBillingMetricsCollector(ctx, logger, r.Config.Billing, storeForNode, metrics)
	}

	if err := util.StartPrometheusMetricsServer(ctx, logger.Named("prometheus"), 9100, promReg); err != nil {
		return fmt.Errorf("Error starting prometheus metrics server: %w", err)
	}

	if r.Config.DumpState != nil {
		logger.Info("Starting 'dump state' server")
		if err := globalState.StartDumpStateServer(ctx, logger.Named("dump-state"), r.Config.DumpState); err != nil {
			return fmt.Errorf("Error starting dump state server: %w", err)
		}
	}

	logger.Info("Entering main loop")
	for {
		event, err := vmEventQueue.Wait(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// treat context canceled as a "normal" exit (because it is)
				return nil
			}

			logger.Error("vmEventQueue returned error", zap.Error(err))
			return err
		}
		globalState.handleEvent(ctx, logger, event)
	}
}
