package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

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
	cpu    *prometheus.GaugeVec
	memory *prometheus.GaugeVec
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
				"value",        // vmResourceValue: min, spec_use, status_use, max
			},
		)),
	}

	return metrics, reg
}

// vmMetric is a data object that represents a single metric
// (either CPU or memory) for a VM.
type vmMetric struct {
	labels prometheus.Labels
	value  float64
}

func makeLabels(namespace string, vmName string, endpointID string, valueType vmResourceValueType) prometheus.Labels {
	return prometheus.Labels{
		"vm_namespace": namespace,
		"vm_name":      vmName,
		"endpoint_id":  endpointID,
		"value":        string(valueType),
	}
}
