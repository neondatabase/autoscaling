package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type Reconcile struct {
	WaitDurations    prometheus.Histogram
	ProcessDurations *prometheus.HistogramVec
	Failing          *prometheus.GaugeVec
	Panics           *prometheus.CounterVec
}

func buildReconcileMetrics(reg prometheus.Registerer) Reconcile {
	return Reconcile{
		WaitDurations: util.RegisterMetric(reg, prometheus.NewHistogram(
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
		ProcessDurations: util.RegisterMetric(reg, prometheus.NewHistogramVec(
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
		Failing: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_plugin_reconcile_failing_objects",
				Help: "Number of objects currently failing to be reconciled",
			},
			[]string{"kind"},
		)),
		Panics: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_reconcile_panics_count",
				Help: "Number of times reconcile operations have panicked",
			},
			[]string{"kind"},
		)),
	}
}
