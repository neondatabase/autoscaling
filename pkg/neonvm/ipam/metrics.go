package ipam

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	v1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type IPAMMetrics struct {
	ongoing         *prometheus.GaugeVec
	duration        *prometheus.HistogramVec
	allocationsSize *prometheus.GaugeVec
	quarantineSize  *prometheus.GaugeVec
}

type IPAMAction string

const (
	IPAMAcquire IPAMAction = "acquire"
	IPAMRelease IPAMAction = "release"
)

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
		allocationsSize: util.RegisterMetric(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ipam_allocations_size",
			Help: "Size of IPAM allocations map",
		}, []string{"pool"})),
		quarantineSize: util.RegisterMetric(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ipam_quarantine_size",
			Help: "Size of IPAM quarantine list",
		}, []string{"pool"})),
	}
}

func (m *IPAMMetrics) PoolChanged(pool *v1.IPPool) {
	m.allocationsSize.WithLabelValues(pool.Name).Set(float64(len(pool.Spec.Allocations)))
	m.quarantineSize.WithLabelValues(pool.Name).Set(float64(len(pool.Spec.QuarantinedOffsets)))
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
