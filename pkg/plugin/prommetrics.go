package plugin

// defines prometheus metrics and provides the server, via (*AutoscaleEnforcer).startPrometheusServer()

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type PromMetrics struct {
	pluginCalls           *prometheus.CounterVec
	resourceRequests      *prometheus.CounterVec
	validResourceRequests *prometheus.CounterVec
}

func (p *AutoscaleEnforcer) makePrometheusRegistry() *prometheus.Registry {
	p.metrics = PromMetrics{
		pluginCalls: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "plugin_extension_calls_total",
				Help: "Number of calls to plugin extension points",
			},
			[]string{"method"},
		),
		resourceRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "plugin_resource_requests_total",
				Help: "Number of resource requests",
			},
			[]string{"client_addr", "code"},
		),
		validResourceRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "plugin_resource_requests_results_total",
				Help: "Number of resource requests with various results",
			},
			[]string{"code", "node", "has_metrics"},
		),
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		p.metrics.pluginCalls,
		p.metrics.resourceRequests,
		p.metrics.validResourceRequests,
	)

	return reg
}
