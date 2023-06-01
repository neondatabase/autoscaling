package billing

// Prometheus metrics for the agent's billing subsystem

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type PromMetrics struct {
	vmsProcessedTotal *prometheus.CounterVec
	vmsCurrent        *prometheus.GaugeVec
	batchSizeCurrent  prometheus.Gauge
	sendErrorsTotal   prometheus.Counter
}

func NewPromMetrics() PromMetrics {
	return PromMetrics{
		vmsProcessedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_billing_vms_processed_total",
				Help: "Total number of times the autoscaler-agent's billing subsystem processes any VM",
			},
			[]string{"is_endpoint", "autoscaling_enabled", "phase"},
		),
		vmsCurrent: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_agent_billing_vms_current",
				Help: "Total current VMs visible to the autoscaler-agent's billing subsystem, labeled by some bits of metadata",
			},
			[]string{"is_endpoint", "autoscaling_enabled", "phase"},
		),
		batchSizeCurrent: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "autoscaling_agent_billing_batch_size",
				Help: "Size of the billing subsystem's most recent batch",
			},
		),
		sendErrorsTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_billing_send_errors_total",
				Help: "Total errors from attempting to send billing events",
			},
		),
	}
}

func (m PromMetrics) MustRegister(reg *prometheus.Registry) {
	reg.MustRegister(m.vmsProcessedTotal)
	reg.MustRegister(m.vmsCurrent)
	reg.MustRegister(m.batchSizeCurrent)
	reg.MustRegister(m.sendErrorsTotal)
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
	m.vmsCurrent.Reset()

	return batchMetrics{
		total: make(map[batchMetricsLabels]int),

		vmsProcessedTotal: m.vmsProcessedTotal,
		vmsCurrent:        m.vmsCurrent,
	}
}

func (b batchMetrics) inc(isEndpoint, autoscalingEnabled bool, phase vmapi.VmPhase) {
	key := batchMetricsLabels{
		isEndpoint:         strconv.FormatBool(isEndpoint),
		autoscalingEnabled: strconv.FormatBool(autoscalingEnabled),
		phase:              string(phase),
	}

	b.total[key] = b.total[key] + 1
	b.vmsProcessedTotal.
		WithLabelValues(key.isEndpoint, key.autoscalingEnabled, key.phase).
		Inc()
}

func (b batchMetrics) finish() {
	for key, count := range b.total {
		b.vmsCurrent.WithLabelValues(key.isEndpoint, key.autoscalingEnabled, key.phase).Set(float64(count))
	}
}
