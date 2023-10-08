package agent

// Core glue and logic for a single VM
//
// The primary object in this file is the Runner. We create a new Runner for each VM, and the Runner
// spawns a handful of long-running tasks that share state via the Runner object itself.
//
// Each of these tasks is created by (*Runner).spawnBackgroundWorker(), which gracefully handles
// panics so that it terminates (and restarts) the Runner itself, instead of e.g. taking down the
// entire autoscaler-agent.
//
// The main entrypoint is (*Runner).Spawn(), which in turn calls (*Runner).Run(), etc.
//
// For more information, refer to ARCHITECTURE.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/agent/executor"
	"github.com/neondatabase/autoscaling/pkg/agent/schedwatch"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// PluginProtocolVersion is the current version of the agent<->scheduler plugin in use by this
// autoscaler-agent.
//
// Currently, each autoscaler-agent supports only one version at a time. In the future, this may
// change.
const PluginProtocolVersion api.PluginProtoVersion = api.PluginProtoV2_0

// Runner is per-VM Pod god object responsible for handling everything
//
// It primarily operates as a source of shared data for a number of long-running tasks. For
// additional general information, refer to the comment at the top of this file.
type Runner struct {
	global *agentState
	// status provides the high-level status of the Runner. Reading or updating the status requires
	// holding podStatus.lock. Updates are typically done handled by the setStatus method.
	status *lockedPodStatus

	// shutdown provides a clean way to trigger all background Runner threads to shut down. shutdown
	// is set exactly once, by (*Runner).Run
	shutdown context.CancelFunc

	vmName  util.NamespacedName
	podName util.NamespacedName
	podIP   string

	memSlotSize *resource.Quantity

	// lock guards the values of all mutable fields - namely, scheduler and monitor (which may be
	// read without the lock, but the lock must be acquired to lock them).
	lock util.ChanMutex

	// executorStateDump is set by (*Runner).Run and provides a way to get the state of the
	// "executor"
	executorStateDump func() executor.StateDump

	// scheduler is the current scheduler that we're communicating with, or nil if there isn't one.
	// Each scheduler's info field is immutable. When a scheduler is replaced, only the pointer
	// value here is updated; the original Scheduler remains unchanged.
	//
	// Additionally, this field MAY ONLY be updated while holding both lock AND the executor's lock,
	// which means that it may be read when EITHER holding lock OR the executor's lock.
	scheduler *Scheduler
	// monitor, if non nil, stores the current Dispatcher in use for communicating with the
	// vm-monitor, alongside a generation number.
	//
	// Additionally, this field MAY ONLY be updated while holding both lock AND the executor's lock,
	// which means that it may be read when EITHER holding lock OR the executor's lock.
	monitor *monitorInfo

	// backgroundWorkerCount tracks the current number of background workers. It is exclusively
	// updated by r.spawnBackgroundWorker
	backgroundWorkerCount atomic.Int64
	backgroundPanic       chan error
}

// Scheduler stores relevant state for a particular scheduler that a Runner is (or has) connected to
type Scheduler struct {
	// runner is the parent Runner interacting with this Scheduler instance
	//
	// This field is immutable but the data behind the pointer is not.
	runner *Runner

	// info holds the immutable information we use to connect to and describe the scheduler
	info schedwatch.SchedulerInfo

	generation executor.GenerationNumber
}

// RunnerState is the serializable state of the Runner, extracted by its State method
type RunnerState struct {
	PodIP                 string             `json:"podIP"`
	ExecutorState         executor.StateDump `json:"executorState"`
	Scheduler             *SchedulerState    `json:"scheduler"`
	BackgroundWorkerCount int64              `json:"backgroundWorkerCount"`
}

// SchedulerState is the state of a Scheduler, constructed as part of a Runner's State Method
type SchedulerState struct {
	Info schedwatch.SchedulerInfo `json:"info"`
}

func (r *Runner) State(ctx context.Context) (*RunnerState, error) {
	if err := r.lock.TryLock(ctx); err != nil {
		return nil, err
	}
	defer r.lock.Unlock()

	var scheduler *SchedulerState
	if r.scheduler != nil {
		scheduler = &SchedulerState{
			Info: r.scheduler.info,
		}
	}

	var executorState *executor.StateDump
	if r.executorStateDump != nil /* may be nil if r.Run() hasn't fully started yet */ {
		s := r.executorStateDump()
		executorState = &s
	}

	return &RunnerState{
		PodIP:                 r.podIP,
		ExecutorState:         *executorState,
		Scheduler:             scheduler,
		BackgroundWorkerCount: r.backgroundWorkerCount.Load(),
	}, nil
}

func (r *Runner) Spawn(ctx context.Context, logger *zap.Logger, vmInfoUpdated util.CondChannelReceiver) {
	go func() {
		// Gracefully handle panics, plus trigger restart
		defer func() {
			if err := recover(); err != nil {
				now := time.Now()
				r.status.update(r.global, func(stat podStatus) podStatus {
					stat.endState = &podStatusEndState{
						ExitKind: podStatusExitPanicked,
						Error:    fmt.Errorf("Runner %v panicked: %v", stat.vmInfo.NamespacedName(), err),
						Time:     now,
					}
					return stat
				})
			}

			r.global.TriggerRestartIfNecessary(ctx, logger, r.podName, r.podIP)
		}()

		err := r.Run(ctx, logger, vmInfoUpdated)
		endTime := time.Now()

		exitKind := podStatusExitCanceled // normal exit, only by context being canceled.
		if err != nil {
			exitKind = podStatusExitErrored
			r.global.metrics.runnerFatalErrors.Inc()
		}
		r.status.update(r.global, func(stat podStatus) podStatus {
			stat.endState = &podStatusEndState{
				ExitKind: exitKind,
				Error:    err,
				Time:     endTime,
			}
			return stat
		})

		if err != nil {
			logger.Error("Ended with error", zap.Error(err))
		} else {
			logger.Info("Ended without error")
		}
	}()
}

// Run is the main entrypoint to the long-running per-VM pod tasks
func (r *Runner) Run(ctx context.Context, logger *zap.Logger, vmInfoUpdated util.CondChannelReceiver) error {
	ctx, r.shutdown = context.WithCancel(ctx)
	defer r.shutdown()

	schedulerWatch, scheduler, err := schedwatch.WatchSchedulerUpdates(
		ctx, logger.Named("sched-watch"), r.global.schedulerEventBroker, r.global.schedulerStore,
	)
	if err != nil {
		return fmt.Errorf("Error starting scheduler watcher: %w", err)
	}
	defer schedulerWatch.Stop()

	if scheduler == nil {
		logger.Warn("No initial scheduler found")
	} else {
		logger.Info("Got initial scheduler pod", zap.Object("scheduler", scheduler))
		schedulerWatch.Using(*scheduler)
	}

	getVmInfo := func() api.VmInfo {
		r.status.mu.Lock()
		defer r.status.mu.Unlock()
		return r.status.vmInfo
	}

	execLogger := logger.Named("exec")

	coreExecLogger := execLogger.Named("core")
	executorCore := executor.NewExecutorCore(coreExecLogger, getVmInfo(), executor.Config{
		OnNextActions: r.global.metrics.runnerNextActions.Inc,
		Core: core.Config{
			DefaultScalingConfig:               r.global.config.Scaling.DefaultConfig,
			PluginRequestTick:                  time.Second * time.Duration(r.global.config.Scheduler.RequestAtLeastEverySeconds),
			PluginDeniedRetryWait:              time.Second * time.Duration(r.global.config.Scheduler.RetryDeniedUpscaleSeconds),
			MonitorDeniedDownscaleCooldown:     time.Second * time.Duration(r.global.config.Monitor.RetryDeniedDownscaleSeconds),
			MonitorRequestedUpscaleValidPeriod: time.Second * time.Duration(r.global.config.Monitor.RequestedUpscaleValidSeconds),
			MonitorRetryWait:                   time.Second * time.Duration(r.global.config.Monitor.RetryFailedRequestSeconds),
			Log: core.LogConfig{
				Info: coreExecLogger.Info,
				Warn: coreExecLogger.Warn,
			},
		},
	})

	r.executorStateDump = executorCore.StateDump

	pluginGeneration := executor.NewStoredGenerationNumber()
	monitorGeneration := executor.NewStoredGenerationNumber()

	pluginIface := makePluginInterface(r, executorCore, pluginGeneration)
	neonvmIface := makeNeonVMInterface(r)
	monitorIface := makeMonitorInterface(r, executorCore, monitorGeneration)

	// "ecwc" stands for "ExecutorCoreWithClients"
	ecwc := executorCore.WithClients(executor.ClientSet{
		Plugin:  pluginIface,
		NeonVM:  neonvmIface,
		Monitor: monitorIface,
	})

	logger.Info("Starting background workers")

	// FIXME: make this timeout/delay a separately defined constant, or configurable
	mainDeadlockChecker := r.lock.DeadlockChecker(250*time.Millisecond, time.Second)

	r.spawnBackgroundWorker(ctx, logger, "deadlock checker", ignoreLogger(mainDeadlockChecker))
	r.spawnBackgroundWorker(ctx, logger, "podStatus updater", func(c context.Context, l *zap.Logger) {
		r.status.periodicallyRefreshState(c, l, r.global)
	})
	r.spawnBackgroundWorker(ctx, logger, "VmInfo updater", func(c context.Context, l *zap.Logger) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-vmInfoUpdated.Recv():
				vm := getVmInfo()
				ecwc.Updater().UpdatedVM(vm, func() {
					l.Info("VmInfo updated", zap.Any("vmInfo", vm))
				})
			}
		}
	})
	r.spawnBackgroundWorker(ctx, logger, "track scheduler", func(c context.Context, l *zap.Logger) {
		newScheduler := func(withLock func()) {
			ecwc.Updater().NewScheduler(withLock)
		}
		schedulerGone := func(withLock func()) {
			ecwc.Updater().SchedulerGone(withLock)
		}
		r.trackSchedulerLoop(c, l, scheduler, schedulerWatch, pluginGeneration, newScheduler, schedulerGone)
	})
	r.spawnBackgroundWorker(ctx, logger, "get metrics", func(c context.Context, l *zap.Logger) {
		r.getMetricsLoop(c, l, func(metrics api.Metrics, withLock func()) {
			ecwc.Updater().UpdateMetrics(metrics, withLock)
		})
	})
	r.spawnBackgroundWorker(ctx, logger.Named("vm-monitor"), "vm-monitor reconnection loop", func(c context.Context, l *zap.Logger) {
		r.connectToMonitorLoop(c, l, monitorGeneration, monitorStateCallbacks{
			reset: func(withLock func()) {
				ecwc.Updater().ResetMonitor(withLock)
			},
			upscaleRequested: func(request api.MoreResources, withLock func()) {
				ecwc.Updater().UpscaleRequested(request, withLock)
			},
			setActive: func(active bool, withLock func()) {
				ecwc.Updater().MonitorActive(active, withLock)
			},
		})
	})
	r.spawnBackgroundWorker(ctx, execLogger.Named("sleeper"), "executor: sleeper", ecwc.DoSleeper)
	r.spawnBackgroundWorker(ctx, execLogger.Named("plugin"), "executor: plugin", ecwc.DoPluginRequests)
	r.spawnBackgroundWorker(ctx, execLogger.Named("neonvm"), "executor: neonvm", ecwc.DoNeonVMRequests)
	r.spawnBackgroundWorker(ctx, execLogger.Named("vm-monitor-downscale"), "executor: vm-monitor downscale", ecwc.DoMonitorDownscales)
	r.spawnBackgroundWorker(ctx, execLogger.Named("vm-monitor-upscale"), "executor: vm-monitor upscale", ecwc.DoMonitorUpscales)

	// Note: Run doesn't terminate unless the parent context is cancelled - either because the VM
	// pod was deleted, or the autoscaler-agent is exiting.
	select {
	case <-ctx.Done():
		return nil
	case err := <-r.backgroundPanic:
		panic(err)
	}
}

//////////////////////
// Background tasks //
//////////////////////

func ignoreLogger(f func(context.Context)) func(context.Context, *zap.Logger) {
	return func(c context.Context, _ *zap.Logger) {
		f(c)
	}
}

// spawnBackgroundWorker is a helper function to appropriately handle panics in the various goroutines
// spawned by `(Runner) Run`, sending them back on r.backgroundPanic
//
// This method is essentially equivalent to 'go f(ctx)' but with appropriate panic handling,
// start/stop logging, and updating of r.backgroundWorkerCount
func (r *Runner) spawnBackgroundWorker(ctx context.Context, logger *zap.Logger, name string, f func(context.Context, *zap.Logger)) {
	// Increment the background worker count
	r.backgroundWorkerCount.Add(1)

	logger = logger.With(zap.String("taskName", name))

	go func() {
		defer func() {
			// Decrement the background worker count
			r.backgroundWorkerCount.Add(-1)

			if v := recover(); v != nil {
				r.global.metrics.runnerThreadPanics.Inc()

				err := fmt.Errorf("background worker %q panicked: %v", name, v)
				// note: In Go, the stack doesn't "unwind" on panic. Instead, a panic will traverse up
				// the callstack, and each deferred function, when called, will be *added* to the stack
				// as if the original panic() is calling them. So the output of runtime/debug.Stack()
				// has a couple frames do with debug.Stack() and this deferred function, and then the
				// rest of the callstack starts from where the panic occurred.
				//
				// FIXME: we should handle the stack ourselves to remove the stack frames from
				// debug.Stack() and co. -- it's ok to have nice things!
				logger.Error(
					"background worker panicked",
					zap.String("error", fmt.Sprint(v)),
					zap.String("stack", string(debug.Stack())),
				)
				// send to r.backgroundPanic if we can; otherwise, don't worry about it.
				select {
				case r.backgroundPanic <- err:
				default:
				}
			} else {
				logger.Info("background worker ended normally")
			}
		}()

		logger.Info("background worker started")

		f(ctx, logger)
	}()
}

// getMetricsLoop repeatedly attempts to fetch metrics from the VM
//
// Every time metrics are successfully fetched, the value is recorded with newMetrics.
func (r *Runner) getMetricsLoop(
	ctx context.Context,
	logger *zap.Logger,
	newMetrics func(metrics api.Metrics, withLock func()),
) {
	timeout := time.Second * time.Duration(r.global.config.Metrics.RequestTimeoutSeconds)
	waitBetweenDuration := time.Second * time.Duration(r.global.config.Metrics.SecondsBetweenRequests)

	for {
		metrics, err := r.doMetricsRequest(ctx, logger, timeout)
		if err != nil {
			logger.Error("Error making metrics request", zap.Error(err))
			goto next
		} else if metrics == nil {
			goto next
		}

		newMetrics(*metrics, func() {
			logger.Info("Updated metrics", zap.Any("metrics", *metrics))
		})

	next:
		select {
		case <-ctx.Done():
			return
		case <-time.After(waitBetweenDuration):
		}
	}
}

type monitorInfo struct {
	generation executor.GenerationNumber
	dispatcher *Dispatcher
}

type monitorStateCallbacks struct {
	reset            func(withLock func())
	upscaleRequested func(request api.MoreResources, withLock func())
	setActive        func(active bool, withLock func())
}

// connectToMonitorLoop does lifecycle management of the (re)connection to the vm-monitor
func (r *Runner) connectToMonitorLoop(
	ctx context.Context,
	logger *zap.Logger,
	generation *executor.StoredGenerationNumber,
	callbacks monitorStateCallbacks,
) {
	addr := fmt.Sprintf("ws://%s:%d/monitor", r.podIP, r.global.config.Monitor.ServerPort)

	minWait := time.Second * time.Duration(r.global.config.Monitor.ConnectionRetryMinWaitSeconds)
	var lastStart time.Time

	for i := 0; ; i += 1 {
		// Remove any prior Dispatcher from the Runner
		if i != 0 {
			func() {
				r.lock.Lock()
				defer r.lock.Unlock()
				callbacks.reset(func() {
					generation.Inc()
					r.monitor = nil
					logger.Info("Reset previous vm-monitor connection")
				})
			}()
		}

		// If the context was canceled, don't restart
		if err := ctx.Err(); err != nil {
			action := "attempt"
			if i != 0 {
				action = "retry "
			}
			logger.Info(
				fmt.Sprintf("Aborting vm-monitor connection %s because context is already canceled", action),
				zap.Error(err),
			)
			return
		}

		// Delayed restart management, long because of friendly logging:
		if i != 0 {
			endTime := time.Now()
			runtime := endTime.Sub(lastStart)

			if runtime > minWait {
				logger.Info(
					"Immediately retrying connection to vm-monitor",
					zap.String("addr", addr),
					zap.Duration("totalRuntime", runtime),
				)
			} else {
				delay := minWait - runtime
				logger.Info(
					"Connection to vm-monitor was not live for long, retrying after delay",
					zap.Duration("delay", delay),
					zap.Duration("totalRuntime", runtime),
				)

				select {
				case <-time.After(delay):
					logger.Info(
						"Retrying connection to vm-monitor",
						zap.Duration("delay", delay),
						zap.Duration("waitTime", time.Since(endTime)),
						zap.String("addr", addr),
					)
				case <-ctx.Done():
					logger.Info(
						"Canceling retrying connection to vm-monitor",
						zap.Duration("delay", delay),
						zap.Duration("waitTime", time.Since(endTime)),
						zap.Error(ctx.Err()),
					)
					return
				}
			}
		} else {
			logger.Info("Connecting to vm-monitor", zap.String("addr", addr))
		}

		dispatcher, err := NewDispatcher(ctx, logger, addr, r, callbacks.upscaleRequested)
		if err != nil {
			logger.Error("Failed to connect to vm-monitor", zap.String("addr", addr), zap.Error(err))
			continue
		}

		// Update runner to the new dispatcher
		func() {
			r.lock.Lock()
			defer r.lock.Unlock()
			callbacks.setActive(true, func() {
				r.monitor = &monitorInfo{
					generation: generation.Inc(),
					dispatcher: dispatcher,
				}
				logger.Info("Connected to vm-monitor")
			})
		}()

		// Wait until the dispatcher is no longer running, either due to error or because the
		// root-level Runner context was canceled.
		<-dispatcher.ExitSignal()

		if err := dispatcher.ExitError(); err != nil {
			logger.Error("Dispatcher for vm-monitor connection exited due to error", zap.Error(err))
		}
	}
}

// trackSchedulerLoop listens on the schedulerWatch, keeping r.scheduler up-to-date and signalling
// on registeredScheduler whenever a new Scheduler is successfully registered
func (r *Runner) trackSchedulerLoop(
	ctx context.Context,
	logger *zap.Logger,
	init *schedwatch.SchedulerInfo,
	schedulerWatch schedwatch.SchedulerWatch,
	generation *executor.StoredGenerationNumber,
	newScheduler func(withLock func()),
	schedulerGone func(withLock func()),
) {
	// pre-declare a bunch of variables because we have some gotos here.
	var (
		lastStart   time.Time
		minWait     time.Duration    = 5 * time.Second // minimum time we have to wait between scheduler starts
		okForNew    <-chan time.Time                   // channel that sends when we've waited long enough for a new scheduler
		currentInfo schedwatch.SchedulerInfo
	)

	if init == nil {
		goto waitForNewScheduler
	}

	currentInfo = *init

startScheduler:
	schedulerWatch.ExpectingDeleted()

	lastStart = time.Now()
	okForNew = time.After(minWait)

	// Set the current scheduler
	{
		verb := "Setting"
		if init == nil || init.UID != currentInfo.UID {
			verb = "Updating"
		}

		func() {
			r.lock.Lock()
			defer r.lock.Unlock()

			newScheduler(func() {
				logger.Info(fmt.Sprintf("%s scheduler pod", verb), zap.Object("scheduler", currentInfo))
				r.scheduler = &Scheduler{
					runner:     r,
					info:       currentInfo,
					generation: generation.Inc(),
				}
			})
		}()
	}

	// Start watching for the current scheduler to be deleted
	for {
		select {
		case <-ctx.Done():
			return
		case info := <-schedulerWatch.Deleted:
			matched := func() bool {
				r.lock.Lock()
				defer r.lock.Unlock()

				if r.scheduler.info.UID != info.UID {
					logger.Info(
						"Scheduler candidate pod was deleted, but we aren't using it yet",
						zap.Object("scheduler", r.scheduler.info), zap.Object("candidate", info),
					)
					return false
				}

				schedulerGone(func() {
					logger.Info(
						"Scheduler pod was deleted. Aborting further communication",
						zap.Object("scheduler", r.scheduler.info),
					)

					generation.Inc()
					r.scheduler = nil
				})
				return true
			}()

			if matched {
				goto waitForNewScheduler
			}
		}
	}

waitForNewScheduler:
	schedulerWatch.ExpectingReady()

	// If there's a previous scheduler, make sure that we don't restart too quickly. We want a
	// minimum delay between scheduler starts, controlled by okForNew
	if okForNew != nil {
		select {
		case <-okForNew:
		default:
			// Not ready yet; let's log something about it:
			logger.Info(
				"Scheduler ended quickly. Respecting minimum delay before switching to a new one",
				zap.Duration("activeTime", time.Since(lastStart)), zap.Duration("delay", minWait),
			)
			select {
			case <-ctx.Done():
				return
			case <-okForNew:
			}
		}
	}

	// Actually watch for a new scheduler
	select {
	case <-ctx.Done():
		return
	case newInfo := <-schedulerWatch.ReadyQueue:
		currentInfo = newInfo
		goto startScheduler
	}
}

//////////////////////////////////////////
// Lower-level implementation functions //
//////////////////////////////////////////

// doMetricsRequest makes a single metrics request to the VM
func (r *Runner) doMetricsRequest(
	ctx context.Context,
	logger *zap.Logger,
	timeout time.Duration,
) (*api.Metrics, error) {
	url := fmt.Sprintf("http://%s:%d/metrics", r.podIP, r.global.config.Metrics.Port)

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, bytes.NewReader(nil))
	if err != nil {
		panic(fmt.Errorf("Error constructing metrics request to %q: %w", url, err))
	}

	logger.Info("Making metrics request to VM", zap.String("url", url))

	resp, err := http.DefaultClient.Do(req)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	} else if err != nil {
		return nil, fmt.Errorf("Error making request to %q: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Error receiving response body: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Unsuccessful response status %d: %s", resp.StatusCode, string(body))
	}

	m, err := api.ReadMetrics(body, r.global.config.Metrics.LoadMetricPrefix)
	if err != nil {
		return nil, fmt.Errorf("Error reading metrics from prometheus output: %w", err)
	}

	return &m, nil
}

func (r *Runner) doNeonVMRequest(ctx context.Context, target api.Resources) error {
	patches := []util.JSONPatch{{
		Op:    util.PatchReplace,
		Path:  "/spec/guest/cpus/use",
		Value: target.VCPU.ToResourceQuantity(),
	}, {
		Op:    util.PatchReplace,
		Path:  "/spec/guest/memorySlots/use",
		Value: target.Mem,
	}}

	patchPayload, err := json.Marshal(patches)
	if err != nil {
		panic(fmt.Errorf("Error marshalling JSON patch: %w", err))
	}

	timeout := time.Second * time.Duration(r.global.config.Scaling.RequestTimeoutSeconds)
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// FIXME: We should check the returned VM object here, in case the values are different.
	//
	// Also relevant: <https://github.com/neondatabase/autoscaling/issues/23>
	_, err = r.global.vmClient.NeonvmV1().VirtualMachines(r.vmName.Namespace).
		Patch(requestCtx, r.vmName.Name, ktypes.JSONPatchType, patchPayload, metav1.PatchOptions{})

	if err != nil {
		r.global.metrics.neonvmRequestsOutbound.WithLabelValues(fmt.Sprintf("[error: %s]", util.RootError(err))).Inc()
		return err
	}

	r.global.metrics.neonvmRequestsOutbound.WithLabelValues("ok").Inc()
	return nil
}

func (r *Runner) recordResourceChange(current, target api.Resources, metrics resourceChangePair) {
	getDirection := func(targetIsGreater bool) string {
		if targetIsGreater {
			return directionValueInc
		} else {
			return directionValueDec
		}
	}

	abs := current.AbsDiff(target)

	// Add CPU
	if abs.VCPU != 0 {
		direction := getDirection(target.VCPU > current.VCPU)

		metrics.cpu.WithLabelValues(direction).Add(abs.VCPU.AsFloat64())
	}

	// Add memory
	if abs.Mem != 0 {
		direction := getDirection(target.Mem > current.Mem)

		// Avoid floating-point inaccuracy.
		byteTotal := r.memSlotSize.Value() * int64(abs.Mem)
		mib := int64(1 << 20)
		floatMB := float64(byteTotal/mib) + float64(byteTotal%mib)/float64(mib)

		metrics.mem.WithLabelValues(direction).Add(floatMB)
	}
}

func doMonitorDownscale(
	ctx context.Context,
	logger *zap.Logger,
	dispatcher *Dispatcher,
	target api.Resources,
) (*api.DownscaleResult, error) {
	r := dispatcher.runner
	rawResources := target.ConvertToAllocation(r.memSlotSize)

	timeout := time.Second * time.Duration(r.global.config.Monitor.ResponseTimeoutSeconds)

	res, err := dispatcher.Call(ctx, logger, timeout, "DownscaleRequest", api.DownscaleRequest{
		Target: rawResources,
	})
	if err != nil {
		return nil, err
	}

	return res.Result, nil
}

func doMonitorUpscale(
	ctx context.Context,
	logger *zap.Logger,
	dispatcher *Dispatcher,
	target api.Resources,
) error {
	r := dispatcher.runner
	rawResources := target.ConvertToAllocation(r.memSlotSize)

	timeout := time.Second * time.Duration(r.global.config.Monitor.ResponseTimeoutSeconds)

	_, err := dispatcher.Call(ctx, logger, timeout, "UpscaleNotification", api.UpscaleNotification{
		Granted: rawResources,
	})
	return err
}

// DoRequest sends a request to the scheduler and does not validate the response.
func (s *Scheduler) DoRequest(
	ctx context.Context,
	logger *zap.Logger,
	resources api.Resources,
	metrics *api.Metrics,
) (_ *api.PluginResponse, err error) {
	reqData := &api.AgentRequest{
		ProtoVersion: PluginProtocolVersion,
		Pod:          s.runner.podName,
		Resources:    resources,
		Metrics:      metrics,
	}

	// make sure we log any error we're returning:
	defer func() {
		if err != nil {
			logger.Error("Scheduler request failed", zap.Error(err))
		}
	}()

	reqBody, err := json.Marshal(reqData)
	if err != nil {
		return nil, fmt.Errorf("Error encoding request JSON: %w", err)
	}

	timeout := time.Second * time.Duration(s.runner.global.config.Scaling.RequestTimeoutSeconds)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/", s.info.IP, s.runner.global.config.Scheduler.RequestPort)

	request, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("Error building request to %q: %w", url, err)
	}
	request.Header.Set("content-type", "application/json")

	logger.Info("Sending request to scheduler", zap.Any("request", reqData))

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		description := fmt.Sprintf("[error doing request: %s]", util.RootError(err))
		s.runner.global.metrics.schedulerRequests.WithLabelValues(description).Inc()
		return nil, fmt.Errorf("Error doing request: %w", err)
	}
	defer response.Body.Close()

	s.runner.global.metrics.schedulerRequests.WithLabelValues(strconv.Itoa(response.StatusCode)).Inc()

	respBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("Error reading body for response: %w", err)
	}

	if response.StatusCode != 200 {
		// Fatal because 4XX implies our state doesn't match theirs, 5XX means we can't assume
		// current contents of the state, and anything other than 200, 4XX, or 5XX shouldn't happen
		return nil, fmt.Errorf("Received response status %d body %q", response.StatusCode, string(respBody))
	}

	var respData api.PluginResponse
	if err := json.Unmarshal(respBody, &respData); err != nil {
		// Fatal because invalid JSON might also be semantically invalid
		return nil, fmt.Errorf("Bad JSON response: %w", err)
	}

	logger.Info("Received response from scheduler", zap.Any("response", respData))

	return &respData, nil
}
