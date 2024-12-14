package plugin

// Initial setup for the moving pieces of the scheduler plugin.

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	coreclient "k8s.io/client-go/kubernetes"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
	"github.com/neondatabase/autoscaling/pkg/plugin/initevents"
	"github.com/neondatabase/autoscaling/pkg/plugin/reconcile"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

func startPlugin(
	baseCtx context.Context,
	logger *zap.Logger,
	config *Config,
	k8sClient coreclient.Interface,
	vmClient vmclient.Interface,
) (_ *PluginState, finalErr error) {
	// set up a new context to cancel the background tasks if we bail early.
	ctx, cancel := context.WithCancel(baseCtx)
	defer func() {
		if finalErr != nil {
			cancel()
		}
	}()

	promReg := prometheus.NewRegistry()
	registerDefaultCollectors(promReg)

	// pre-define this so that we can reference it in the handlers, knowing that it won't be used
	// until we start the workers (which we do *after* we've set this value).
	var pluginState *PluginState

	initEvents := initevents.NewInitEventsMiddleware()

	reconcileQueue, err := reconcile.NewQueue(
		map[reconcile.Object]reconcile.HandlerFunc{
			&corev1.Node{}: func(logger *zap.Logger, k reconcile.EventKind, obj reconcile.Object) (reconcile.Result, error) {
				return lo.Empty[reconcile.Result](), pluginState.HandleNodeEvent(logger, k, obj.(*corev1.Node))
			},

			&corev1.Pod{}: func(logger *zap.Logger, k reconcile.EventKind, obj reconcile.Object) (reconcile.Result, error) {
				result, err := pluginState.HandlePodEvent(logger, k, obj.(*corev1.Pod))
				return lo.FromPtr(result), err
			},

			&vmv1.VirtualMachineMigration{}: func(logger *zap.Logger, k reconcile.EventKind, obj reconcile.Object) (reconcile.Result, error) {
				vmm := obj.(*vmv1.VirtualMachineMigration)
				return lo.Empty[reconcile.Result](), pluginState.HandleMigrationEvent(logger, k, vmm)
			},
		},
		reconcile.WithBaseContext(ctx),
		reconcile.WithMiddleware(initEvents),
		// Note: we need one layer of indirection for callbacks referencing pluginState, because
		// it's initialized later, so directly referencing the methods at this point will use the
		// nil pluginState and panic on use.
		reconcile.WithQueueWaitDurationCallback(func(duration time.Duration) {
			pluginState.reconcileQueueWaitCallback(duration)
		}),
		reconcile.WithResultCallback(func(params reconcile.MiddlewareParams, duration time.Duration, err error) {
			pluginState.reconcileResultCallback(params, duration, err)
		}),
		reconcile.WithErrorStatsCallback(func(params reconcile.MiddlewareParams, stats reconcile.ErrorStats) {
			pluginState.reconcileErrorStatsCallback(logger, params, stats)
		}),
		reconcile.WithPanicCallback(func(params reconcile.MiddlewareParams) {
			pluginState.reconcilePanicCallback(params)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("could not setup reconcile queue: %w", err)
	}

	watchMetrics := watch.NewMetrics("autoscaling_plugin_watchers")
	watchMetrics.MustRegister(promReg)

	// Fetch the nodes first, so that they'll *tend* to be added to the state before we try to
	// handle the pods that are on them.
	// It's not guaranteed, because parallel workers acquiring the same lock ends up with *some*
	// reordered handling, but it helps dramatically reduce the number of warnings in practice.
	nodeHandlers := watchHandlers[*corev1.Node](reconcileQueue, initEvents)
	nodeStore, err := watchNodeEvents(ctx, logger, k8sClient, watchMetrics, nodeHandlers)
	if err != nil {
		return nil, fmt.Errorf("could not start watch on Node events: %w", err)
	}

	podHandlers := watchHandlers[*corev1.Pod](reconcileQueue, initEvents)
	podStore, err := watchPodEvents(ctx, logger, k8sClient, watchMetrics, podHandlers)
	if err != nil {
		return nil, fmt.Errorf("could not start watch on Pod events: %w", err)
	}

	// we make these handlers with nil instead of initEvents so that we're not blocking plugin setup
	// on the migration objects being handled.
	vmmHandlers := watchHandlers[*vmv1.VirtualMachineMigration](reconcileQueue, nil)
	if err := watchMigrationEvents(ctx, logger, vmClient, watchMetrics, vmmHandlers); err != nil {
		return nil, fmt.Errorf("could not start watch on Pod events: %w", err)
	}

	pluginState = NewPluginState(*config, vmClient, promReg, podStore, nodeStore)

	// Start the workers for the queue. We can't do these earlier because our handlers depend on the
	// PluginState that only exists now.
	reconcileLogger := logger.Named("reconcile")
	for i := 0; i < config.ReconcileWorkers; i++ {
		go reconcileWorker(ctx, reconcileLogger, reconcileQueue)
	}

	err = util.StartPrometheusMetricsServer(ctx, logger.Named("prometheus"), 9100, promReg)
	if err != nil {
		return nil, fmt.Errorf("could not start prometheus server: %w", err)
	}

	indexedPodStore := watch.NewIndexedStore(podStore, watch.NewNameIndex[corev1.Pod]())
	getPod := func(p util.NamespacedName) (*corev1.Pod, bool) {
		return indexedPodStore.GetIndexed(func(index *watch.NameIndex[corev1.Pod]) (*corev1.Pod, bool) {
			return index.Get(p.Namespace, p.Name)
		})
	}
	err = pluginState.startPermitHandler(ctx, logger.Named("agent-handler"), getPod, podStore.Listen)
	if err != nil {
		return nil, fmt.Errorf("could not start agent request handler: %w", err)
	}

	// The reconciles are ongoing -- we need to wait until they're finished.
	timeout := time.Second * time.Duration(config.StartupEventHandlingTimeoutSeconds)
	start := time.Now()
	select {
	case <-ctx.Done():
		logger.Warn("Context unexpectedly canceled while waiting for initial events to be handled")
		return nil, ctx.Err()
	case <-time.After(timeout):
		logger.Error("Timed out handling initial events")
		// intentionally use separate log lines, to emit *something* if it deadlocks.
		logger.Warn("Objects remaining to be reconciled", zap.Any("Remaining", initEvents.Remaining()))
		return nil, fmt.Errorf("timed out after %s while handling initial events", time.Since(start))
	case <-initEvents.Done():
		logger.Info("Handled all initial events", zap.Duration("duration", time.Since(start)))
	}

	// Reconciles are finished -- for now. Some of them may be waiting on startup to complete, in
	// order to guarantee accuracy. Let's mark startup as done, and requeue those:
	pluginState.mu.Lock()
	defer pluginState.mu.Unlock()

	pluginState.startupDone = true
	for uid := range pluginState.requeueAfterStartup {
		err := pluginState.requeuePod(uid)
		if err != nil {
			logger.Warn(
				"Could not requeue Pod after startup, maybe it was deleted?",
				zap.String("UID", string(uid)),
			)
		}
	}
	clear(pluginState.requeueAfterStartup)

	return pluginState, nil
}

func (s *PluginState) reconcileQueueWaitCallback(duration time.Duration) {
	s.metrics.reconcile.waitDurations.Observe(duration.Seconds())
}

func (s *PluginState) reconcileResultCallback(params reconcile.MiddlewareParams, duration time.Duration, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	s.metrics.reconcile.processDurations.
		WithLabelValues(params.GVK.Kind, outcome).
		Observe(duration.Seconds())
}

func (s *PluginState) reconcileErrorStatsCallback(logger *zap.Logger, params reconcile.MiddlewareParams, stats reconcile.ErrorStats) {
	// update count of current failing objects
	s.metrics.reconcile.failing.
		WithLabelValues(params.GVK.Kind).
		Set(float64(stats.TypedCount))

	// Make sure that repeatedly failing objects are sufficiently noisy
	if stats.SuccessiveFailures >= s.config.LogSuccessiveFailuresThreshold {
		logger.Warn(
			fmt.Sprintf("%s has failed to reconcile >%d times in a row", params.GVK.Kind, s.config.LogSuccessiveFailuresThreshold),
			zap.Int("SuccessiveFailures", stats.SuccessiveFailures),
			zap.String("EventKind", string(params.EventKind)),
			reconcile.ObjectMetaLogField(params.GVK.Kind, params.Obj),
		)
	}
}

func (s *PluginState) reconcilePanicCallback(params reconcile.MiddlewareParams) {
	s.metrics.reconcile.panics.WithLabelValues(params.GVK.Kind).Inc()
}

func reconcileWorker(ctx context.Context, logger *zap.Logger, queue *reconcile.Queue) {
	wait := queue.WaitChan()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-wait:
			if !ok {
				// channel closed; we're done.
				return
			}
			callback, ok := queue.Next()
			if !ok {
				// Spurious wake-up; retry.
				continue
			}

			callback(logger)
		}
	}
}