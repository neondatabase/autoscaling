package plugin

// defines prometheus metrics and provides the server, via (*AutoscaleEnforcer).startPrometheusServer()

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type PromMetrics struct {
	pluginCalls           *prometheus.CounterVec
	pluginCallFails       *prometheus.CounterVec
	resourceRequests      *prometheus.CounterVec
	validResourceRequests *prometheus.CounterVec
	nodeCPUResources      *prometheus.GaugeVec
	nodeMemResources      *prometheus.GaugeVec
	migrationCreations    prometheus.Counter
	migrationDeletions    *prometheus.CounterVec
	migrationCreateFails  prometheus.Counter
	migrationDeleteFails  *prometheus.CounterVec
	reserveShouldDeny     *prometheus.CounterVec
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
			[]string{"method", "desired_availability_zone", "ignored_namespace"},
		)),
		pluginCallFails: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_extension_call_fails_total",
				Help: "Number of unsuccessful calls to scheduler plugin extension points",
			},
			[]string{"method", "desired_availability_zone", "ignored_namespace", "status"},
		)),
		resourceRequests: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_resource_requests_total",
				Help: "Number of resource requests received by the scheduler plugin",
			},
			[]string{"code"},
		)),
		validResourceRequests: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_resource_requests_results_total",
				Help: "Number of resource requests to the scheduler plugin with various results",
			},
			[]string{"code", "node", "has_metrics"},
		)),
		nodeCPUResources: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_plugin_node_cpu_resources_current",
				Help: "Current amount of CPU for 'nodeResourceState' fields",
			},
			[]string{"node", "node_group", "availability_zone", "field"},
		)),
		nodeMemResources: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_plugin_node_mem_resources_current",
				Help: "Current amount of memory (in bytes) for 'nodeResourceState' fields",
			},
			[]string{"node", "node_group", "availability_zone", "field"},
		)),
		migrationCreations: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_migrations_created_total",
				Help: "Number of successful VirtualMachineMigration Create requests by the plugin",
			},
		)),
		migrationDeletions: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_migrations_deleted_total",
				Help: "Number of successful VirtualMachineMigration Delete requests by the plugin",
			},
			[]string{"phase"},
		)),
		migrationCreateFails: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_migration_create_fails_total",
				Help: "Number of failed VirtualMachineMigration Create requests by the plugin",
			},
		)),
		migrationDeleteFails: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_migration_delete_fails_total",
				Help: "Number of failed VirtualMachineMigration Delete requests by the plugin",
			},
			[]string{"phase"},
		)),
		reserveShouldDeny: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_reserve_should_deny_total",
				Help: "Number of times the plugin should deny a reservation",
			},
			[]string{"availability_zone", "node", "node_group"},
		)),
	}

	return reg
}

func (m *PromMetrics) IncMethodCall(method string, pod *corev1.Pod, ignored bool) {
	m.pluginCalls.WithLabelValues(method, util.PodPreferredAZIfPresent(pod), strconv.FormatBool(ignored)).Inc()
}

func (m *PromMetrics) IncFailIfNotSuccess(method string, pod *corev1.Pod, ignored bool, status *framework.Status) {
	if !status.IsSuccess() {
		return
	}

	m.pluginCallFails.WithLabelValues(method, util.PodPreferredAZIfPresent(pod), strconv.FormatBool(ignored), status.Code().String())
}

func (m *PromMetrics) IncReserveShouldDeny(pod *corev1.Pod, node *nodeState) {
	m.reserveShouldDeny.WithLabelValues(util.PodPreferredAZIfPresent(pod), node.name, node.nodeGroup).Inc()
}
