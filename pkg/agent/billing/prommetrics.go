package billing

// Prometheus metrics for the agent's billing subsystem

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type PromMetrics struct {
	vmsProcessedTotal            *prometheus.CounterVec
	vmsCurrent                   *prometheus.GaugeVec
	queueSizeCurrent             prometheus.Gauge
	lastSendDuration             prometheus.Gauge
	sendErrorsTotal              *prometheus.CounterVec
	fetchNetworkUsageErrorsTotal *prometheus.CounterVec
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
		queueSizeCurrent: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "autoscaling_agent_billing_queue_size",
				Help: "Size of the billing subsystem's queue of unsent events",
			},
		),
		lastSendDuration: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "autoscaling_agent_billing_last_send_duration_seconds",
				Help: "Duration, in seconds, that it took to send the latest set of billing events (or current time if ongoing)",
			},
		),
		sendErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_billing_send_errors_total",
				Help: "Total errors from attempting to send billing events",
			},
			[]string{"cause"},
		),
		fetchNetworkUsageErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_billing_fetch_network_usage_errors_total",
				Help: "Total errors from attempting to fetch network usage",
			},
			[]string{"cause"},
		),
	}
}

func (m PromMetrics) MustRegister(reg *prometheus.Registry) {
	reg.MustRegister(m.vmsProcessedTotal)
	reg.MustRegister(m.vmsCurrent)
	reg.MustRegister(m.queueSizeCurrent)
	reg.MustRegister(m.lastSendDuration)
	reg.MustRegister(m.sendErrorsTotal)
	reg.MustRegister(m.fetchNetworkUsageErrorsTotal)
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

type isEndpointFlag bool
type autoscalingEnabledFlag bool

func (b batchMetrics) inc(isEndpoint isEndpointFlag, autoscalingEnabled autoscalingEnabledFlag, phase vmapi.VmPhase) {
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
	for key, count := range b.total {
		b.vmsCurrent.WithLabelValues(key.isEndpoint, key.autoscalingEnabled, key.phase).Set(float64(count))
	}
}
