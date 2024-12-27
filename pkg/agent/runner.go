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
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/agent/core/revsource"
	"github.com/neondatabase/autoscaling/pkg/agent/executor"
	"github.com/neondatabase/autoscaling/pkg/agent/scalingevents"
	"github.com/neondatabase/autoscaling/pkg/agent/schedwatch"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/patch"
)

// PluginProtocolVersion is the current version of the agent<->scheduler plugin in use by this
// autoscaler-agent.
//
// Currently, each autoscaler-agent supports only one version at a time. In the future, this may
// change.
const PluginProtocolVersion api.PluginProtoVersion = api.PluginProtoV5_0

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

	memSlotSize api.Bytes

	// lock guards the values of all mutable fields - namely, scheduler and monitor (which may be
	// read without the lock, but the lock must be acquired to lock them).
	lock util.ChanMutex

	// executorStateDump is set by (*Runner).Run and provides a way to get the state of the
	// "executor"
	executorStateDump func() executor.StateDump

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

// RunnerState is the serializable state of the Runner, extracted by its State method
type RunnerState struct {
	PodIP                 string             `json:"podIP"`
	ExecutorState         executor.StateDump `json:"executorState"`
	Monitor               *MonitorState      `json:"monitor"`
	BackgroundWorkerCount int64              `json:"backgroundWorkerCount"`
}

// SchedulerState is the state of a Scheduler, constructed as part of a Runner's State Method
type SchedulerState struct {
	Info schedwatch.SchedulerInfo `json:"info"`
}

// Temporary type, to hopefully help with debugging https://github.com/neondatabase/autoscaling/issues/503
type MonitorState struct {
	WaitersSize int `json:"waitersSize"`
}

func (r *Runner) State(ctx context.Context) (*RunnerState, error) {
	if err := r.lock.TryLock(ctx); err != nil {
		return nil, err
	}
	defer r.lock.Unlock()

	var monitorState *MonitorState
	if r.monitor != nil {
		monitorState = &MonitorState{
			WaitersSize: r.monitor.dispatcher.lenWaiters(),
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
		Monitor:               monitorState,
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

		r.Run(ctx, logger, vmInfoUpdated)
		endTime := time.Now()

		exitKind := podStatusExitCanceled // normal exit, only by context being canceled.
		r.status.update(r.global, func(stat podStatus) podStatus {
			stat.endState = &podStatusEndState{
				ExitKind: exitKind,
				Error:    nil,
				Time:     endTime,
			}
			return stat
		})

		logger.Info("Ended without error")
	}()
}

// Run is the main entrypoint to the long-running per-VM pod tasks
func (r *Runner) Run(ctx context.Context, logger *zap.Logger, vmInfoUpdated util.CondChannelReceiver) {
	ctx, r.shutdown = context.WithCancel(ctx)
	defer r.shutdown()

	getVmInfo := func() api.VmInfo {
		r.status.mu.Lock()
		defer r.status.mu.Unlock()
		return r.status.vmInfo
	}

	execLogger := logger.Named("exec")

	// Subtract a small random amount from core.Config.PluginRequestTick so that periodic requests
	// tend to become distribted randomly over time.
	pluginRequestJitter := util.NewTimeRange(time.Millisecond, 0, 100).Random()

	coreExecLogger := execLogger.Named("core")

	vmInfo := getVmInfo()
	var initialRevision int64
	if vmInfo.CurrentRevision != nil {
		initialRevision = vmInfo.CurrentRevision.Value
	}
	// "dsrl" stands for "desired scaling report limiter" -- helper to avoid spamming events.
	dsrl := &desiredScalingReportLimiter{
		lastTarget: nil,
		lastParts:  nil,
	}
	revisionSource := revsource.NewRevisionSource(initialRevision, WrapHistogramVec(&r.global.metrics.scalingLatency))
	executorCore := executor.NewExecutorCore(coreExecLogger, vmInfo, executor.Config{
		OnNextActions: r.global.metrics.runnerNextActions.Inc,
		Core: core.Config{
			ComputeUnit:                        r.global.config.Scaling.ComputeUnit,
			DefaultScalingConfig:               r.global.config.Scaling.DefaultConfig,
			NeonVMRetryWait:                    time.Second * time.Duration(r.global.config.NeonVM.RetryFailedRequestSeconds),
			PluginRequestTick:                  time.Second*time.Duration(r.global.config.Scheduler.RequestAtLeastEverySeconds) - pluginRequestJitter,
			PluginRetryWait:                    time.Second * time.Duration(r.global.config.Scheduler.RetryFailedRequestSeconds),
			PluginDeniedRetryWait:              time.Second * time.Duration(r.global.config.Scheduler.RetryDeniedUpscaleSeconds),
			MonitorDeniedDownscaleCooldown:     time.Second * time.Duration(r.global.config.Monitor.RetryDeniedDownscaleSeconds),
			MonitorRequestedUpscaleValidPeriod: time.Second * time.Duration(r.global.config.Monitor.RequestedUpscaleValidSeconds),
			MonitorRetryWait:                   time.Second * time.Duration(r.global.config.Monitor.RetryFailedRequestSeconds),
			Log: core.LogConfig{
				Info: coreExecLogger.Info,
				Warn: coreExecLogger.Warn,
			},
			RevisionSource: revisionSource,
			ObservabilityCallbacks: core.ObservabilityCallbacks{
				PluginLatency:  WrapHistogramVec(&r.global.metrics.pluginLatency),
				MonitorLatency: WrapHistogramVec(&r.global.metrics.monitorLatency),
				NeonVMLatency:  WrapHistogramVec(&r.global.metrics.neonvmLatency),
				ScalingEvent:   r.reportScalingEvent,
				DesiredScaling: func(ts time.Time, current, target uint32, parts scalingevents.GoalCUComponents) {
					r.reportDesiredScaling(dsrl, ts, current, target, parts)
				},
			},
		},
	})

	r.executorStateDump = executorCore.StateDump

	monitorGeneration := executor.NewStoredGenerationNumber()

	pluginIface := makePluginInterface(r)
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
	r.spawnBackgroundWorker(ctx, logger, "podStatus updater", func(ctx2 context.Context, logger2 *zap.Logger) {
		r.status.periodicallyRefreshState(ctx2, logger2, r.global)
	})
	r.spawnBackgroundWorker(ctx, logger, "VmInfo updater", func(ctx2 context.Context, logger2 *zap.Logger) {
		for {
			select {
			case <-ctx2.Done():
				return
			case <-vmInfoUpdated.Recv():
				vm := getVmInfo()
				ecwc.Updater().UpdatedVM(vm, func() {
					logger2.Info("VmInfo updated", zap.Any("vmInfo", vm))
				})
			}
		}
	})
	r.spawnBackgroundWorker(ctx, logger, "get system metrics", func(ctx2 context.Context, logger2 *zap.Logger) {
		getMetricsLoop(
			r,
			ctx2,
			logger2,
			r.global.config.Metrics.System,
			metricsMgr[*core.SystemMetrics]{
				kind:         "system",
				emptyMetrics: func() *core.SystemMetrics { return new(core.SystemMetrics) },
				isActive:     func() bool { return true },
				updateMetrics: func(metrics *core.SystemMetrics, withLock func()) {
					ecwc.Updater().UpdateSystemMetrics(*metrics, withLock)
				},
			},
		)
	})
	r.spawnBackgroundWorker(ctx, logger, "get LFC metrics", func(ctx2 context.Context, logger2 *zap.Logger) {
		getMetricsLoop(
			r,
			ctx2,
			logger2,
			r.global.config.Metrics.LFC,
			metricsMgr[*core.LFCMetrics]{
				kind:         "LFC",
				emptyMetrics: func() *core.LFCMetrics { return new(core.LFCMetrics) },
				isActive: func() bool {
					scalingConfig := r.global.config.Scaling.DefaultConfig.WithOverrides(getVmInfo().Config.ScalingConfig)
					return *scalingConfig.EnableLFCMetrics // guaranteed non-nil as a required field.
				},
				updateMetrics: func(metrics *core.LFCMetrics, withLock func()) {
					ecwc.Updater().UpdateLFCMetrics(*metrics, withLock)
				},
			},
		)
	})
	r.spawnBackgroundWorker(ctx, logger.Named("vm-monitor"), "vm-monitor reconnection loop", func(ctx2 context.Context, logger2 *zap.Logger) {
		r.connectToMonitorLoop(ctx2, logger2, monitorGeneration, monitorStateCallbacks{
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
		return
	case err := <-r.backgroundPanic:
		panic(err)
	}
}

func (r *Runner) reportScalingEvent(timestamp time.Time, currentCU, targetCU uint32) {
	var endpointID string

	enabled := func() bool {
		r.status.mu.Lock()
		defer r.status.mu.Unlock()

		endpointID = r.status.endpointID
		return endpointID != "" && r.status.vmInfo.Config.ReportScalingEvents
	}()
	if !enabled {
		return
	}

	reporter := r.global.scalingReporter
	reporter.Submit(reporter.NewRealEvent(
		timestamp,
		endpointID,
		currentCU,
		targetCU,
	))
}

func (r *Runner) reportDesiredScaling(
	rl *desiredScalingReportLimiter,
	timestamp time.Time,
	currentCU uint32,
	targetCU uint32,
	parts scalingevents.GoalCUComponents,
) {
	var endpointID string

	enabled := func() bool {
		r.status.mu.Lock()
		defer r.status.mu.Unlock()

		endpointID = r.status.endpointID
		return endpointID != "" && r.status.vmInfo.Config.ReportDesiredScaling
	}()
	if !enabled {
		return
	}

	r.global.vmMetrics.updateDesiredCU(
		r.vmName,
		r.global.config.ScalingEvents.CUMultiplier, // have to multiply before exposing as metrics here.
		targetCU,
		parts,
	)

	rl.report(r.global.scalingReporter, timestamp, endpointID, currentCU, targetCU, parts)
}

type desiredScalingReportLimiter struct {
	lastTarget *uint32
	lastParts  *scalingevents.GoalCUComponents
}

func (rl *desiredScalingReportLimiter) report(
	reporter *scalingevents.Reporter,
	timestamp time.Time,
	endpointID string,
	currentCU uint32,
	targetCU uint32,
	parts scalingevents.GoalCUComponents,
) {
	closeEnough := func(x *float64, y *float64) bool {
		if (x != nil) != (y != nil) {
			return false
		}
		if x == nil /* && y == nil */ {
			return true
		}
		// true iff x and y are within the threshold of each other
		return math.Abs(*x-*y) < 0.25
	}

	// Check if we should skip this time.
	if rl.lastTarget != nil && rl.lastParts != nil {
		skip := *rl.lastTarget == targetCU &&
			closeEnough(rl.lastParts.CPU, parts.CPU) &&
			closeEnough(rl.lastParts.Mem, parts.Mem) &&
			closeEnough(rl.lastParts.LFC, parts.LFC)
		if skip {
			return
		}
	}

	// Not skipping.
	rl.lastTarget = &targetCU
	rl.lastParts = &parts
	reporter.Submit(reporter.NewHypotheticalEvent(
		timestamp,
		endpointID,
		currentCU,
		targetCU,
		parts,
	))
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

type metricsMgr[M core.FromPrometheus] struct {
	// kind is the human-readable name representing this type of metrics.
	// It's either "system" or "LFC".
	kind string

	// emptyMetrics returns a new M
	//
	// Typically this is required because M is itself a pointer, so if we just initialized it with a
	// zero value, we'd end up with nil pointer derefs. There *are* ways around this with generics,
	// but at the time we decided this is the least convoluted way.
	emptyMetrics func() M

	// isActive returns whether these metrics should currently be collected for the VM.
	//
	// For example, with LFC metrics, we return false if they are not enabled for the VM.
	isActive func() bool

	// updateMetrics is a callback to update the internal state with new values for these metrics.
	updateMetrics func(metrics M, withLock func())
}

// getMetricsLoop repeatedly attempts to fetch metrics from the VM
//
// Every time metrics are successfully fetched, the value is recorded with mgr.updateMetrics().
func getMetricsLoop[M core.FromPrometheus](
	r *Runner,
	ctx context.Context,
	logger *zap.Logger,
	config MetricsSourceConfig,
	mgr metricsMgr[M],
) {
	waitBetweenDuration := time.Second * time.Duration(config.SecondsBetweenRequests)

	randomStartWait := util.NewTimeRange(time.Second, 0, int(config.SecondsBetweenRequests)).Random()

	lastActive := mgr.isActive()

	// Don't log anything if we're not making this type of metrics request currently.
	//
	// The idea is that isActive() can/should be used for gradual rollout of new metrics, and we
	// don't want to log every time we *don't* do the new thing.
	if lastActive {
		logger.Info(
			fmt.Sprintf("Sleeping for random delay before making first %s metrics request", mgr.kind),
			zap.Duration("delay", randomStartWait),
		)
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(randomStartWait):
	}

	for {
		if !mgr.isActive() {
			if lastActive {
				logger.Info(fmt.Sprintf("VM is no longer active for %s metrics requests", mgr.kind))
			}
			lastActive = false
		} else {
			if !lastActive {
				logger.Info(fmt.Sprintf("VM is now active for %s metrics requests", mgr.kind))
			}
			lastActive = true

			metrics := mgr.emptyMetrics()
			err := doMetricsRequest(r, ctx, logger, metrics, config)
			if err != nil {
				logger.Error("Error making metrics request", zap.Error(err))
				goto next
			}

			mgr.updateMetrics(metrics, func() {
				logger.Info("Updated metrics", zap.Any("metrics", metrics))
			})
		}

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

		lastStart = time.Now()
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

//////////////////////////////////////////
// Lower-level implementation functions //
//////////////////////////////////////////

// doMetricsRequest makes a single metrics request to the VM, writing the result into 'metrics'
func doMetricsRequest(
	r *Runner,
	ctx context.Context,
	logger *zap.Logger,
	metrics core.FromPrometheus,
	config MetricsSourceConfig,
) error {
	url := fmt.Sprintf("http://%s:%d/metrics", r.podIP, config.Port)

	timeout := time.Second * time.Duration(config.RequestTimeoutSeconds)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, bytes.NewReader(nil))
	if err != nil {
		panic(fmt.Errorf("Error constructing metrics request to %q: %w", url, err))
	}

	logger.Debug("Making metrics request to VM", zap.String("url", url))

	resp, err := http.DefaultClient.Do(req)
	if ctx.Err() != nil {
		return ctx.Err()
	} else if err != nil {
		return fmt.Errorf("Error making request to %q: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("Unsuccessful response status %d", resp.StatusCode)
	}

	if err := core.ParseMetrics(resp.Body, metrics); err != nil {
		return fmt.Errorf("Error parsing metrics from prometheus output: %w", err)
	}

	return nil
}

func (r *Runner) doNeonVMRequest(
	ctx context.Context,
	target api.Resources,
	targetRevision vmv1.RevisionWithTime,
) error {
	patches := []patch.Operation{{
		Op:    patch.OpReplace,
		Path:  "/spec/guest/cpus/use",
		Value: target.VCPU.ToResourceQuantity(),
	}, {
		Op:    patch.OpReplace,
		Path:  "/spec/guest/memorySlots/use",
		Value: uint32(target.Mem / r.memSlotSize),
	}, {
		Op:    patch.OpReplace,
		Path:  "/spec/targetRevision",
		Value: targetRevision,
	}}

	patchPayload, err := json.Marshal(patches)
	if err != nil {
		panic(fmt.Errorf("Error marshalling JSON patch: %w", err))
	}

	timeout := time.Second * time.Duration(r.global.config.NeonVM.RequestTimeoutSeconds)
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// FIXME: We should check the returned VM object here, in case the values are different.
	//
	// Also relevant: <https://github.com/neondatabase/autoscaling/issues/23>
	_, err = r.global.vmClient.NeonvmV1().VirtualMachines(r.vmName.Namespace).
		Patch(requestCtx, r.vmName.Name, ktypes.JSONPatchType, patchPayload, metav1.PatchOptions{})
	if err != nil {
		errMsg := util.RootError(err).Error()
		// Some error messages contain the object name. We could try to filter them all out, but
		// it's probably more maintainable to just keep them as-is and remove the name.
		errMsg = strings.ReplaceAll(errMsg, r.vmName.Name, "<name>")
		r.global.metrics.neonvmRequestsOutbound.WithLabelValues(fmt.Sprintf("[error: %s]", errMsg)).Inc()
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
		byteTotal := abs.Mem
		mib := api.Bytes(1 << 20)
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
	rawResources := target.ConvertToAllocation()

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
	rawResources := target.ConvertToAllocation()

	timeout := time.Second * time.Duration(r.global.config.Monitor.ResponseTimeoutSeconds)

	_, err := dispatcher.Call(ctx, logger, timeout, "UpscaleNotification", api.UpscaleNotification{
		Granted: rawResources,
	})
	return err
}

// DoSchedulerRequest sends a request to the scheduler and does not validate the response.
func (r *Runner) DoSchedulerRequest(
	ctx context.Context,
	logger *zap.Logger,
	resources api.Resources,
	lastPermit *api.Resources,
	metrics *api.Metrics,
) (_ *api.PluginResponse, err error) {
	reqData := &api.AgentRequest{
		ProtoVersion: PluginProtocolVersion,
		Pod:          r.podName,
		ComputeUnit:  r.global.config.Scaling.ComputeUnit,
		Resources:    resources,
		LastPermit:   lastPermit,
		Metrics:      metrics,
	}

	// make sure we log any error we're returning:
	defer func() {
		if err != nil {
			logger.Error("Scheduler request failed", zap.Error(err))
		}
	}()

	sched := r.global.schedTracker.Get()
	if sched == nil {
		err := errors.New("no known ready scheduler to send request to")
		description := fmt.Sprintf("[error doing request: %s]", err)
		r.global.metrics.schedulerRequests.WithLabelValues(description).Inc()
		return nil, err
	}

	reqBody, err := json.Marshal(reqData)
	if err != nil {
		return nil, fmt.Errorf("Error encoding request JSON: %w", err)
	}

	timeout := time.Second * time.Duration(r.global.config.NeonVM.RequestTimeoutSeconds)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/", sched.IP, r.global.config.Scheduler.RequestPort)

	request, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("Error building request to %q: %w", url, err)
	}
	request.Header.Set("content-type", "application/json")

	logger.Debug("Sending request to scheduler", zap.Any("request", reqData))

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		description := fmt.Sprintf("[error doing request: %s]", util.RootError(err))
		r.global.metrics.schedulerRequests.WithLabelValues(description).Inc()
		return nil, fmt.Errorf("Error doing request: %w", err)
	}
	defer response.Body.Close()

	r.global.metrics.schedulerRequests.WithLabelValues(strconv.Itoa(response.StatusCode)).Inc()

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
	level := zap.DebugLevel
	if respData.Permit.HasFieldLessThan(resources) {
		level = zap.WarnLevel
	}
	logger.Log(level, "Received response from scheduler", zap.Any("response", respData), zap.Any("requested", resources))

	return &respData, nil
}
