package scalingevents

// Prometheus metrics for the agent's scaling event reporting subsystem

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/neondatabase/autoscaling/pkg/reporting"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type PromMetrics struct {
	reporting  *reporting.EventSinkMetrics
	totalCount *prometheus.GaugeVec
}

func NewPromMetrics(reg prometheus.Registerer) PromMetrics {
	return PromMetrics{
		reporting: reporting.NewEventSinkMetrics("autoscaling_agent_scalingevents", reg),
		totalCount: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_agent_scaling_events_total",
				Help: "Total number of scaling events generated",
			},
			[]string{"kind"},
		)),
	}
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
