package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	klog "k8s.io/klog/v2"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type Runner struct {
	podName        api.PodName
	schedulerIP    net.IP
	vmClient       *vmclient.Clientset
	vmName         string
	metricsURL     *url.URL
	vm             api.VmInfo
	servePort      uint16
	readinessPort  uint16
	politeExitPort uint16
}

func NewRunner(args Args, schedulerIP string, vmClient *vmclient.Clientset) (*Runner, error) {
	parsedSchedulerIP := net.ParseIP(schedulerIP)
	if parsedSchedulerIP == nil {
		return nil, fmt.Errorf("Invalid scheduler IP %q", schedulerIP)
	}

	if args.VmInfo.Cpu.Use == 0 {
		// note: this is guaranteed by api.ExtractVmInfo; we're just double-checking here.
		panic("expected init vCPU use >= 1")
	}

	return &Runner{
		podName:        api.PodName{Name: args.K8sPodName, Namespace: args.K8sPodNamespace},
		schedulerIP:    parsedSchedulerIP,
		vmClient:       vmClient,
		vmName:         args.VmName,
		metricsURL:     args.MetricsURL,
		vm:             *args.VmInfo,
		readinessPort:  args.ReadinessPort,
		politeExitPort: args.PoliteExitPort,
	}, nil
}

func (r *Runner) MainLoop(config *Config, ctx context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	if r.politeExitPort != 0 {
		go r.cancelOnPoliteExit(cancel)
	} else {
		klog.V(1).Info("Skipping polite exit server because port is set to zero")
		defer cancel() // make sure cancel() still gets called
	}

	var gotMetricsYet atomic.Bool
	if r.readinessPort != 0 {
		go r.signalReadiness(&gotMetricsYet)
	} else {
		klog.V(1).Info("Skipping readiness server because port is set to zero")
	}

	metrics := make(chan api.Metrics)
	go r.getMetricsLoop(config, metrics)

	klog.Infof("Starting main loop. vCPU = %+v, memSlots = %+v", r.vm.Cpu, r.vm.Mem)

	var computeUnit *api.Resources

	for {
		select {
		// Simply exit if we're done
		case <-ctx.Done():
			return nil
		case m := <-metrics:
			// If we haven't yet sent the scheduler any metrics, we need to send an initial message
			// to be informed of compute unit size
			if computeUnit == nil {
				req := api.AgentRequest{
					Pod: r.podName,
					Resources: api.Resources{
						VCPU: r.vm.Cpu.Use,
						Mem:  r.vm.Mem.Use,
					},
					Metrics: m,
				}
				resp, err := r.sendRequestToPlugin(config, &req)
				if err != nil {
					klog.Errorf("Error from initial plugin message %v", err)
					goto badScheduler // assume something's permanently wrong with the scheduler
				} else if err := resp.ComputeUnit.ValidateNonZero(); err != nil {
					klog.Errorf(
						"Initial plugin response gave bad compute unit %+v: %v",
						resp.ComputeUnit, err,
					)
					goto badScheduler
				}

				computeUnit = &resp.ComputeUnit

				if resp.Permit.VCPU != r.vm.Cpu.Use || resp.Permit.Mem != r.vm.Mem.Use {
					klog.Errorf("Initial plugin response gave bad permit %+v", resp.Permit)
					goto badScheduler
				}

				if resp.Migrate != nil {
					klog.Infof("Scheduler responded to initial message with migration, exiting without further changes.")
					return nil
				}
			}

			// Determine the minimum number of compute units to fit the current vCPU and memory,
			// then use that for "inertia" when considering scaling. We don't want to naively
			// decrease if one resource is only somewhat underutilized compared to the other.
			minComputeUnits := r.minComputeUnits(*computeUnit)
			inertiaCpu := minComputeUnits * computeUnit.VCPU

			newRes := api.Resources{VCPU: r.newGoalCPUCount(m, inertiaCpu)}
			newRes.Mem = memSlotsForCpu(*computeUnit, newRes.VCPU)

			changedVCPU := newRes.VCPU != r.vm.Cpu.Use
			changedMem := newRes.Mem != r.vm.Mem.Use

			// Log the current goal
			{
				descriptor := func(changed bool) string {
					if changed {
						return "new"
					} else {
						return "unchanged"
					}
				}

				klog.Infof(
					"Goal vCPU = %d (%s), Goal mem slots = %d (%s)",
					newRes.VCPU, descriptor(changedVCPU), newRes.Mem, descriptor(changedMem),
				)
			}

			// If either new goal amount is less than the current, decrease immediately. However, we
			// need to be careful not make changes for any resource that is *not* decreasing.
			doChangesBeforeRequest := false
			immediateChanges := api.Resources{
				VCPU: r.vm.Cpu.Use,
				Mem:  r.vm.Mem.Use,
			}

			if newRes.VCPU < r.vm.Cpu.Use {
				klog.Infof(
					"Goal vCPU %d < Current vCPU %d, decrease immediately",
					newRes.VCPU, r.vm.Cpu.Use,
				)
				immediateChanges.VCPU = newRes.VCPU
				doChangesBeforeRequest = true
			}
			if newRes.Mem < r.vm.Mem.Use {
				klog.Infof(
					"Goal mem slots %d < Current mem slots %d, decrease immediately",
					newRes.Mem, r.vm.Mem.Use,
				)
				immediateChanges.Mem = newRes.Mem
				doChangesBeforeRequest = true
			}

			if doChangesBeforeRequest {
				if err := r.setResources(ctx, config, newRes); err != nil {
					return fmt.Errorf("Error while setting VM resources: %s", err)
				}
				r.vm.Cpu.Use = newRes.VCPU
				r.vm.Mem.Use = newRes.Mem
			}

			// With pre-request changes out of the way, let's send our request.
			req := api.AgentRequest{
				Pod:       r.podName,
				Resources: newRes,
				Metrics:   m,
			}
			resp, err := r.sendRequestToPlugin(config, &req)
			if err != nil {
				klog.Errorf("Error from resource request: %s", err)
				goto badScheduler // assume something's permanently wrong with the scheduler
			} else if err = resp.ComputeUnit.ValidateNonZero(); err != nil {
				klog.Errorf("Plugin gave bad compute unit %+v: %v", resp.ComputeUnit, err)
				goto badScheduler
			}

			computeUnit = &resp.ComputeUnit

			if resp.Migrate != nil {
				klog.Infof("Scheduler responded with migration, exiting without further changes.")
				return nil
			}

			if !changedVCPU && !changedMem {
				continue
			}

			// We made our request. Now we handle any remaining increases.
			doChangesAfterRequest := false
			schedulerGaveBadPermit := false

			discontinueMsg := "Discontinuing further contact after updating resources"

			if resp.Permit.VCPU != r.vm.Cpu.Use {
				if resp.Permit.VCPU < r.vm.Cpu.Use {
					// We shouldn't reach this, because req.VCPUs < r.vm.Cpu.Use is already handled
					// above, after which r.vm.Cpu.Use == req.VCPUs, so that would mean that the
					// permit caused a decrease beyond what we're expecting.
					klog.Errorf("Scheduler gave bad permit less than current vCPU. %s", discontinueMsg)
					schedulerGaveBadPermit = true
				} else if resp.Permit.VCPU > req.Resources.VCPU {
					// Permits given by the scheduler should never be greater than what's requested,
					// and doing so indicates that something went very wrong.
					klog.Errorf("Scheduler gave bad permit greater than requested vCPU. %s", discontinueMsg)
					schedulerGaveBadPermit = true
				}

				maybeLessThanDesired := ""
				if resp.Permit.VCPU < newRes.VCPU {
					maybeLessThanDesired = " (less than desired)"
				}
				klog.Infof("Setting vCPU = %d%s", resp.Permit.VCPU, maybeLessThanDesired)
				newRes.VCPU = resp.Permit.VCPU
				doChangesAfterRequest = true

			} else /* resp.Permit.VCPU == r.vm.Cpu.Use */ {
				if r.vm.Cpu.Use == newRes.VCPU {
					// We actually already set the CPU for this (see above), the returned permit
					// just confirmed that
					if changedVCPU {
						klog.Infof("Scheduler confirmed decrease to %d vCPU", newRes.VCPU)
					}
				} else /* resp.Permit.VCPU == r.vm.Cpu.Use && resp.Permit.VCPU != newRes.VCPU */ {
					// We wanted to increase vCPUs, but the scheduler didn't allow it.
					klog.Infof("Scheduler denied increase to %d vCPU, staying at %d", newRes.VCPU, r.vm.Cpu.Use)
				}
			}

			// Comments for memory are omitted; it's essentially the same as CPU handling
			if resp.Permit.Mem != r.vm.Mem.Use {
				if resp.Permit.Mem < r.vm.Mem.Use {
					klog.Errorf("Scheduler gave bad permit less than current memory slots. %s", discontinueMsg)
					schedulerGaveBadPermit = true
				} else if resp.Permit.Mem > req.Resources.Mem {
					klog.Errorf("Scheduler gave bad permit greater than requested memory slots. %s", discontinueMsg)
					schedulerGaveBadPermit = true
				}

				maybeLessThanDesired := ""
				if resp.Permit.Mem < newRes.Mem {
					maybeLessThanDesired = " (less than desired)"
				}
				klog.Infof("Setting memory slots = %d%s", resp.Permit.Mem, maybeLessThanDesired)
				newRes.Mem = resp.Permit.Mem
				doChangesAfterRequest = true
			} else /* resp.Permit.Mem == r.vm.Mem.Use */ {
				if r.vm.Mem.Use == newRes.Mem {
					if changedMem {
						klog.Infof("Scheduler confirmed decrease to %d memory slots", newRes.Mem)
					}
				} else {
					klog.Infof(
						"Scheduler denied increase to %d memory slots, staying at %d",
						newRes.Mem, r.vm.Mem.Use,
					)
				}
			}

			if doChangesAfterRequest {
				if err := r.setResources(ctx, config, newRes); err != nil {
					return fmt.Errorf("Error while setting vCPU count: %s", err)
				}

				r.vm.Cpu.Use = newRes.VCPU
				r.vm.Mem.Use = newRes.Mem

				if schedulerGaveBadPermit {
					goto badScheduler
				}
			}
		}
	}

	// Error-state handling: something went wrong, so we won't allow any more CPU changes. Our logic
	// up to now means our current state is fine (even if it's not the minimum)
badScheduler:
	// We know the scheduler (if it is somehow behaving correctly) won't over-commit resources into
	// our current resource usage
	maxFuture := api.Resources{VCPU: r.vm.Cpu.Use, Mem: r.vm.Mem.Use}

	klog.Warning("Ignoring all future requests from scheduler because of bad request")
	klog.Infof("Future resource limits set at current = %+v", maxFuture)

	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-metrics:
			// We aren't allowed to communicate with the scheduling plugin, so we're upper-bounded
			// by maxFutureVCPU, which was determined by our state at the time the scheduler failed.
			newCpuCount := r.newGoalCPUCount(m, r.vm.Cpu.Use)

			// Bound by our artificial maximum
			if newCpuCount > maxFuture.VCPU {
				klog.Infof(
					"Want to scale to %d vCPUs, but capped at %d vCPUs because scheduler failed",
					newCpuCount, maxFuture.VCPU,
				)
				newCpuCount = maxFuture.VCPU
			}
			// Because we're not communicating with the scheduler, we can change the CPU
			// immediately. We know this value is ok because it's bounded by maxFutureVCPU.
			if newCpuCount != r.vm.Cpu.Use {
				// But first, figure out our memory usage.
				var newMemSlotsCount uint16
				if computeUnit != nil {
					newMemSlotsCount = memSlotsForCpu(*computeUnit, newCpuCount)

					// It's possible for our desired memory slots to be greater than our future
					// maximum, in cases where our resources weren't aligned to a compute unit when
					// we abandoned the scheduler.
					if newMemSlotsCount > maxFuture.Mem {
						klog.Infof(
							"Want to scale to %d mem slots (to match %d vCPU) but capped at %d mem slots because scheduler failed",
							newMemSlotsCount, newCpuCount, maxFuture.Mem,
						)
						newMemSlotsCount = maxFuture.Mem
					}
				} else {
					newMemSlotsCount = r.vm.Mem.Use
					klog.Warningf("Cannot determine new memory slots count because we never received ")
				}

				klog.Infof("Setting vCPU = %d, memSlots = %d", newCpuCount, newMemSlotsCount)

				resources := api.Resources{VCPU: newCpuCount, Mem: newMemSlotsCount}
				if err := r.setResources(ctx, config, resources); err != nil {
					return fmt.Errorf("Error while setting CPU count: %s", err)
				}
				r.vm.Cpu.Use = newCpuCount
			}
		}
	}
}

// minComputeUnits returns the minimum number of compute units it would take to fit the current
// resource allocations
func (r *Runner) minComputeUnits(computeUnit api.Resources) uint16 {
	// (x + M-1) / M is equivalent to x/M rounded up, as long as M != 0, which is guaranteed for
	// compute units.
	cpuUnits := (r.vm.Cpu.Use + computeUnit.VCPU - 1) / computeUnit.VCPU
	memUnits := (r.vm.Mem.Use + computeUnit.Mem - 1) / computeUnit.Mem
	if cpuUnits < memUnits {
		return cpuUnits
	} else {
		return memUnits
	}
}

// Calculates a new target CPU count, bounded ONLY by the minimum and maximum vCPUs
//
// It is the caller's responsibility to make sure that they don't over-commit beyond what the
// scheduler plugin has permitted.
func (r *Runner) newGoalCPUCount(metrics api.Metrics, currentCpu uint16) uint16 {
	goal := currentCpu
	if metrics.LoadAverage1Min > 0.9*float32(r.vm.Cpu.Use) {
		goal *= 2
	} else if metrics.LoadAverage1Min < 0.4*float32(r.vm.Cpu.Use) {
		goal /= 2
	}

	// bound goal by min and max
	if goal < r.vm.Cpu.Min {
		goal = r.vm.Cpu.Min
	} else if goal > r.vm.Cpu.Max {
		goal = r.vm.Cpu.Max
	}

	return goal
}

func roundCpuToComputeUnit(computeUnit api.Resources, cpu uint16) uint16 {
	// TODO: currently we always round up. We can do better, with a little context from which way
	// we're changing the CPU.
	if cpu%computeUnit.VCPU != 0 {
		cpu += computeUnit.VCPU - cpu%computeUnit.VCPU
	}

	return cpu
}

func memSlotsForCpu(computeUnit api.Resources, cpu uint16) uint16 {
	if cpu%computeUnit.VCPU != 0 {
		klog.Warningf(
			"vCPU %d is not a multiple of the compute unit's CPU (%d), using approximate ratio for memory",
			cpu, computeUnit.VCPU,
		)

		ratio := float64(computeUnit.Mem) / float64(computeUnit.VCPU)
		mem := uint16(math.Round(ratio * float64(cpu)))
		return mem
	}

	units := cpu / computeUnit.VCPU
	mem := units * computeUnit.Mem
	return mem
}

// Helper function to abbreviate http server creation
func listenAndServe(addr, routePattern string, handler func(http.ResponseWriter, *http.Request)) error {
	mux := http.NewServeMux()
	mux.HandleFunc(routePattern, handler)
	server := http.Server{Addr: addr, Handler: mux}
	return server.ListenAndServe()
}

func (r *Runner) cancelOnPoliteExit(cancelFunc func()) {
	klog.Fatalf("Polite exit server failed: %s", listenAndServe(
		fmt.Sprintf("0.0.0.0:%d", r.politeExitPort), "/",
		func(w http.ResponseWriter, r *http.Request) {
			klog.Info("Received polite exit request")
			w.WriteHeader(200)
			w.Write([]byte("ok"))
			cancelFunc() // TODO: will this cause an exit before the response is written?
		},
	))
}

func (r *Runner) signalReadiness(gotMetrics *atomic.Bool) {
	klog.Infof("Starting readiness server (port %d)", r.readinessPort)

	klog.Fatalf("Readiness server failed: %s", listenAndServe(
		fmt.Sprintf("0.0.0.0:%d", r.readinessPort), "/healthz",
		func(w http.ResponseWriter, r *http.Request) {
			if gotMetrics.Load() {
				w.WriteHeader(200)
				w.Write([]byte("ok"))
			} else {
				w.WriteHeader(503)
				w.Write([]byte(fmt.Sprintf("error: haven't received metrics yet")))
			}
		},
	))
}

func (r *Runner) getMetricsLoop(config *Config, metrics chan<- api.Metrics) {
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			err := fmt.Errorf("Unexpected redirect while getting metrics")
			klog.Warning(err)
			return err
		},
		Timeout: time.Second * time.Duration(config.Metrics.RequestTimeoutSeconds),
	}

	req := http.Request{
		Method: "GET",
		URL:    r.metricsURL,
		// Allow reusing the TCP connection across multiple requests. Already false by default, but
		// worth setting explicitly.
		Close: false,
	}

	klog.Infof("Starting metrics loop. Timeout = %f seconds", client.Timeout.Seconds())

	// Helper to track whether we've gotten any responses yet. If we have, failed requests are
	// treated more seriously
	gotAtLeastOne := false

	time.Sleep(time.Second * time.Duration(config.Metrics.InitialDelaySeconds))

	for {
		// Wrap the loop body in a function so that we can defer inside it
		func() {
			resp, err := client.Do(&req)
			if err != nil {
				err = fmt.Errorf("Error getting metrics: %s", err)
				if !gotAtLeastOne {
					klog.Warning(err)
				} else {
					klog.Error(err)
				}
				return
			}
			defer resp.Body.Close()

			gotAtLeastOne = true
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				klog.Errorf("Error while reading metrics response: %s", err)
				return
			}

			m, err := api.ReadMetrics(body)
			if err != nil {
				klog.Errorf("Error reading metrics from node_exporter output: %s", err)
				return
			}

			klog.Infof("Processed metrics from VM: %+v", m)
			metrics <- m
		}()

		time.Sleep(time.Second * time.Duration(config.Metrics.SecondsBetweenRequests))
	}
}

func (r *Runner) sendRequestToPlugin(config *Config, req *api.AgentRequest) (*api.PluginResponse, error) {
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			err := fmt.Errorf("Unexpected redirect while sending message to plugin")
			klog.Warning(err)
			return err
		},
		Timeout: time.Second * time.Duration(config.Scheduler.RequestTimeoutSeconds),
	}

	requestBody, err := json.Marshal(req)
	if err != nil {
		klog.Fatalf("Error encoding scheduer request into JSON: %s", err)
	}

	klog.Infof("Sending AgentRequest: %+v", req)

	url := fmt.Sprintf("http://%s:%d/", r.schedulerIP.String(), config.Scheduler.RequestPort)
	resp, err := client.Post(url, "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("Error sending scheduler request: %s", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Error reading body for response: %s", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Received response status %d body %q", resp.StatusCode, string(body))
	}

	var respMsg api.PluginResponse
	if err := json.Unmarshal(body, &respMsg); err != nil {
		return nil, fmt.Errorf("Bad JSON response: %s", err)
	}

	klog.Infof("Received PluginResponse: %+v", respMsg)
	return &respMsg, nil
}

func (r *Runner) setResources(ctx context.Context, config *Config, resources api.Resources) error {
	patches := []util.JSONPatch{{
		Op:    util.PatchReplace,
		Path:  "/spec/guest/cpus/use",
		Value: resources.VCPU,
	}, {
		Op:    util.PatchReplace,
		Path:  "/spec/guest/memorySlots/use",
		Value: resources.Mem,
	}}
	patchPayload, err := json.Marshal(patches)
	if err != nil {
		return fmt.Errorf("Error marshalling JSON patch: %w", err)
	}

	timeout := time.Second * time.Duration(config.Scaling.RequestTimeoutSeconds)
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	_, err = r.vmClient.NeonvmV1().VirtualMachines(r.podName.Namespace).
		Patch(requestCtx, r.vmName, ktypes.JSONPatchType, patchPayload, metav1.PatchOptions{})

	if err != nil {
		return fmt.Errorf("Error making VM patch request: %w", err)
	}

	return nil
}
