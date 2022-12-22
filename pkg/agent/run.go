package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type runner struct {
	config     *Config
	vmClient   *vmclient.Clientset
	kubeClient *kubernetes.Clientset

	// note: only vm.Name and vm.Namespace are expected to be set before calling Spawn or Run. The
	// rest will be determined by an initial request to get information about the VM.
	vm      *api.VmInfo
	podName api.PodName
	podIP   string

	stop    util.SignalReceiver
	deleted util.SignalReceiver
}

func (r runner) Spawn(ctx context.Context, status *podStatus) {
	go func() {
		// Gracefully handle panics:
		defer func() {
			if err := recover(); err != nil {
				status.lock.Lock()
				defer status.lock.Unlock() // defer inside defer? sure!

				status.panicked = true
				status.done = true
				status.errored = fmt.Errorf("%s", fmt.Sprint("runner panicked:", err))
			}
		}()

		logger := RunnerLogger{prefix: fmt.Sprintf("Runner %v: ", r.podName)}

		migrating, err := r.Run(ctx, logger)

		status.lock.Lock()
		defer status.lock.Unlock()

		status.done = true
		status.migrating = migrating
		status.errored = err

		if err != nil {
			logger.Errorf("Ended with error: %s", err)
		} else {
			logger.Infof("Ended without error")
		}
	}()
}

// getInitialVMInfo fetches and returns the VmInfo for the VM as described by r.vm.{Name,Namespace}
//
// This method returns (nil, nil) if the VM does not exist, which may occur due to inherently racy
// behavior -- by the time this method is called, the pod's creation event might have been
// superseded by the VM's deletion.
func (r runner) getInitialVMInfo(ctx context.Context) (*api.VmInfo, error) {
	// In order to smoothly handle cases where the VM is missing, we perform a List request instead
	// of a Get, with a FieldSelector that limits the result just to the target VM, if it exists.

	opts := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", r.vm.Name),
	}
	list, err := r.vmClient.NeonvmV1().VirtualMachines(r.vm.Namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("Error listing VM %s:%s: %w", r.vm.Namespace, r.vm.Name, err)
	}

	if len(list.Items) > 1 {
		return nil, fmt.Errorf("List for VM %s:%s returned > 1 item", r.vm.Namespace, r.vm.Name)
	} else if len(list.Items) == 0 {
		return nil, nil
	}

	vmInfo, err := api.ExtractVmInfo(&list.Items[0])
	if err != nil {
		return nil, fmt.Errorf("Error extracting VmInfo from %s:%s: %w", r.vm.Name, r.vm.Namespace, err)
	}

	return vmInfo, nil
}

type RunnerLogger struct {
	prefix string
}

func (l RunnerLogger) Infof(format string, args ...interface{}) {
	klog.InfofDepth(1, l.prefix+format, args...)
}

func (l RunnerLogger) Warningf(format string, args ...interface{}) {
	klog.WarningfDepth(1, l.prefix+format, args...)
}

func (l RunnerLogger) Errorf(format string, args ...interface{}) {
	klog.ErrorfDepth(1, l.prefix+format, args...)
}

func (l RunnerLogger) Fatalf(format string, args ...interface{}) {
	klog.FatalfDepth(1, l.prefix+format, args...)
}

func (r runner) Run(ctx context.Context, logger RunnerLogger) (migrating bool, _ error) {
	ctx, cancelCtx := context.WithCancel(ctx)
	defer cancelCtx() // Make sure that background tasks are cleaned up.

	initVmInfo, err := r.getInitialVMInfo(ctx)
	if err != nil {
		return false, err
	} else if initVmInfo == nil {
		logger.Warningf(
			"Could not find VM %s:%s, maybe it was already deleted?",
			r.vm.Namespace, r.vm.Name,
		)
		return false, nil
	}

	r.vm = initVmInfo
	initVmInfo = nil // clear to allow GC.

	metrics := make(chan api.Metrics)
	metricsPanicked := make(chan struct{})
	go r.getMetricsLoop(ctx, logger, r.config, metrics, metricsPanicked)

	var computeUnit *api.Resources

	logger.Infof("Starting scheduler watcher and getting initial scheduler")
	schedulerWatch, scheduler, err := watchSchedulerUpdates(ctx, r.kubeClient, r.config.Scheduler.SchedulerName)
	if err != nil {
		return false, fmt.Errorf("Error starting scheduler watcher: %w", err)
	} else if scheduler == nil {
		logger.Infof("No initial scheduler found, waiting for one to become ready")
		goto noSchedulerLoop
	}

	logger.Infof(
		"Got initial scheduler pod %v (UID = %v) with IP %v",
		scheduler.podName, scheduler.uid, scheduler.ip,
	)
	schedulerWatch.Using(*scheduler)

	logger.Infof("Starting main loop. vCPU = %+v, memSlots = %+v", r.vm.Cpu, r.vm.Mem)

restartConnection:

	schedulerWatch.ExpectingDeleted()

	for {
		select {
		// Simply exit if we're done
		case <-r.stop.Recv():
			logger.Infof("Ending runner loop, received stop signal")
			return false, nil
		// and also exit if the pod was deleted
		case <-r.deleted.Recv():
			logger.Infof("Ending runner loop for VM, pod was deleted")
			return false, nil
		case <-metricsPanicked:
			return false, fmt.Errorf("Metrics loop panicked")
		case info := <-schedulerWatch.Deleted:
			if info.uid != scheduler.uid {
				logger.Infof(
					"Received info that scheduler candidate pod %v was deleted, but we aren't using it (it's UID = %v, ours = %v)",
					info.podName, info.uid, scheduler.uid,
				)
				continue
			}
			logger.Warningf(
				"Received info that scheduler pod %v (UID = %v) was deleted, ending further communication",
				scheduler.podName, scheduler.uid,
			)
			goto noSchedulerLoop
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
				resp, err := r.sendRequestToPlugin(logger, scheduler, r.config, &req)
				if err != nil {
					logger.Errorf("Error from initial plugin message %s", err)
					goto badScheduler // assume something's permanently wrong with the scheduler
				} else if err := resp.ComputeUnit.ValidateNonZero(); err != nil {
					logger.Errorf(
						"Initial plugin response gave bad compute unit %+v: %v",
						resp.ComputeUnit, err,
					)
					goto badScheduler
				}

				computeUnit = &resp.ComputeUnit

				if resp.Permit.VCPU != r.vm.Cpu.Use || resp.Permit.Mem != r.vm.Mem.Use {
					logger.Errorf("Initial plugin response gave bad permit %+v", resp.Permit)
					goto badScheduler
				}

				if resp.Migrate != nil {
					logger.Infof("Scheduler responded to initial message with migration, exiting without further changes.")
					return true, nil
				}
			}

			// Determine the minimum number of compute units to fit the current vCPU and memory,
			// then use that for "inertia" when considering scaling. We don't want to naively
			// decrease if one resource is only somewhat underutilized compared to the other.
			minComputeUnits := r.minComputeUnits(*computeUnit)
			inertiaCpu := minComputeUnits * computeUnit.VCPU

			newRes := api.Resources{VCPU: r.newGoalCPUCount(m, inertiaCpu)}
			newRes.Mem = memSlotsForCpu(logger, *computeUnit, newRes.VCPU)

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

				logger.Infof(
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
				logger.Infof(
					"Goal vCPU %d < Current vCPU %d, decrease immediately",
					newRes.VCPU, r.vm.Cpu.Use,
				)
				immediateChanges.VCPU = newRes.VCPU
				doChangesBeforeRequest = true
			}
			if newRes.Mem < r.vm.Mem.Use {
				logger.Infof(
					"Goal mem slots %d < Current mem slots %d, decrease immediately",
					newRes.Mem, r.vm.Mem.Use,
				)
				immediateChanges.Mem = newRes.Mem
				doChangesBeforeRequest = true
			}

			if doChangesBeforeRequest {
				if err := r.setResources(ctx, r.config, newRes); err != nil {
					// FIXME: maybe we should retry on failure?
					return false, fmt.Errorf("Error while setting VM resources: %s", err)
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
			resp, err := r.sendRequestToPlugin(logger, scheduler, r.config, &req)
			if err != nil {
				logger.Errorf("Error from resource request: %s", err)
				goto badScheduler // assume something's permanently wrong with the scheduler
			} else if err = resp.ComputeUnit.ValidateNonZero(); err != nil {
				logger.Errorf("Plugin gave bad compute unit %+v: %v", resp.ComputeUnit, err)
				goto badScheduler
			}

			computeUnit = &resp.ComputeUnit

			if resp.Migrate != nil {
				logger.Infof("Scheduler responded with migration, exiting without further changes.")
				return true, nil
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
					logger.Errorf("Scheduler gave bad permit less than current vCPU. %s", discontinueMsg)
					schedulerGaveBadPermit = true
				} else if resp.Permit.VCPU > req.Resources.VCPU {
					// Permits given by the scheduler should never be greater than what's requested,
					// and doing so indicates that something went very wrong.
					logger.Errorf("Scheduler gave bad permit greater than requested vCPU. %s", discontinueMsg)
					schedulerGaveBadPermit = true
				}

				maybeLessThanDesired := ""
				if resp.Permit.VCPU < newRes.VCPU {
					maybeLessThanDesired = " (less than desired)"
				}
				logger.Infof("Setting vCPU = %d%s", resp.Permit.VCPU, maybeLessThanDesired)
				newRes.VCPU = resp.Permit.VCPU
				doChangesAfterRequest = true

			} else /* resp.Permit.VCPU == r.vm.Cpu.Use */ {
				if r.vm.Cpu.Use == newRes.VCPU {
					// We actually already set the CPU for this (see above), the returned permit
					// just confirmed that
					if changedVCPU {
						logger.Infof("Scheduler confirmed decrease to %d vCPU", newRes.VCPU)
					}
				} else /* resp.Permit.VCPU == r.vm.Cpu.Use && resp.Permit.VCPU != newRes.VCPU */ {
					// We wanted to increase vCPUs, but the scheduler didn't allow it.
					logger.Infof("Scheduler denied increase to %d vCPU, staying at %d", newRes.VCPU, r.vm.Cpu.Use)
				}
			}

			// Comments for memory are omitted; it's essentially the same as CPU handling
			if resp.Permit.Mem != r.vm.Mem.Use {
				if resp.Permit.Mem < r.vm.Mem.Use {
					logger.Errorf("Scheduler gave bad permit less than current memory slots. %s", discontinueMsg)
					schedulerGaveBadPermit = true
				} else if resp.Permit.Mem > req.Resources.Mem {
					logger.Errorf("Scheduler gave bad permit greater than requested memory slots. %s", discontinueMsg)
					schedulerGaveBadPermit = true
				}

				maybeLessThanDesired := ""
				if resp.Permit.Mem < newRes.Mem {
					maybeLessThanDesired = " (less than desired)"
				}
				logger.Infof("Setting memory slots = %d%s", resp.Permit.Mem, maybeLessThanDesired)
				newRes.Mem = resp.Permit.Mem
				doChangesAfterRequest = true
			} else /* resp.Permit.Mem == r.vm.Mem.Use */ {
				if r.vm.Mem.Use == newRes.Mem {
					if changedMem {
						logger.Infof("Scheduler confirmed decrease to %d memory slots", newRes.Mem)
					}
				} else {
					logger.Infof(
						"Scheduler denied increase to %d memory slots, staying at %d",
						newRes.Mem, r.vm.Mem.Use,
					)
				}
			}

			if doChangesAfterRequest {
				if err := r.setResources(ctx, r.config, newRes); err != nil {
					// FIXME: maybe we should retry on failure?
					return false, fmt.Errorf("Error while setting vCPU count: %s", err)
				}

				r.vm.Cpu.Use = newRes.VCPU
				r.vm.Mem.Use = newRes.Mem

				if schedulerGaveBadPermit {
					goto badScheduler
				}
			}
		}
	}

badScheduler:
	// Error-state handling: something went wrong, so we won't allow any more CPU changes. Our logic
	// up to now means our current state is fine (even if it's not the minimum)
	logger.Warningf("Stopping all future requests to scheduler %v because of bad request", scheduler.podName)
noSchedulerLoop:
	// We know the scheduler (if it is behaving correctly) won't over-commit resources into our
	// current resource usage, so we're safe to use *up to* maxFuture
	maxFuture := api.Resources{VCPU: r.vm.Cpu.Use, Mem: r.vm.Mem.Use}

	logger.Infof("Future resource limits set at current = %+v", maxFuture)

	schedulerWatch.ExpectingReady()

	for {
		select {
		case <-r.stop.Recv():
			logger.Infof("Ending runner loop, received stop signal")
			return false, nil
		case <-r.deleted.Recv():
			logger.Infof("Ending runner loop, pod was deleted")
			return false, nil
		case info := <-schedulerWatch.ReadyQueue:
			logger.Infof("Retrying with new ready scheduler pod %v with IP %v...", info.podName, info.ip)
			scheduler = &info
			computeUnit = nil
			goto restartConnection
		case m := <-metrics:
			// We aren't allowed to communicate with the scheduling plugin, so we're upper-bounded
			// by maxFutureVCPU, which was determined by our state at the time the scheduler failed.
			newCpuCount := r.newGoalCPUCount(m, r.vm.Cpu.Use)

			// Bound by our artificial maximum
			if newCpuCount > maxFuture.VCPU {
				logger.Infof(
					"Want to scale to %d vCPUs, but capped at %d vCPUs because we have no scheduler",
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
					newMemSlotsCount = memSlotsForCpu(logger, *computeUnit, newCpuCount)

					// It's possible for our desired memory slots to be greater than our future
					// maximum, in cases where our resources weren't aligned to a compute unit when
					// we abandoned the scheduler.
					if newMemSlotsCount > maxFuture.Mem {
						logger.Infof(
							"Want to scale to %d mem slots (to match %d vCPU) but capped at %d mem slots because we have no scheduler",
							newMemSlotsCount, newCpuCount, maxFuture.Mem,
						)
						newMemSlotsCount = maxFuture.Mem
					}
				} else {
					newMemSlotsCount = r.vm.Mem.Use
					logger.Warningf("Cannot determine new memory slots count because we never received a computeUnit from scheduler")
				}

				logger.Infof("Setting vCPU = %d, memSlots = %d", newCpuCount, newMemSlotsCount)

				resources := api.Resources{VCPU: newCpuCount, Mem: newMemSlotsCount}
				if err := r.setResources(ctx, r.config, resources); err != nil {
					// FIXME: maybe we should retry on failure?
					return false, fmt.Errorf("Error while setting resources: %s", err)
				}
				r.vm.Cpu.Use = newCpuCount
			}
		}
	}
}

// minComputeUnits returns the minimum number of compute units it would take to fit the current
// resource allocations
func (r *runner) minComputeUnits(computeUnit api.Resources) uint16 {
	// (x + M-1) / M is equivalent to ceil(x/M), as long as M != 0, which is guaranteed for
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
func (r *runner) newGoalCPUCount(metrics api.Metrics, currentCpu uint16) uint16 {
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

func memSlotsForCpu(logger RunnerLogger, computeUnit api.Resources, cpu uint16) uint16 {
	if cpu%computeUnit.VCPU != 0 {
		logger.Warningf(
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

// note: we actually use the context here; we have a deferred cancel on it in Run
func (r *runner) getMetricsLoop(
	ctx context.Context, logger RunnerLogger, config *Config, metrics chan<- api.Metrics, panicked chan<- struct{},
) {
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			err := fmt.Errorf("Unexpected redirect while getting metrics")
			logger.Warningf("%s", err)
			return err
		},
		Timeout: time.Second * time.Duration(config.Metrics.RequestTimeoutSeconds),
	}

	metricsURL, err := url.Parse(fmt.Sprintf("http://%s:9100/metrics", r.podIP))
	if err != nil {
		panic(fmt.Sprintf("Error creating metrics URL: %s", err))
	}

	req := http.Request{
		Method: "GET",
		URL:    metricsURL,
		// Allow reusing the TCP connection across multiple requests. Already false by default, but
		// worth setting explicitly.
		Close: false,
	}

	logger.Infof("Starting metrics loop. Timeout = %f seconds", client.Timeout.Seconds())

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
				if !gotAtLeastOne || ctx.Err() != nil {
					logger.Warningf("%s", err)
				} else {
					logger.Errorf("%s", err)
				}
				return
			}
			defer resp.Body.Close()

			gotAtLeastOne = true
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				logger.Errorf("Error while reading metrics response: %s", err)
				return
			}

			m, err := api.ReadMetrics(body)
			if err != nil {
				logger.Errorf("Error reading metrics from node_exporter output: %s", err)
				return
			}

			logger.Infof("Processed metrics from VM: %+v", m)
			metrics <- m
		}()

		select {
		case <-time.NewTimer(time.Second * time.Duration(config.Metrics.SecondsBetweenRequests)).C:
			// Continue to the next request
		case <-ctx.Done():
			logger.Infof("Ending metrics loop for VM %s:%s", r.vm.Name, r.vm.Namespace)
			// end the metrics loop
			return
		}
	}
}

func (r *runner) sendRequestToPlugin(
	logger RunnerLogger, sched *schedulerInfo, config *Config, req *api.AgentRequest,
) (*api.PluginResponse, error) {
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			err := fmt.Errorf("Unexpected redirect while sending message to plugin")
			logger.Warningf("%s", err)
			return err
		},
		Timeout: time.Second * time.Duration(config.Scheduler.RequestTimeoutSeconds),
	}

	requestBody, err := json.Marshal(req)
	if err != nil {
		logger.Fatalf("Error encoding scheduer request into JSON: %s", err)
	}

	logger.Infof("Sending AgentRequest: %+v", req)

	url := fmt.Sprintf("http://%s:%d/", sched.ip, config.Scheduler.RequestPort)
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

	logger.Infof("Received PluginResponse: %+v", respMsg)
	return &respMsg, nil
}

func (r *runner) setResources(ctx context.Context, config *Config, resources api.Resources) error {
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

	_, err = r.vmClient.NeonvmV1().VirtualMachines(r.vm.Namespace).
		Patch(requestCtx, r.vm.Name, ktypes.JSONPatchType, patchPayload, metav1.PatchOptions{})

	if err != nil {
		return fmt.Errorf("Error making VM patch request: %w", err)
	}

	return nil
}
