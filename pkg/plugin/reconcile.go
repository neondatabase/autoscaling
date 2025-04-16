package plugin

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/plugin/metrics"
	"github.com/neondatabase/autoscaling/pkg/plugin/reconcile"
)

func reconcileQueueWaitCallback(metrics *metrics.Plugin, duration time.Duration) {
	metrics.Reconcile.WaitDurations.Observe(duration.Seconds())
}

func reconcileQueueStatusCallback(metrics *metrics.Plugin, waiting bool) {
	// update the timer so we record the amount of time during which at least some items were
	// waiting in the queue.
	metrics.Reconcile.WaitingTimer.SetWaiting(waiting)
}

func reconcileResultCallback(
	metrics *metrics.Plugin,
	params reconcile.ObjectParams,
	duration time.Duration,
	err error,
) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	metrics.Reconcile.ProcessDurations.
		WithLabelValues(params.GVK.Kind, outcome).
		Observe(duration.Seconds())
}

func reconcileErrorStatsCallback(
	metrics *metrics.Plugin,
	config *Config,
	logger *zap.Logger,
	params reconcile.ObjectParams,
	stats reconcile.ErrorStats,
) {
	// update count of current failing objects
	metrics.Reconcile.Failing.
		WithLabelValues(params.GVK.Kind).
		Set(float64(stats.TypedCount))

	// Make sure that repeatedly failing objects are sufficiently noisy
	if stats.SuccessiveFailures >= config.LogSuccessiveFailuresThreshold {
		logger.Warn(
			fmt.Sprintf("%s has failed to reconcile >%d times in a row", params.GVK.Kind, config.LogSuccessiveFailuresThreshold),
			zap.Int("SuccessiveFailures", stats.SuccessiveFailures),
			zap.String("EventKind", string(params.EventKind)),
			reconcile.ObjectMetaLogField(params.GVK.Kind, params.Obj),
		)
	}
}

func reconcilePanicCallback(metrics *metrics.Plugin, params reconcile.ObjectParams) {
	metrics.Reconcile.Panics.WithLabelValues(params.GVK.Kind).Inc()
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
