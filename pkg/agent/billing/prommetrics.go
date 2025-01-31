package billing

// Prometheus metrics for the agent's billing subsystem

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/reporting"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type PromMetrics struct {
	reporting *reporting.EventSinkMetrics

	vmsProcessedTotal *prometheus.CounterVec
	vmsCurrent        *prometheus.GaugeVec
}

func NewPromMetrics(reg prometheus.Registerer) PromMetrics {
	return PromMetrics{
		reporting: reporting.NewEventSinkMetrics("autoscaling_agent_billing", reg),

		vmsProcessedTotal: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_billing_vms_processed_total",
				Help: "Total number of times the autoscaler-agent's billing subsystem processes any VM",
			},
			[]string{"is_endpoint", "autoscaling_enabled", "phase"},
		)),
		vmsCurrent: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_agent_billing_vms_current",
				Help: "Total current VMs visible to the autoscaler-agent's billing subsystem, labeled by some bits of metadata",
			},
			[]string{"is_endpoint", "autoscaling_enabled", "phase"},
		)),
	}
}

type batchMetrics struct {
	total map[batchMetricsLabels]int

	vmsProcessedTotal *prometheus.CounterVec
	vmsCurrent        *prometheus.GaugeVec
}

type batchMetricsLabels struct {
	isEndpoint         string
	autoscalingEnabled string
	phase              string
}

func (m PromMetrics) forBatch() batchMetrics {
	return batchMetrics{
		total: make(map[batchMetricsLabels]int),

		vmsProcessedTotal: m.vmsProcessedTotal,
		vmsCurrent:        m.vmsCurrent,
	}
}

type (
	isEndpointFlag         bool
	autoscalingEnabledFlag bool
)

func (b batchMetrics) inc(isEndpoint isEndpointFlag, autoscalingEnabled autoscalingEnabledFlag, phase vmv1.VmPhase) {
	key := batchMetricsLabels{
		isEndpoint:         strconv.FormatBool(bool(isEndpoint)),
		autoscalingEnabled: strconv.FormatBool(bool(autoscalingEnabled)),
		phase:              string(phase),
	}

	b.total[key] = b.total[key] + 1
	b.vmsProcessedTotal.
		WithLabelValues(key.isEndpoint, key.autoscalingEnabled, key.phase).
		Inc()
}

func (b batchMetrics) finish() {
	b.vmsCurrent.Reset()

	for key, count := range b.total {
		b.vmsCurrent.WithLabelValues(key.isEndpoint, key.autoscalingEnabled, key.phase).Set(float64(count))
	}
}
