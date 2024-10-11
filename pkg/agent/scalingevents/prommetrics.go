package scalingevents

// Prometheus metrics for the agent's scaling event reporting subsystem

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/neondatabase/autoscaling/pkg/reporting"
)

type PromMetrics struct {
	reporting  *reporting.EventSinkMetrics
	totalCount prometheus.Gauge
}

func NewPromMetrics() PromMetrics {
	return PromMetrics{
		reporting: reporting.NewEventSinkMetrics("autoscaling_agent_events"),
		totalCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "autoscaling_agent_scaling_events_total",
			Help: "Total number of scaling events generated",
		}),
	}
}

func (m PromMetrics) MustRegister(reg *prometheus.Registry) {
	m.reporting.MustRegister(reg)
	reg.MustRegister(m.totalCount)
}
