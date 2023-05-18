package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type PromMetrics struct {
	schedulerRequests         *prometheus.CounterVec
	informantRequestsOutbound *prometheus.CounterVec
	informantRequestsInbound  *prometheus.CounterVec
	runnerFatalErrors         prometheus.Counter
	runnerThreadPanics        prometheus.Counter
	runnerStarts              prometheus.Counter
	runnerRestarts            prometheus.Counter
}

func register[P prometheus.Collector](reg *prometheus.Registry, collector P) P {
	reg.MustRegister(collector)
	return collector
}

func makePrometheusParts(globalstate *agentState) (PromMetrics, *prometheus.Registry) {
	var metrics PromMetrics
	reg := prometheus.NewRegistry()

	// register stock collectors directly:
	//   (even though MustRegister is variadic, the function calls
	//   are cheap and calling it more than once means that when
	//   it panics, we know exactly which metric caused the error.)
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// the register() function registers the collector and returns
	// it so we can set it directly on the output structure.
	metrics.schedulerRequests = register(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "autoscaling_agent_scheduler_plugin_requests_total",
				Help: "Number of attempted HTTP requests to the scheduler plugin by autoscaler-agents",
			},
			[]string{"code"},
		))

	metrics.informantRequestsOutbound = register(reg, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "autoscaling_agent_informant_outbound_requests_total",
			Help: "Number of attempted HTTP requests to vm-informants by autoscaler-agents",
		},
		[]string{"code"},
	))

	metrics.informantRequestsInbound = register(reg, prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "autoscaling_agent_informant_inbound_requests_total",
			Help: "Number of HTTP requests from vm-informants received by autoscaler-agents",
		},
		[]string{"endpoint", "code"},
	))
	metrics.runnerFatalErrors = register(reg, prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "autoscaling_agent_runner_fatal_errors_total",
			Help: "Number of fatal errors from autoscaler-agent per-VM main runner thread",
		},
	))

	metrics.runnerThreadPanics = register(reg, prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "autoscaling_agent_runner_thread_panics_total",
			Help: "Number of panics from autoscaler-agent per-VM runner threads",
		},
	))
	metrics.runnerStarts = register(reg, prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "autoscaling_agent_runner_starts",
			Help: "Number of new per-VM Runners started",
		},
	))
	metrics.runnerRestarts = register(reg, prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "autoscaling_agent_runner_restarts",
			Help: "Number of existing per-VM Runners restarted due to failure",
		},
	))

	// the remaining metrics are computed on timers run within
	// prom: register them directly.
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
			Help: "Number of VMs on the autoscaler-agent's node that it's tracking",
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
				if p.status.informantIsUnhealthy(globalstate.config) {
					count++
				}
			}

			return float64(count)
		},
	))

	return metrics, reg
}
