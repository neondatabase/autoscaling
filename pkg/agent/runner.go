package agent

// Core glue and logic for a single VM
//
// The primary object in this file is the Runner. We create a new Runner for each VM, and the Runner
// spawns a handful of long-running tasks that share state via the Runner object itself.
//
// # General paradigm
//
// At a high level, we're trying to balance a few goals that are in tension with each other:
//
//  1. It should be OK to panic, if an error is truly unrecoverable
//  2. A single Runner's panic shouldn't bring down the entire autoscaler-agent¹
//  3. We want to expose a State() method to view (almost) all internal state
//  4. Some high-level actions (e.g., call to vm-monitor; update VM to desired state) require
//     that we have *at most* one such action running at a time.
//
// There are a number of possible solutions to this set of goals. All reasonable solutions require
// multiple goroutines. Here's what we do:
//
//  * Runner acts as a global (per-VM) shared object with its fields guarded by Runner.lock. The
//    lock is held for as short a duration as possible.
//  * "Background" threads are responsible for relatively high-level tasks - like:
//     * "track scheduler"
//     * "get metrics"
//     * "handle VM resources" - using metrics, calculates target resources level and contacts
//       scheduler, vm-monitor, and NeonVM -- the "scaling" part of "autoscaling".
//  * Each thread makes *synchronous* HTTP requests while holding the necessary lock to prevent any other
//    thread from making HTTP requests to the same entity. For example:
//    * All requests to NeonVM and the scheduler plugin are guarded by Runner.requestLock, which
//      guarantees that we aren't simultaneously telling the scheduler one thing and changing it at
//      the same time.
//  * Each "background" thread is spawned by (*Runner).spawnBackgroundWorker(), which appropriately
//    catches panics and signals the Runner so that the main thread from (*Runner).Run() cleanly
//    shuts everything down.
//  * Every significant lock has an associated "deadlock checker" background thread that panics if
//    it takes too long to acquire the lock.
//
// spawnBackgroundWorker guarantees (1) and (2); Runner.lock makes (3) possible; and
// Runner.requestLock guarantees (4).
//
// ---
// ¹ If we allowed a single Runner to take down the whole autoscaler-agent, it would open up the
// possibility of crash-looping due to unusual cluster state (e.g., weird values in a NeonVM object)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

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

	// vm stores some common information about the VM.
	//
	// This field MUST NOT be read or updated without holding lock.
	vm      api.VmInfo
	podName util.NamespacedName
	podIP   string

	// schedulerRespondedWithMigration is true iff the scheduler has returned an api.PluginResponse
	// indicating that it was prompted to start migrating the VM.
	//
	// This field MUST NOT be updated without holding BOTH lock and requestLock.
	//
	// This field MAY be read while holding EITHER lock or requestLock.
	schedulerRespondedWithMigration bool

	// lock guards the values of all mutable fields. The immutable fields are:
	// - global
	// - status
	// - podName
	// - podIP
	// - logger
	// - backgroundPanic
	// lock MUST NOT be held while interacting with the network. The appropriate synchronization to
	// ensure we don't send conflicting requests is provided by requestLock.
	lock util.ChanMutex

	// requestLock must be held during any request to the scheduler plugin or any patch request to
	// NeonVM.
	//
	// requestLock MUST NOT be held while performing any interactions with the network, apart from
	// those listed above.
	requestLock util.ChanMutex

	// lastMetrics stores the most recent metrics we've received from the VM
	//
	// This field is exclusively set by the getMetricsLoop background worker, and will never change
	// from non-nil to nil. The data behind each pointer is immutable, but the value of the pointer
	// itself is not.
	lastMetrics *api.Metrics

	// scheduler is the current scheduler that we're communicating with, or nil if there isn't one.
	// Each scheduler's info field is immutable. When a scheduler is replaced, only the pointer
	// value here is updated; the original Scheduler remains unchanged.
	scheduler atomic.Pointer[Scheduler]
	// monitor, if non nil, stores the current Dispatcher in use for communicating with the
	// vm-monitor
	monitor atomic.Pointer[Dispatcher]
	// computeUnit is the latest Compute Unit reported by a scheduler. It may be nil, if we haven't
	// been able to contact one yet.
	//
	// This field MUST NOT be updated without holding BOTH lock and requestLock.
	computeUnit *api.Resources

	// lastApproved is the last resource allocation that a scheduler has approved. It may be nil, if
	// we haven't been able to contact one yet.
	lastApproved *api.Resources

	// lastSchedulerError provides the error that occurred - if any - during the most recent request
	// to the current scheduler. This field is not nil only when scheduler is not nil.
	lastSchedulerError error

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

	// registered is true only once a call to this Scheduler's Register() method has been made
	//
	// All methods that make a request to the scheduler will first call Register() if registered is
	// false.
	//
	// This field MUST NOT be updated without holding BOTH runner.requestLock AND runner.lock
	//
	// This field MAY be read while holding EITHER runner.requestLock OR runner.lock.
	registered bool

	// fatalError is non-nil if an error occurred while communicating with the scheduler that we
	// cannot recover from.
	//
	// Examples of fatal errors:
	//
	// * HTTP response status 4XX or 5XX - we don't know the plugin's state
	// * Semantically invalid response - either a logic error occurred, or the plugin's state
	//   doesn't match ours
	//
	// This field MUST NOT be updated without holding BOTH runner.requestLock AND runner.lock.
	//
	// This field MAY be read while holding EITHER runner.requestLock OR runner.lock.
	fatalError error

	// fatal is used for signalling that fatalError has been set (and so we should look for a new
	// scheduler)
	fatal util.SignalSender[struct{}]
}

// RunnerState is the serializable state of the Runner, extracted by its State method
type RunnerState struct {
	PodIP                 string          `json:"podIP"`
	VM                    api.VmInfo      `json:"vm"`
	LastMetrics           *api.Metrics    `json:"lastMetrics"`
	Scheduler             *SchedulerState `json:"scheduler"`
	ComputeUnit           *api.Resources  `json:"computeUnit"`
	LastApproved          *api.Resources  `json:"lastApproved"`
	LastSchedulerError    error           `json:"lastSchedulerError"`
	BackgroundWorkerCount int64           `json:"backgroundWorkerCount"`

	SchedulerRespondedWithMigration bool `json:"migrationStarted"`
}

// SchedulerState is the state of a Scheduler, constructed as part of a Runner's State Method
type SchedulerState struct {
	Info       schedwatch.SchedulerInfo `json:"info"`
	Registered bool                     `json:"registered"`
	FatalError error                    `json:"fatalError"`
}

func (r *Runner) State(ctx context.Context) (*RunnerState, error) {
	if err := r.lock.TryLock(ctx); err != nil {
		return nil, err
	}
	defer r.lock.Unlock()

	var scheduler *SchedulerState
	if sched := r.scheduler.Load(); sched != nil {
		scheduler = &SchedulerState{
			Info:       sched.info,
			Registered: sched.registered,
			FatalError: sched.fatalError,
		}
	}

	return &RunnerState{
		LastMetrics:           r.lastMetrics,
		Scheduler:             scheduler,
		ComputeUnit:           r.computeUnit,
		LastApproved:          r.lastApproved,
		LastSchedulerError:    r.lastSchedulerError,
		VM:                    r.vm,
		PodIP:                 r.podIP,
		BackgroundWorkerCount: r.backgroundWorkerCount.Load(),

		SchedulerRespondedWithMigration: r.schedulerRespondedWithMigration,
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
						Error:    fmt.Errorf("Runner %v panicked: %v", r.vm.NamespacedName(), err),
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

	execLogger := logger.Named("exec")

	coreExecLogger := execLogger.Named("core")
	executorCore := executor.NewExecutorCore(coreExecLogger.Named("state"), r.vm, executor.Config{
		DefaultScalingConfig:           r.global.config.Scaling.DefaultConfig,
		PluginRequestTick:              time.Second * time.Duration(r.global.config.Scheduler.RequestAtLeastEverySeconds),
		PluginDeniedRetryWait:          time.Second * time.Duration(r.global.config.Scheduler.RetryDeniedUpscaleSeconds),
		MonitorDeniedDownscaleCooldown: time.Second * time.Duration(r.global.config.Monitor.RetryDeniedDownscaleSeconds),
		MonitorRetryWait:               time.Second * time.Duration(r.global.config.Monitor.RetryFailedRequestSeconds),
		Warn: func(msg string, args ...any) {
			coreExecLogger.Warn(fmt.Sprintf(msg, args...))
		},
	})

	pluginIface := makePluginInterface(r, executorCore)
	neonvmIface := makeNeonVMInterface(r)
	monitorIface := makeMonitorInterface(r, executorCore)

	// "ecwc" stands for "ExecutorCoreWithClients"
	ecwc := executorCore.WithClients(executor.ClientSet{
		Plugin:  pluginIface,
		NeonVM:  neonvmIface,
		Monitor: monitorIface,
	})

	logger.Info("Starting background workers")

	// FIXME: make these timeouts/delays separately defined constants, or configurable
	mainDeadlockChecker := r.lock.DeadlockChecker(250*time.Millisecond, time.Second)
	reqDeadlockChecker := r.requestLock.DeadlockChecker(5*time.Second, time.Second)

	r.spawnBackgroundWorker(ctx, logger, "podStatus updater", func(c context.Context, l *zap.Logger) {
		r.status.periodicallyRefreshState(c, l, r.global)
	})
	r.spawnBackgroundWorker(ctx, logger, "deadlock checker (main)", ignoreLogger(mainDeadlockChecker))
	r.spawnBackgroundWorker(ctx, logger, "deadlock checker (request lock)", ignoreLogger(reqDeadlockChecker))
	r.spawnBackgroundWorker(ctx, logger, "track scheduler", func(c context.Context, l *zap.Logger) {
		r.trackSchedulerLoop(c, l, scheduler, schedulerWatch, func(withLock func()) {
			ecwc.Updater().NewScheduler(withLock)
		})
	})
	r.spawnBackgroundWorker(ctx, logger, "get metrics", func(c context.Context, l *zap.Logger) {
		r.getMetricsLoop(c, l, func(metrics api.Metrics, withLock func()) {
			ecwc.Updater().UpdateMetrics(metrics, withLock)
		})
	})
	r.spawnBackgroundWorker(ctx, logger.Named("vm-monitor"), "vm-monitor reconnection loop", func(c context.Context, l *zap.Logger) {
		r.connectToMonitorLoop(c, l, monitorStateCallbacks{
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
// Every time metrics are successfully fetched, the value of r.lastMetrics is updated and newMetrics
// is signalled. The update to r.lastMetrics and signal on newMetrics occur without releasing r.lock
// in between.
func (r *Runner) getMetricsLoop(
	ctx context.Context,
	logger *zap.Logger,
	newMetrics func(metrics api.Metrics, withLock func()),
) {
	timeout := time.Second * time.Duration(r.global.config.Metrics.RequestTimeoutSeconds)
	waitBetweenDuration := time.Second * time.Duration(r.global.config.Metrics.SecondsBetweenRequests)

	// FIXME: make this configurable
	minWaitDuration := time.Second

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
		waitBetween := time.After(waitBetweenDuration)
		minWait := time.After(minWaitDuration)

		select {
		case <-ctx.Done():
			return
		case <-minWait:
		}

		select {
		case <-ctx.Done():
			return
		case <-waitBetween:
		}
	}
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
					r.monitor.Store(nil)
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
				r.monitor.Store(dispatcher)
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
	newScheduler func(withLock func()),
) {
	// pre-declare a bunch of variables because we have some gotos here.
	var (
		lastStart   time.Time
		minWait     time.Duration    = 5 * time.Second // minimum time we have to wait between scheduler starts
		okForNew    <-chan time.Time                   // channel that sends when we've waited long enough for a new scheduler
		currentInfo schedwatch.SchedulerInfo
		fatal       util.SignalReceiver[struct{}]
		failed      bool
	)

	if init == nil {
		goto waitForNewScheduler
	}

	currentInfo = *init

startScheduler:
	schedulerWatch.ExpectingDeleted()

	lastStart = time.Now()
	okForNew = time.After(minWait)
	failed = false

	// Set the current scheduler
	fatal = func() util.SignalReceiver[struct{}] {
		logger := logger.With(zap.Object("scheduler", currentInfo))

		verb := "Setting"
		if init == nil || init.UID != currentInfo.UID {
			verb = "Updating"
		}

		sendFatal, recvFatal := util.NewSingleSignalPair[struct{}]()

		sched := &Scheduler{
			runner:     r,
			info:       currentInfo,
			registered: false,
			fatalError: nil,
			fatal:      sendFatal,
		}

		r.lock.Lock()
		defer r.lock.Unlock()

		newScheduler(func() {
			logger.Info(fmt.Sprintf("%s scheduler pod", verb))

			r.scheduler.Store(sched)
			r.lastSchedulerError = nil
		})

		return recvFatal
	}()

	// Start watching for the current scheduler to be deleted or have fatally errored
	for {
		select {
		case <-ctx.Done():
			return
		case <-fatal.Recv():
			logger.Info(
				"Waiting for new scheduler because current fatally errored",
				zap.Object("scheduler", currentInfo),
			)
			failed = true
			goto waitForNewScheduler
		case info := <-schedulerWatch.Deleted:
			matched := func() bool {
				r.lock.Lock()
				defer r.lock.Unlock()

				scheduler := r.scheduler.Load()

				if scheduler.info.UID != info.UID {
					logger.Info(
						"Scheduler candidate pod was deleted, but we aren't using it yet",
						zap.Object("scheduler", scheduler.info), zap.Object("candidate", info),
					)
					return false
				}

				logger.Info(
					"Scheduler pod was deleted. Aborting further communication",
					zap.Object("scheduler", scheduler.info),
				)

				r.scheduler.Store(nil)
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
			var endingMode string
			if failed {
				endingMode = "failed"
			} else {
				endingMode = "ended"
			}

			// Not ready yet; let's log something about it:
			logger.Info(
				fmt.Sprintf("Scheduler %s quickly. Respecting minimum delay before switching to a new one", endingMode),
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
	_, err = r.global.vmClient.NeonvmV1().VirtualMachines(r.vm.Namespace).
		Patch(requestCtx, r.vm.Name, ktypes.JSONPatchType, patchPayload, metav1.PatchOptions{})

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
		byteTotal := r.vm.Mem.SlotSize.Value() * int64(abs.Mem)
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
	rawResources := target.ConvertToAllocation(r.vm.Mem.SlotSize)

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
	rawResources := target.ConvertToAllocation(r.vm.Mem.SlotSize)

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
		return nil, s.handlePreRequestError(fmt.Errorf("Error encoding request JSON: %w", err))
	}

	timeout := time.Second * time.Duration(s.runner.global.config.Scaling.RequestTimeoutSeconds)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/", s.info.IP, s.runner.global.config.Scheduler.RequestPort)

	request, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, s.handlePreRequestError(fmt.Errorf("Error building request to %q: %w", url, err))
	}
	request.Header.Set("content-type", "application/json")

	logger.Info("Sending request to scheduler", zap.Any("request", reqData))

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		description := fmt.Sprintf("[error doing request: %s]", util.RootError(err))
		s.runner.global.metrics.schedulerRequests.WithLabelValues(description).Inc()
		return nil, s.handleRequestError(reqData, fmt.Errorf("Error doing request: %w", err))
	}
	defer response.Body.Close()

	s.runner.global.metrics.schedulerRequests.WithLabelValues(strconv.Itoa(response.StatusCode)).Inc()

	respBody, err := io.ReadAll(response.Body)
	if err != nil {
		var handle func(*api.AgentRequest, error) error
		if response.StatusCode == 200 {
			handle = s.handleRequestError
		} else {
			// if status != 200, fatal for the same reasons as the != 200 check lower down
			handle = s.handleFatalError
		}

		return nil, handle(reqData, fmt.Errorf("Error reading body for response: %w", err))
	}

	if response.StatusCode != 200 {
		// Fatal because 4XX implies our state doesn't match theirs, 5XX means we can't assume
		// current contents of the state, and anything other than 200, 4XX, or 5XX shouldn't happen
		return nil, s.handleFatalError(
			reqData,
			fmt.Errorf("Received response status %d body %q", response.StatusCode, string(respBody)),
		)
	}

	var respData api.PluginResponse
	if err := json.Unmarshal(respBody, &respData); err != nil {
		// Fatal because invalid JSON might also be semantically invalid
		return nil, s.handleRequestError(reqData, fmt.Errorf("Bad JSON response: %w", err))
	}

	logger.Info("Received response from scheduler", zap.Any("response", respData))

	return &respData, nil
}

// handlePreRequestError appropriately handles updating the Scheduler and its Runner's state to
// reflect that an error occurred. It returns the error passed to it
//
// This method will update s.runner.lastSchedulerError if s.runner.scheduler == s.
//
// This method MUST be called while holding s.runner.requestLock AND NOT s.runner.lock.
func (s *Scheduler) handlePreRequestError(err error) error {
	if err == nil {
		panic(errors.New("handlePreRequestError called with nil error"))
	}

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	if s.runner.scheduler.Load() == s {
		s.runner.lastSchedulerError = err
	}

	return err
}

// handleRequestError appropriately handles updating the Scheduler and its Runner's state to reflect
// that an error occurred while making a request. It returns the error passed to it
//
// This method will update s.runner.{lastApproved,lastSchedulerError} if s.runner.scheduler == s.
//
// This method MUST be called while holding s.runner.requestLock AND NOT s.runner.lock.
func (s *Scheduler) handleRequestError(req *api.AgentRequest, err error) error {
	if err == nil {
		panic(errors.New("handleRequestError called with nil error"))
	}

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	if s.runner.scheduler.Load() == s {
		s.runner.lastSchedulerError = err

		// Because downscaling s.runner.vm must be done before any request that decreases its
		// resources, any request greater than the current usage must be an increase, which the
		// scheduler may or may not have approved. So: If we decreased and the scheduler failed, we
		// can't assume it didn't register the decrease. If we want to increase and the scheduler
		// failed, we can't assume it *did* register the increase. In both cases, the registered
		// state for a well-behaved scheduler will be >= our state.
		//
		// note: this is also replicated below, in handleFatalError.
		lastApproved := s.runner.vm.Using()
		s.runner.lastApproved = &lastApproved
	}

	return err
}

// handleError appropriately handles updating the Scheduler and its Runner's state to reflect that
// a fatal error occurred. It returns the error passed to it
//
// This method will update s.runner.{lastApproved,lastSchedulerError} if s.runner.scheduler == s, in
// addition to s.fatalError.
//
// This method MUST be called while holding s.runner.requestLock AND NOT s.runner.lock.
func (s *Scheduler) handleFatalError(req *api.AgentRequest, err error) error {
	if err == nil {
		panic(errors.New("handleFatalError called with nil error"))
	}

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	s.fatalError = err

	if s.runner.scheduler.Load() == s {
		s.runner.lastSchedulerError = err
		// for reasoning on lastApproved, see handleRequestError.
		lastApproved := s.runner.vm.Using()
		s.runner.lastApproved = &lastApproved
	}

	return err
}
