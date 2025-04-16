package plugin

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/plugin/reconcile"
)

func logSuccessiveReconcileFailure(
	logger *zap.Logger,
	params reconcile.ObjectParams,
	stats reconcile.ErrorStats,
	threshold int,
) {
	// Make sure that repeatedly failing objects are sufficiently noisy
	if stats.SuccessiveFailures >= threshold {
		logger.Warn(
			fmt.Sprintf("%s has failed to reconcile >%d times in a row", params.GVK.Kind, threshold),
			zap.Int("SuccessiveFailures", stats.SuccessiveFailures),
			zap.String("EventKind", string(params.EventKind)),
			reconcile.ObjectMetaLogField(params.GVK.Kind, params.Obj),
		)
	}
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
