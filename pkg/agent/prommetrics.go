package agent

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type PromMetrics struct {
	schedulerRequests         *prometheus.CounterVec
	informantRequestsOutbound *prometheus.CounterVec
	informantRequestsInbound  *prometheus.CounterVec
}

func startPrometheusServer(ctx context.Context, globalstate *agentState) (PromMetrics, error) {
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

	if err := util.StartPrometheusMetricsServer(ctx, 9100, reg); err != nil {
		return PromMetrics{}, err
	}

	return PromMetrics{
		schedulerRequests:         schedulerRequests,
		informantRequestsOutbound: informantRequestsOutbound,
		informantRequestsInbound:  informantRequestsInbound,
	}, nil
}
