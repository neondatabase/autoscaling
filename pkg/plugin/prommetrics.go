package plugin

// defines prometheus metrics and provides the server, via (*AutoscaleEnforcer).startPrometheusServer()

import (
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type PromMetrics struct {
	pluginCalls           *prometheus.CounterVec
	resourceRequests      *prometheus.CounterVec
	validResourceRequests *prometheus.CounterVec
}

func (p *AutoscaleEnforcer) makePrometheusRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()

	// register stock collectors directly:
	//   (even though MustRegister is variadic, the function calls
	//   are cheap and calling it more than once means that when
	//   it panics, we know exactly which metric caused the error.)
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	p.metrics = PromMetrics{
		pluginCalls: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_extension_calls_total",
				Help: "Number of calls to scheduler plugin extension points",
			},
			[]string{"method"},
		)),
		resourceRequests: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_resource_requests_total",
				Help: "Number of resource requests received by the scheduler plugin",
			},
			[]string{"client_addr", "code"},
		)),
		validResourceRequests: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_resource_requests_results_total",
				Help: "Number of resource requests to the scheduler plugin with various results",
			},
			[]string{"code", "node", "has_metrics"},
		)),
	}

	return reg
}
