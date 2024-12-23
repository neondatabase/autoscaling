package metrics

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"golang.org/x/exp/slices"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/neondatabase/autoscaling/pkg/plugin/state"
	"github.com/neondatabase/autoscaling/pkg/util"
)

func RegisterDefaultCollectors(reg prometheus.Registerer) {
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

type nodeLabeling struct {
	// k8sLabelNames is the ordered list of labels on Node objects that we directly include in
	// node-related metrics.
	k8sLabelNames []string

	// metricLabelnames is the ordered list of the *metric* labels that we use to represent the
	// kubernetes labels from k8sLabelNames.
	//
	// Each metricLabelNames[i] is the metric label marking the value of the Node object's
	// .metadata.labels[k8sLabelNames[i]].
	metricLabelNames []string
}

type PluginMetrics struct {
	nodeLabels nodeLabeling

	Framework frameworkMetrics
	Nodes     nodeMetrics
	Reconcile reconcileMetrics

	ResourceRequests      *prometheus.CounterVec
	ValidResourceRequests *prometheus.CounterVec

	K8sOps *prometheus.CounterVec
}

func BuildPluginMetrics(nodeMetricLabels map[string]string, reg prometheus.Registerer) PluginMetrics {
	nodeLabels := buildNodeLabels(nodeMetricLabels)

	return PluginMetrics{
		nodeLabels: nodeLabels,
		Framework:  buildSchedFrameworkMetrics(nodeLabels, reg),
		Nodes:      buildNodeMetrics(nodeLabels, reg),
		Reconcile:  buildReconcileMetrics(reg),

		ResourceRequests: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_resource_requests_total",
				Help: "Number of resource requests received by the scheduler plugin",
			},
			[]string{"code"},
		)),
		ValidResourceRequests: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_resource_requests_results_total",
				Help: "Number of resource requests to the scheduler plugin with various results",
			},
			[]string{"code", "node"},
		)),

		K8sOps: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_k8s_ops_total",
				Help: "Number of k8s API requests and their outcome",
			},
			[]string{"op", "kind", "outcome"},
		)),
	}
}

func buildNodeLabels(nodeMetricLabels map[string]string) nodeLabeling {
	type labelPair struct {
		metricLabel string
		k8sLabel    string
	}
	labels := []labelPair{}
	for metricLabel, k8sLabel := range nodeMetricLabels {
		labels = append(labels, labelPair{
			metricLabel: metricLabel,
			k8sLabel:    k8sLabel,
		})
	}
	slices.SortFunc(labels, func(x, y labelPair) bool {
		if x.metricLabel == y.metricLabel {
			return x.k8sLabel < y.k8sLabel
		}
		return x.metricLabel < y.metricLabel
	})

	k8sLabels := []string{}
	metricLabels := []string{}
	for _, p := range labels {
		k8sLabels = append(k8sLabels, p.k8sLabel)
		metricLabels = append(metricLabels, p.metricLabel)
	}

	return nodeLabeling{
		k8sLabelNames:    k8sLabels,
		metricLabelNames: metricLabels,
	}
}

type nodeMetrics struct {
	// InheritedLabels are the labels on the node that are directly used as part of the metrics
	InheritedLabels []string

	cpu *prometheus.GaugeVec
	mem *prometheus.GaugeVec
}

func buildNodeMetrics(labels nodeLabeling, reg prometheus.Registerer) nodeMetrics {
	finalMetricLabels := []string{"node"}
	finalMetricLabels = append(finalMetricLabels, labels.metricLabelNames...)
	finalMetricLabels = append(finalMetricLabels, "field")

	return nodeMetrics{
		InheritedLabels: labels.k8sLabelNames,

		cpu: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_plugin_node_cpu_resources_current",
				Help: "Current amount of CPU for 'state.NodeResources' fields",
			},
			finalMetricLabels,
		)),
		mem: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_plugin_node_mem_resources_current",
				Help: "Current amount of memory (in bytes) for 'state.NodeResources' fields",
			},
			finalMetricLabels,
		)),
	}
}

func (m nodeMetrics) Update(node *state.Node) {
	commonLabels := []string{node.Name}
	for _, label := range m.InheritedLabels {
		value, _ := node.Labels.Get(label)
		commonLabels = append(commonLabels, value)
	}

	// Remove old metrics before setting the new ones, because otherwise we may end up with
	// un-updated metrics if node labels change.
	m.Remove(node)

	for _, f := range node.CPU.Fields() {
		//nolint:gocritic // assigning append value to a different slice is intentional here
		labels := append(commonLabels, f.Name)
		m.cpu.WithLabelValues(labels...).Set(f.Value.AsFloat64())
	}
	for _, f := range node.Mem.Fields() {
		//nolint:gocritic // assigning append value to a different slice is intentional here
		labels := append(commonLabels, f.Name)
		m.mem.WithLabelValues(labels...).Set(f.Value.AsFloat64())
	}
}

func (m nodeMetrics) Remove(node *state.Node) {
	baseMatch := prometheus.Labels{"node": node.Name}
	m.cpu.DeletePartialMatch(baseMatch)
	m.mem.DeletePartialMatch(baseMatch)
}

type reconcileMetrics struct {
	WaitDurations    prometheus.Histogram
	ProcessDurations *prometheus.HistogramVec
	Failing          *prometheus.GaugeVec
	Panics           *prometheus.CounterVec
}

func buildReconcileMetrics(reg prometheus.Registerer) reconcileMetrics {
	return reconcileMetrics{
		WaitDurations: util.RegisterMetric(reg, prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name: "autoscaling_plugin_reconcile_queue_wait_durations",
				Help: "Duration that items in the reconcile queue are waiting to be picked up",
				Buckets: []float64{
					// 10µs, 100µs,
					0.00001, 0.0001,
					// 1ms, 5ms, 10ms, 50ms, 100ms, 250ms, 500ms, 750ms
					0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 0.75,
					// 1s, 2.5s, 5s, 10s, 20s, 45s
					1.0, 2.5, 5, 10, 20, 45,
				},
			},
		)),
		ProcessDurations: util.RegisterMetric(reg, prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "autoscaling_plugin_reconcile_duration_seconds",
				Help: "Duration that items take to be reconciled",
				Buckets: []float64{
					// 10µs, 100µs,
					0.00001, 0.0001,
					// 1ms, 5ms, 10ms, 50ms, 100ms, 250ms, 500ms, 750ms
					0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 0.75,
					// 1s, 2.5s, 5s, 10s, 20s, 45s
					1.0, 2.5, 5, 10, 20, 45,
				},
			},
			[]string{"kind", "outcome"},
		)),
		Failing: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_plugin_reconcile_failing_objects",
				Help: "Number of objects currently failing to be reconciled",
			},
			[]string{"kind"},
		)),
		Panics: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_reconcile_panics_count",
				Help: "Number of times reconcile operations have panicked",
			},
			[]string{"kind"},
		)),
	}
}

type frameworkMetrics struct {
	// inheritedNodeLabels are the labels on the node that are directly included in the metrics,
	// given in the order that they appear in the metric labels.
	inheritedNodeLabels []string

	methodCalls       *prometheus.CounterVec
	methodCallFails   *prometheus.CounterVec
	reserveOverBudget *prometheus.CounterVec
}

func (m frameworkMetrics) IncMethodCall(method string, pod *corev1.Pod, ignored bool) {
	az := util.PodPreferredAZIfPresent(pod)
	m.methodCalls.WithLabelValues(method, az, strconv.FormatBool(ignored)).Inc()
}

func (m frameworkMetrics) IncFailIfnotSuccess(method string, pod *corev1.Pod, ignored bool, status *framework.Status) {
	// it's normal for Filter to return Unschedulable, because that's its way of filtering out pods.
	if status.IsSuccess() || (method == "Filter" && status.Code() == framework.Unschedulable) {
		return
	}

	az := util.PodPreferredAZIfPresent(pod)
	m.methodCallFails.
		WithLabelValues(method, az, strconv.FormatBool(ignored), status.Code().String()).
		Inc()
}

func (m frameworkMetrics) IncReserveOverBudget(ignored bool, node *state.Node) {
	labelValues := []string{node.Name}
	for _, label := range m.inheritedNodeLabels {
		value, _ := node.Labels.Get(label)
		labelValues = append(labelValues, value)
	}
	labelValues = append(labelValues, strconv.FormatBool(ignored))

	m.reserveOverBudget.WithLabelValues(labelValues...).Inc()
}

func buildSchedFrameworkMetrics(labels nodeLabeling, reg prometheus.Registerer) frameworkMetrics {
	reserveLabels := []string{"node"}
	reserveLabels = append(reserveLabels, labels.metricLabelNames...)
	reserveLabels = append(reserveLabels, "ignored_namespace")

	return frameworkMetrics{
		inheritedNodeLabels: labels.k8sLabelNames,

		methodCalls: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_extension_calls_total",
				Help: "Number of calls to scheduler plugin extension points",
			},
			[]string{"method", "desired_availability_zone", "ignored_namespace"},
		)),
		methodCallFails: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_extension_call_fails_total",
				Help: "Number of unsuccessful calls to scheduler plugin extension points",
			},
			[]string{"method", "desired_availability_zone", "ignored_namespace", "status"},
		)),
		reserveOverBudget: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_plugin_reserve_should_deny_total",
				Help: "Number of times the plugin should deny a reservation",
			},
			reserveLabels,
		)),
	}
}

func (m *PluginMetrics) RecordK8sOp(opKind string, objKind string, objName string, err error) {
	if err == nil {
		m.K8sOps.WithLabelValues(opKind, objKind, "success").Inc()
		return
	}

	// error is non-nil; let's prepare it to be a metric label.
	errMsg := util.RootError(err).Error()
	// Some error messages contain the object name. We could try to filter them all out, but
	// it's probably more maintainable to just keep them as-is and remove the name.
	errMsg = strings.ReplaceAll(errMsg, objName, "<name>")

	outcome := fmt.Sprintf("error: %s", errMsg)

	m.K8sOps.WithLabelValues(opKind, objKind, outcome).Inc()
}
