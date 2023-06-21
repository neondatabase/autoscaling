package plugin

// defines prometheus metrics and provides the server, via (*AutoscaleEnforcer).startPrometheusServer()

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type PromMetrics struct {
	pluginCalls           *prometheus.CounterVec
	fullFilterSuccesses   prometheus.Counter
	fullFilterRejections  *prometheus.CounterVec
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
		// the util.RegisterMetric() function registers the collector and returns
		// it so we can set it directly on the output structure.
		pluginCalls: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_extension_calls_total",
				Help: "Number of calls to scheduler plugin extension points",
			},
			[]string{"method"},
		)),
		fullFilterSuccesses: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_full_filter_successes_total",
				Help: "Number of successful Filter stages for any pod",
			},
		)),
		fullFilterRejections: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_full_filter_rejections_total",
				Help: "For each pod, number of times rejected by *all* Filter evaluations",
			},
			[]string{"pod_name"},
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
