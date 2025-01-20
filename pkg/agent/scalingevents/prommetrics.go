package scalingevents

// Prometheus metrics for the agent's scaling event reporting subsystem

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/neondatabase/autoscaling/pkg/reporting"
)

type PromMetrics struct {
	reporting  *reporting.EventSinkMetrics
	totalCount *prometheus.GaugeVec
}

func NewPromMetrics() PromMetrics {
	return PromMetrics{
		reporting: reporting.NewEventSinkMetrics("autoscaling_agent_events"),
		totalCount: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_agent_scaling_events_total",
				Help: "Total number of scaling events generated",
			},
			[]string{"kind"},
		),
	}
}

func (m PromMetrics) MustRegister(reg *prometheus.Registry) {
	m.reporting.MustRegister(reg)
	reg.MustRegister(m.totalCount)
}

func (m PromMetrics) recordSubmitted(event ScalingEvent) {
	var eventKind string
	switch event.Kind {
	case scalingEventActual, scalingEventHypothetical:
		eventKind = string(event.Kind)
	default:
		eventKind = "unknown"
	}
	m.totalCount.WithLabelValues(eventKind).Inc()
}
