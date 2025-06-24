package ipam

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type IPAMMetrics struct {
	ongoing  *prometheus.GaugeVec
	duration *prometheus.HistogramVec
}

const (
	IPAMSuccess = "success"
	IPAMFailure = "failure"
	IPAMPanic   = "panic"
)

func NewIPAMMetrics(reg prometheus.Registerer) *IPAMMetrics {
	// Start with a small set, can add more if needed.
	buckets := []float64{
		0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10, 30,
	}

	return &IPAMMetrics{
		ongoing: util.RegisterMetric(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ipam_ongoing_requests",
			Help: "Number of ongoing IPAM requests",
		}, []string{"action"})),
		duration: util.RegisterMetric(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
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
