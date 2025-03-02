package ipam

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type IPAMMetrics struct {
	ongoing  prometheus.GaugeVec
	duration prometheus.HistogramVec
}

const (
	IPAMAcquire = "acquire"
	IPAMRelease = "release"
	IPAMCleanup = "cleanup"
)

const (
	IPAMSuccess = "success"
	IPAMFailure = "failure"
	IPAMUnknown = "unknown"
)

func NewIPAMMetrics(reg prometheus.Registerer) *IPAMMetrics {
	// Copied bucket values from controller runtime latency metric. We can
	// adjust them in the future if needed.
	buckets := []float64{
		0.005, 0.01, 0.025, 0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0,
		1.25, 1.5, 1.75, 2.0, 2.5, 3.0, 3.5, 4.0, 4.5, 5, 6, 7, 8, 9, 10, 15, 20, 25, 30, 40, 50, 60,
	}

	return &IPAMMetrics{
		ongoing: *util.RegisterMetric(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ipam_ongoing_requests",
			Help: "Number of ongoing IPAM requests",
		}, []string{"action"})),
		duration: *util.RegisterMetric(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ipam_request_duration_seconds",
			Help:    "Duration of IPAM requests",
			Buckets: buckets,
		}, []string{"action", "outcome"})),
	}
}

type metricTimer struct {
	startTime time.Time
	metrics   *IPAMMetrics
	action    string

	// We allow for finish to be called multiple times, but only the first
	// call will have an effect.
	finished bool
}

func (m *IPAMMetrics) StartTimer(action string) *metricTimer {
	m.ongoing.WithLabelValues(action).Inc()

	return &metricTimer{
		startTime: time.Now(),
		metrics:   m,
		action:    action,

		finished: false,
	}
}

func (t *metricTimer) Finish(outcome string) {
	if t.finished {
		return
	}
	t.finished = true

	t.metrics.duration.WithLabelValues(t.action, outcome).Observe(time.Since(t.startTime).Seconds())
	t.metrics.ongoing.WithLabelValues(t.action).Dec()
}
