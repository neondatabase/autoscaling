package metrics

import (
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"golang.org/x/exp/slices"

	"github.com/neondatabase/autoscaling/pkg/util"
)

func RegisterDefaultCollectors(reg prometheus.Registerer) {
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

type Plugin struct {
	nodeLabels nodeLabeling

	Framework Framework
	Nodes     Node
	Reconcile Reconcile

	ResourceRequests      *prometheus.CounterVec
	ValidResourceRequests *prometheus.CounterVec

	K8sOps *prometheus.CounterVec
}

func BuildPluginMetrics(nodeMetricLabels map[string]string, reg prometheus.Registerer) Plugin {
	nodeLabels := buildNodeLabels(nodeMetricLabels)

	return Plugin{
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

func (m *Plugin) RecordK8sOp(opKind string, objKind string, objName string, err error) {
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
