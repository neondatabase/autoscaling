package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/neondatabase/autoscaling/pkg/agent/core/revsource"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type GlobalMetrics struct {
	schedulerRequests        *prometheus.CounterVec
	schedulerRequestedChange resourceChangePair
	schedulerApprovedChange  resourceChangePair

	monitorRequestsOutbound *prometheus.CounterVec
	monitorRequestsInbound  *prometheus.CounterVec
	monitorRequestedChange  resourceChangePair
	monitorApprovedChange   resourceChangePair

	neonvmRequestsOutbound *prometheus.CounterVec
	neonvmRequestedChange  resourceChangePair

	runnersCount       *prometheus.GaugeVec
	runnerFatalErrors  prometheus.Counter
	runnerThreadPanics prometheus.Counter
	runnerStarts       prometheus.Counter
	runnerRestarts     prometheus.Counter
	runnerNextActions  prometheus.Counter

	scalingLatency prometheus.HistogramVec
	pluginLatency  prometheus.HistogramVec
	monitorLatency prometheus.HistogramVec
	neonvmLatency  prometheus.HistogramVec
}

func (m *GlobalMetrics) PluginLatency() *prometheus.HistogramVec {
	return &m.pluginLatency
}

func (m *GlobalMetrics) MonitorLatency() *prometheus.HistogramVec {
	return &m.monitorLatency
}

func (m *GlobalMetrics) NeonVMLatency() *prometheus.HistogramVec {
	return &m.neonvmLatency
}

type resourceChangePair struct {
	cpu *prometheus.CounterVec
	mem *prometheus.CounterVec
}

const (
	directionLabel    = "direction"
	directionValueInc = "inc"
	directionValueDec = "dec"
)

type runnerMetricState string

const (
	runnerMetricStateOk       runnerMetricState = "ok"
	runnerMetricStateStuck    runnerMetricState = "stuck"
	runnerMetricStateErrored  runnerMetricState = "errored"
	runnerMetricStatePanicked runnerMetricState = "panicked"
)

// Copied bucket values from controller runtime latency metric. We can
// adjust them in the future if needed.
var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0,
	1.25, 1.5, 1.75, 2.0, 2.5, 3.0, 3.5, 4.0, 4.5, 5, 6, 7, 8, 9, 10, 15, 20, 25, 30, 40, 50, 60}

func makeGlobalMetrics() (GlobalMetrics, *prometheus.Registry) {
	reg := prometheus.NewRegistry()

	// register stock collectors directly:
	//   (even though MustRegister is variadic, the function calls
	//   are cheap and calling it more than once means that when
	//   it panics, we know exactly which metric caused the error.)
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	metrics := GlobalMetrics{
		// the util.RegisterMetric() function registers the collector and returns
		// it so we can set it directly on the output structure.

		// ---- SCHEDULER ----
		schedulerRequests: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_scheduler_plugin_requests_total",
				Help: "Number of attempted HTTP requests to the scheduler plugin by autoscaler-agents",
			},
			[]string{"code"},
		)),
		schedulerRequestedChange: resourceChangePair{
			cpu: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_scheduler_plugin_requested_cpu_change_total",
					Help: "Total change in CPU requested from the scheduler",
				},
				[]string{directionLabel},
			)),
			mem: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_scheduler_plugin_requested_mem_change_total",
					Help: "Total change in memory (in MiB) requested from the scheduler",
				},
				[]string{directionLabel},
			)),
		},
		schedulerApprovedChange: resourceChangePair{
			cpu: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_scheduler_plugin_accepted_cpu_change_total",
					Help: "Total change in CPU approved by the scheduler",
				},
				[]string{directionLabel},
			)),
			mem: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_scheduler_plugin_accepted_mem_change_total",
					Help: "Total change in memory (in MiB) approved by the scheduler",
				},
				[]string{directionLabel},
			)),
		},

		// ---- MONITOR ----
		monitorRequestsOutbound: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_monitor_outbound_requests_total",
				Help: "Number of attempted HTTP requests to vm-monitors by autoscaler-agents",
			},
			[]string{"endpoint", "code"},
		)),
		monitorRequestsInbound: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_monitor_inbound_requests_total",
				Help: "Number of HTTP requests from vm-monitors received by autoscaler-agents",
			},
			[]string{"endpoint", "code"},
		)),
		monitorRequestedChange: resourceChangePair{
			cpu: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_monitor_requested_cpu_change_total",
					Help: "Total change in CPU requested from the vm-monitor(s)",
				},
				[]string{directionLabel},
			)),
			mem: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_monitor_requested_mem_change_total",
					Help: "Total change in memory (in MiB) requested from the vm-monitor(s)",
				},
				[]string{directionLabel},
			)),
		},
		monitorApprovedChange: resourceChangePair{
			cpu: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_monitor_approved_cpu_change_total",
					Help: "Total change in CPU approved by the vm-monitor(s)",
				},
				[]string{directionLabel},
			)),
			mem: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_monitor_approved_mem_change_total",
					Help: "Total change in memory (in MiB) approved by the vm-monitor(s)",
				},
				[]string{directionLabel},
			)),
		},

		// ---- NEONVM ----
		neonvmRequestsOutbound: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_neonvm_outbound_requests_total",
				Help: "Number of k8s patch requests to NeonVM objects",
			},
			// NOTE: "result" is either "ok" or "[error: $CAUSE]", with $CAUSE as the root cause of
			// the request error.
			[]string{"result"},
		)),
		neonvmRequestedChange: resourceChangePair{
			cpu: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_neonvm_requested_cpu_change_total",
					Help: "Total change in CPU requested for VMs",
				},
				[]string{directionLabel},
			)),
			mem: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_neonvm_requested_mem_changed_total",
					Help: "Total change in memory (in MiB) requested for VMs",
				},
				[]string{directionLabel},
			)),
		},

		// ---- RUNNER LIFECYCLE ----
		runnersCount: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_agent_runners_current",
				Help: "Number of per-VM runners, with associated metadata",
			},
			// NB: is_endpoint ∈ ("true", "false"), state ∈ runnerMetricState = ("ok", "stuck", "errored", "panicked")
			[]string{"is_endpoint", "state"},
		)),
		runnerFatalErrors: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_runner_fatal_errors_total",
				Help: "Number of fatal errors from autoscaler-agent per-VM main runner thread",
			},
		)),
		runnerThreadPanics: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_runner_thread_panics_total",
				Help: "Number of panics from autoscaler-agent per-VM runner threads",
			},
		)),
		runnerStarts: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_runner_starts",
				Help: "Number of new per-VM Runners started",
			},
		)),
		runnerRestarts: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_runner_restarts",
				Help: "Number of existing per-VM Runners restarted due to failure",
			},
		)),
		runnerNextActions: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_runner_next_actions_total",
				Help: "Number of times (*core.State).NextActions() has been called",
			},
		)),

		scalingLatency: *util.RegisterMetric(reg, prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "autoscaling_agent_scaling_latency_seconds",
				Help:    "End-to-end scaling latency",
				Buckets: buckets,
			},
			revsource.AllFlagNames,
		)),
		pluginLatency: *util.RegisterMetric(reg, prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "autoscaling_agent_plugin_latency_seconds",
				Help:    "Plugin request latency",
				Buckets: buckets,
			},
			revsource.AllFlagNames,
		)),
		monitorLatency: *util.RegisterMetric(reg, prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "autoscaling_agent_monitor_latency_seconds",
				Help:    "Monitor request latency",
				Buckets: buckets,
			},
			revsource.AllFlagNames,
		)),
		neonvmLatency: *util.RegisterMetric(reg, prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "autoscaling_agent_neonvm_latency_seconds",
				Help:    "NeonVM request latency",
				Buckets: buckets,
			},
			revsource.AllFlagNames,
		)),
	}

	// Some of of the metrics should have default keys set to zero. Otherwise, these won't be filled
	// unil the value is non-zero (because something's happened), which makes it harder to
	// distinguish between "valid signal of nothing" vs "no signal".
	metricsWithDirection := []resourceChangePair{
		// scheduler:
		metrics.schedulerRequestedChange,
		metrics.schedulerApprovedChange,
		// monitor:
		metrics.monitorRequestedChange,
		metrics.monitorApprovedChange,
		// neonvm:
		metrics.neonvmRequestedChange,
	}
	for _, p := range metricsWithDirection {
		for _, m := range []*prometheus.CounterVec{p.cpu, p.mem} {
			m.WithLabelValues(directionValueInc).Add(0.0)
			m.WithLabelValues(directionValueDec).Add(0.0)
		}
	}

	runnerStates := []runnerMetricState{
		runnerMetricStateOk,
		runnerMetricStateStuck,
		runnerMetricStateErrored,
		runnerMetricStatePanicked,
	}
	for _, s := range runnerStates {
		metrics.runnersCount.WithLabelValues("true", string(s)).Set(0.0)
		metrics.runnersCount.WithLabelValues("false", string(s)).Set(0.0)
	}

	return metrics, reg
}

type PerVMMetrics struct {
	cpu          *prometheus.GaugeVec
	memory       *prometheus.GaugeVec
	restartCount *prometheus.GaugeVec
}

type vmResourceValueType string

const (
	vmResourceValueSpecMin        vmResourceValueType = "spec_min"
	vmResourceValueAutoscalingMin vmResourceValueType = "autoscaling_min"
	vmResourceValueSpecUse        vmResourceValueType = "spec_use"
	vmResourceValueStatusUse      vmResourceValueType = "status_use"
	vmResourceValueSpecMax        vmResourceValueType = "spec_max"
	vmResourceValueAutoscalingMax vmResourceValueType = "autoscaling_max"
)

func makePerVMMetrics() (PerVMMetrics, *prometheus.Registry) {
	reg := prometheus.NewRegistry()

	metrics := PerVMMetrics{
		cpu: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_vm_cpu_cores",
				Help: "Number of CPUs for a VM: min, max, spec using, or status using",
			},
			[]string{
				"vm_namespace", // .metadata.namespace
				"vm_name",      // .metadata.name
				"endpoint_id",  // .metadata.labels["neon/endpoint-id"]
				"project_id",   // .metadata.labels["neon/project-id"]
				"value",        // vmResourceValue: min, spec_use, status_use, max
			},
		)),
		memory: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_vm_memory_bytes",
				Help: "Amount of memory in bytes for a VM: min, max, spec using, or status using",
			},
			[]string{
				"vm_namespace", // .metadata.namespace
				"vm_name",      // .metadata.name
				"endpoint_id",  // .metadata.labels["neon/endpoint-id"]
				"project_id",   // .metadata.labels["neon/project-id"]
				"value",        // vmResourceValue: min, spec_use, status_use, max
			},
		)),
		restartCount: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "autoscaling_vm_restart_count",
				Help: "Number of times that the VM has restarted",
			},
			[]string{
				"vm_namespace", // .metadata.namespace
				"vm_name",      // .metadata.name
				"endpoint_id",  // .metadata.labels["neon/endpoint-id"]
				"project_id",   // .metadata.labels["neon/project-id"]
			},
		)),
	}

	return metrics, reg
}

func makePerVMMetricsLabels(namespace string, vmName string, endpointID string, projectID string, valueType vmResourceValueType) prometheus.Labels {
	labels := prometheus.Labels{
		"vm_namespace": namespace,
		"vm_name":      vmName,
		"endpoint_id":  endpointID,
		"project_id":   projectID,
	}
	if len(valueType) > 0 {
		labels["value"] = string(valueType)
	}
	return labels
}

// vmMetric is a data object that represents a single metric
// (either CPU or memory) for a VM.
type vmMetric struct {
	labels prometheus.Labels
	value  float64
}
