package metrics

import (
	"slices"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/neondatabase/autoscaling/pkg/plugin/state"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type Node struct {
	// InheritedLabels are the labels on the node that are directly used as part of the metrics
	InheritedLabels []string

	// mu locks access to lastLabels
	mu sync.Mutex
	// map of node name -> list of labels that were last used in metrics
	lastLabels map[string][]string

	cpu *prometheus.GaugeVec
	mem *prometheus.GaugeVec
}

func buildNodeMetrics(labels nodeLabeling, reg prometheus.Registerer) Node {
	finalMetricLabels := []string{"node"}
	finalMetricLabels = append(finalMetricLabels, labels.metricLabelNames...)
	finalMetricLabels = append(finalMetricLabels, "field")

	return Node{
		InheritedLabels: labels.k8sLabelNames,

		mu:         sync.Mutex{},
		lastLabels: make(map[string][]string),

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

func (m *Node) Update(node *state.Node) {
	commonLabels := []string{node.Name}
	for _, label := range m.InheritedLabels {
		value, _ := node.Labels.Get(label)
		commonLabels = append(commonLabels, value)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !slices.Equal(commonLabels, m.lastLabels[node.Name]) {
		// Remove old metrics before setting the new ones
		m.removeLocked(node)
	}

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

	m.lastLabels[node.Name] = commonLabels
}

func (m *Node) Remove(node *state.Node) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeLocked(node)
}

func (m *Node) removeLocked(node *state.Node) {
	baseMatch := prometheus.Labels{"node": node.Name}
	m.cpu.DeletePartialMatch(baseMatch)
	m.mem.DeletePartialMatch(baseMatch)
	delete(m.lastLabels, node.Name)
}
