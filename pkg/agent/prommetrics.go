package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type PromMetrics struct {
	schedulerRequests         *prometheus.CounterVec
	informantRequestsOutbound *prometheus.CounterVec
	informantRequestsInbound  *prometheus.CounterVec
}

func makePrometheusParts(globalstate *agentState) (PromMetrics, *prometheus.Registry) {
	schedulerRequests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "scheduler_plugin_requests_total",
			Help: "Number of attempted HTTP requests to the scheduler plugin",
		},
		[]string{"code"},
	)
	informantRequestsOutbound := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "informant_outbound_requests_total",
			Help: "Number of attempted HTTP requests to vm-informants",
		},
		[]string{"code"},
	)
	informantRequestsInbound := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "informant_inbound_requests_total",
			Help: "Number of HTTP requests from vm-informants",
		},
		[]string{"endpoint", "code"},
	)
	totalVMs := prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "tracked_vms_current",
			Help: "Number of VMs on the autoscaler-agent's node that it's tracking",
		},
		func() float64 {
			globalstate.lock.Lock()
			defer globalstate.lock.Unlock()

			return float64(len(globalstate.pods))
		},
	)

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		schedulerRequests,
		informantRequestsOutbound,
		informantRequestsInbound,
		totalVMs,
	)

	return PromMetrics{
		schedulerRequests:         schedulerRequests,
		informantRequestsOutbound: informantRequestsOutbound,
		informantRequestsInbound:  informantRequestsInbound,
	}, reg
}
