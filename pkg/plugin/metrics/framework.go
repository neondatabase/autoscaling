package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/neondatabase/autoscaling/pkg/plugin/state"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type Framework struct {
	// inheritedNodeLabels are the labels on the node that are directly included in the metrics,
	// given in the order that they appear in the metric labels.
	inheritedNodeLabels []string

	methodCalls       *prometheus.CounterVec
	methodCallFails   *prometheus.CounterVec
	reserveOverBudget *prometheus.CounterVec
}

func (m *Framework) IncMethodCall(method string, pod *corev1.Pod, ignored bool) {
	az := util.PodPreferredAZIfPresent(pod)
	m.methodCalls.WithLabelValues(method, az, strconv.FormatBool(ignored)).Inc()
}

func (m *Framework) IncFailIfnotSuccess(method string, pod *corev1.Pod, ignored bool, status *framework.Status) {
	// it's normal for Filter to return Unschedulable, because that's its way of filtering out pods.
	if status.IsSuccess() || (method == "Filter" && status.Code() == framework.Unschedulable) {
		return
	}

	az := util.PodPreferredAZIfPresent(pod)
	m.methodCallFails.
		WithLabelValues(method, az, strconv.FormatBool(ignored), status.Code().String()).
		Inc()
}

func (m *Framework) IncReserveOverBudget(ignored bool, node *state.Node) {
	labelValues := []string{node.Name}
	for _, label := range m.inheritedNodeLabels {
		value, _ := node.Labels.Get(label)
		labelValues = append(labelValues, value)
	}
	labelValues = append(labelValues, strconv.FormatBool(ignored))

	m.reserveOverBudget.WithLabelValues(labelValues...).Inc()
}

func buildSchedFrameworkMetrics(labels nodeLabeling, reg prometheus.Registerer) Framework {
	reserveLabels := []string{"node"}
	reserveLabels = append(reserveLabels, labels.metricLabelNames...)
	reserveLabels = append(reserveLabels, "ignored_namespace")

	return Framework{
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
