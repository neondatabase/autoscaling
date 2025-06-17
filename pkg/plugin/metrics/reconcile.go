package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/neondatabase/autoscaling/pkg/plugin/reconcile"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type Reconcile struct {
	waitDurations    prometheus.Histogram
	waitingTimer     *WaitingTimer
	processDurations *prometheus.HistogramVec
	failing          *prometheus.GaugeVec
	panics           *prometheus.CounterVec
}

func buildReconcileMetrics(reg prometheus.Registerer, numWorkers int) Reconcile {
	// No need to return this - it won't change:
	workers := util.RegisterMetric(reg, prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "autoscaling_plugin_reconcile_workers",
			Help: "Number of worker threads used for reconcile operations",
		},
	))
	workers.Set(float64(numWorkers))

	return Reconcile{
		waitDurations: util.RegisterMetric(reg, prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name: "autoscaling_plugin_reconcile_queue_wait_durations",
				Help: "Duration that items in the reconcile queue are waiting to be picked up",
				Buckets: []float64{
					// 10µs, 100µs,
					0.00001, 0.0001,
					// 1ms, 5ms, 10ms, 50ms, 100ms, 250ms, 500ms, 750ms
					0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 0.75,
					// 1s, 2.5s, 5s, 10s, 20s, 45s
					1.0, 2.5, 5, 10, 20, 45,
				},
			},
		)),
		waitingTimer: util.RegisterMetric(reg, NewWaitingTimer(
			prometheus.Opts{
				Name: "autoscaling_plugin_reconcile_waiting_seconds_total",
				Help: "Total duration where there were some reconcile operations waiting to be picked up",
			},
		)),
		processDurations: util.RegisterMetric(reg, prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "autoscaling_plugin_reconcile_duration_seconds",
				Help: "Duration that items take to be reconciled",
				Buckets: []float64{
					// 10µs, 100µs,
					0.00001, 0.0001,
					// 1ms, 5ms, 10ms, 50ms, 100ms, 250ms, 500ms, 750ms
					0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 0.75,
					// 1s, 2.5s, 5s, 10s, 20s, 45s
					1.0, 2.5, 5, 10, 20, 45,
				},
			},
			[]string{"kind", "outcome"},
		)),
		failing: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_plugin_reconcile_failing_objects",
				Help: "Number of objects currently failing to be reconciled",
			},
			[]string{"kind"},
		)),
		panics: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_reconcile_panics_count",
				Help: "Number of times reconcile operations have panicked",
			},
			[]string{"kind"},
		)),
	}
}

func (r Reconcile) QueueWaitDurationCallback(duration time.Duration) {
	r.waitDurations.Observe(duration.Seconds())
}

func (r Reconcile) QueueSizeCallback(size int) {
	// update the timer so we record the amount of time during which at least some items were
	// waiting in the queue.
	r.waitingTimer.SetWaiting(size != 0)
}

func (r Reconcile) ResultCallback(params reconcile.ObjectParams, duration time.Duration, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	r.processDurations.WithLabelValues(params.GVK.Kind, outcome).Observe(duration.Seconds())
}

func (r Reconcile) ErrorStatsCallback(params reconcile.ObjectParams, stats reconcile.ErrorStats) {
	// update count of current failing objects
	r.failing.WithLabelValues(params.GVK.Kind).Set(float64(stats.TypedCount))
}

func (r Reconcile) PanicCallback(params reconcile.ObjectParams) {
	r.panics.WithLabelValues(params.GVK.Kind).Inc()
}
