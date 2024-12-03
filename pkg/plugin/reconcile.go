package plugin

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/plugin/reconcile"
)

func (s *PluginState) reconcileQueueWaitCallback(duration time.Duration) {
	s.metrics.Reconcile.WaitDurations.Observe(duration.Seconds())
}

func (s *PluginState) reconcileResultCallback(params reconcile.ObjectParams, duration time.Duration, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	s.metrics.Reconcile.ProcessDurations.
		WithLabelValues(params.GVK.Kind, outcome).
		Observe(duration.Seconds())
}

func (s *PluginState) reconcileErrorStatsCallback(logger *zap.Logger, params reconcile.ObjectParams, stats reconcile.ErrorStats) {
	// update count of current failing objects
	s.metrics.Reconcile.Failing.
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

func (s *PluginState) reconcilePanicCallback(params reconcile.ObjectParams) {
	s.metrics.Reconcile.Panics.WithLabelValues(params.GVK.Kind).Inc()
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
