package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type Reconcile struct {
	Workers          prometheus.Gauge
	WaitDurations    prometheus.Histogram
	WaitingTimer     *WaitingReconcilesTimer
	ProcessDurations *prometheus.HistogramVec
	Failing          *prometheus.GaugeVec
	Panics           *prometheus.CounterVec
}

func buildReconcileMetrics(reg prometheus.Registerer) Reconcile {
	return Reconcile{
		Workers: util.RegisterMetric(reg, prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "autoscaling_plugin_reconcile_workers",
				Help: "Number of worker threads used for reconcile operations",
			},
		)),
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
		WaitingTimer: util.RegisterMetric(reg, NewWaitingReconcilesTimer(
			prometheus.Opts{
				Name: "autoscaling_plugin_reconcile_waiting_seconds_total",
				Help: "Total duration that there were some reconcile operations waiting to be picked up",
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

type WaitingReconcilesTimer struct {
	mu sync.Mutex

	waiting        bool
	lastTransition time.Time
	totalTime      time.Duration

	gauge prometheus.GaugeFunc
}

func NewWaitingReconcilesTimer(opts prometheus.Opts) *WaitingReconcilesTimer {
	t := &WaitingReconcilesTimer{
		mu:             sync.Mutex{},
		waiting:        false,
		lastTransition: time.Now(),
		totalTime:      0,
		gauge:          nil, // set below
	}

	t.gauge = prometheus.NewGaugeFunc(prometheus.GaugeOpts(opts), func() float64 {
		return t.getTotal().Seconds()
	})

	return t
}

func (t *WaitingReconcilesTimer) SetWaiting(waiting bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.waiting == waiting {
		return
	}

	now := time.Now()
	// If it was previously marked as waiting (but now no longer), then add the time since we
	// started waiting to the running total.
	if t.waiting {
		t.totalTime += now.Sub(t.lastTransition)
	}
	// Otherwise, we'll mark ourselves as waiting (or not), and set the last change to now so that
	// the next switch records the time since now.
	t.waiting = waiting
	t.lastTransition = now
}

func (t *WaitingReconcilesTimer) getTotal() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	total := t.totalTime
	if t.waiting {
		total += time.Since(t.lastTransition)
	}
	return total
}

// Describe implements prometheus.Collector
func (t *WaitingReconcilesTimer) Describe(ch chan<- *prometheus.Desc) {
	t.gauge.Describe(ch)
}

// Collect implements prometheus.Collector
func (t *WaitingReconcilesTimer) Collect(ch chan<- prometheus.Metric) {
	t.gauge.Collect(ch)
}
