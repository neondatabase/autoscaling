package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type PromMetrics struct {
	schedulerRequests        *prometheus.CounterVec
	schedulerRequestedChange resourceChangePair
	schedulerApprovedChange  resourceChangePair

	informantRequestsOutbound *prometheus.CounterVec
	informantRequestsInbound  *prometheus.CounterVec
	informantRequestedChange  resourceChangePair
	informantApprovedChange   resourceChangePair

	neonvmRequestsOutbound *prometheus.CounterVec
	neonvmRequestedChange  resourceChangePair

	runnerFatalErrors  prometheus.Counter
	runnerThreadPanics prometheus.Counter
	runnerStarts       prometheus.Counter
	runnerRestarts     prometheus.Counter
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

func makePrometheusParts(globalstate *agentState) (PromMetrics, *prometheus.Registry) {
	reg := prometheus.NewRegistry()

	// register stock collectors directly:
	//   (even though MustRegister is variadic, the function calls
	//   are cheap and calling it more than once means that when
	//   it panics, we know exactly which metric caused the error.)
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	metrics := PromMetrics{
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

		// ---- INFORMANT ----
		informantRequestsOutbound: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_informant_outbound_requests_total",
				Help: "Number of attempted HTTP requests to vm-informants by autoscaler-agents",
			},
			[]string{"endpoint", "code"},
		)),
		informantRequestsInbound: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_informant_inbound_requests_total",
				Help: "Number of HTTP requests from vm-informants received by autoscaler-agents",
			},
			[]string{"endpoint", "code"},
		)),
		informantRequestedChange: resourceChangePair{
			cpu: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_informant_requested_cpu_change_total",
					Help: "Total change in CPU requested from the informant(s)",
				},
				[]string{directionLabel},
			)),
			mem: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_informant_requested_mem_change_total",
					Help: "Total change in memory (in MiB) requested from the informant(s)",
				},
				[]string{directionLabel},
			)),
		},
		informantApprovedChange: resourceChangePair{
			cpu: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_informant_approved_cpu_change_total",
					Help: "Total change in CPU approved by the informant(s)",
				},
				[]string{directionLabel},
			)),
			mem: util.RegisterMetric(reg, prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "autoscaling_agent_informant_approved_mem_change_total",
					Help: "Total change in memory (in MiB) approved by the informant(s)",
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
	}

	// Some of of the metrics should have default keys set to zero. Otherwise, these won't be filled
	// unil the value is non-zero (because something's happened), which makes it harder to
	// distinguish between "valid signal of nothing" vs "no signal".
	metricsWithDirection := []resourceChangePair{
		// scheduler:
		metrics.schedulerRequestedChange,
		metrics.schedulerApprovedChange,
		// informant:
		metrics.informantRequestedChange,
		metrics.informantApprovedChange,
		// neonvm:
		metrics.neonvmRequestedChange,
	}
	for _, p := range metricsWithDirection {
		for _, m := range []*prometheus.CounterVec{p.cpu, p.mem} {
			m.WithLabelValues(directionValueInc).Add(0.0)
			m.WithLabelValues(directionValueDec).Add(0.0)
		}
	}

	// the remaining metrics are computed at scrape time by prom:
	// register them directly.
	reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "autoscaling_errored_vm_runners_current",
			Help: "Number of VMs whose per-VM runner has panicked (and not restarted)",
		},
		func() float64 {
			globalstate.lock.Lock()
			defer globalstate.lock.Unlock()

			count := 0

			for _, p := range globalstate.pods {
				func() {
					p.status.mu.Lock()
					defer p.status.mu.Unlock()

					if p.status.endState != nil && p.status.endState.ExitKind == podStatusExitErrored {
						count += 1
					}
				}()
			}

			return float64(count)
		},
	))

	reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "autoscaling_panicked_vm_runners_current",
			Help: "Number of VMs whose per-VM runner has panicked (and not restarted)",
		},
		func() float64 {
			globalstate.lock.Lock()
			defer globalstate.lock.Unlock()

			count := 0

			for _, p := range globalstate.pods {
				func() {
					p.status.mu.Lock()
					defer p.status.mu.Unlock()

					if p.status.endState != nil && p.status.endState.ExitKind == podStatusExitPanicked {
						count += 1
					}
				}()
			}

			return float64(count)
		},
	))

	reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "autoscaling_agent_tracked_vms_current",
			Help: "Number of autoscaling-enabled non-migrating VMs on the autoscaler-agent's node",
		},
		func() float64 {
			globalstate.lock.Lock()
			defer globalstate.lock.Unlock()

			return float64(len(globalstate.pods))
		},
	))

	reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "autoscaling_vms_unsuccessful_communication_with_informant_current",
			Help: "Number of VMs whose vm-informants aren't successfully communicating with the autoscaler-agent",
		},
		func() float64 {
			globalstate.lock.Lock()
			defer globalstate.lock.Unlock()

			count := 0

			for _, p := range globalstate.pods {
				if p.status.informantIsUnhealthy(globalstate.config, unhealthyAny) {
					count++
				}
			}

			return float64(count)
		},
	))

	reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "autoscaling_billed_vms_unsuccessful_communication_with_informant_current",
			Help: "Number of VMs *getting billed* whose vm-informants aren't successfully communicating with the autoscaler-agent",
		},
		func() float64 {
			globalstate.lock.Lock()
			defer globalstate.lock.Unlock()

			count := 0

			for _, p := range globalstate.pods {
				if p.status.informantIsUnhealthy(globalstate.config, unhealthyEndpoint) {
					count++
				}
			}

			return float64(count)
		},
	))

	return metrics, reg
}
