package ipam

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/metricfunc"
)

type IPAMMetrics struct {
	ongoing  *prometheus.GaugeVec
	duration *prometheus.HistogramVec

	managerIPCount *metricfunc.GaugeVecFunc
	poolIPCount    *prometheus.GaugeVec
}

const (
	IPAMSuccess = "success"
	IPAMFailure = "failure"
	IPAMPanic   = "panic"
)

const (
	ActionLabel  = "action"
	OutcomeLabel = "outcome"
	PoolLabel    = "pool"
	StateLabel   = "state"
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
		}, []string{ActionLabel})),
		duration: util.RegisterMetric(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ipam_request_duration_seconds",
			Help:    "Duration of IPAM requests",
			Buckets: buckets,
		}, []string{ActionLabel, OutcomeLabel})),
		managerIPCount: util.RegisterMetric(reg, metricfunc.NewGaugeVecFunc(prometheus.GaugeOpts{
			Name: "ipam_manager_ip_count",
			Help: "Numbers IPs in different states in IPAM Manager",
		}, []string{PoolLabel, StateLabel})),
		poolIPCount: util.RegisterMetric(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ipam_pool_ip_count",
			Help: "Numbers IPs in different states in IPAM Pool",
		}, []string{PoolLabel, StateLabel})),
	}
}

func (m *IPAMMetrics) AddManager(manager *Manager) {
	m.managerIPCount.Add(manager.ipCountMetric)
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
