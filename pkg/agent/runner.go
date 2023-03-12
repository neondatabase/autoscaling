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
//  4. Some high-level actions (e.g., HTTP request to Informant; update VM to desired state) require
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
//       scheduler, informant, and NeonVM -- the "scaling" part of "autoscaling".
//     * "informant server loop" - keeps Runner.informant and Runner.server up-to-date.
//     * ... and a few more.
//  * Each thread makes *synchronous* HTTP requests while holding the necessary lock to prevent any other
//    thread from making HTTP requests to the same entity. For example:
//    * Runner has Runner.vmStateLock that guards the abstract flow of "updating VM resources if we
//      need to, according to current metrics".
//  * Each "background" thread is spawned by (*Runner).spawnBackgroundWorker(), which appropriately
//    catches panics and signals the Runner so that the main thread from (*Runner).Run() cleanly
//    shuts everything down.
//  * Every significant lock has an associated "deadlock checker" background thread that panics if
//    it takes too long to acquire the lock.
//
// spawnBackgroundWorker guarantees (1) and (2); Runner.lock makes (3) possible; and
// Runner.vmStateLock and others guarantee (4).
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
	"runtime"
	"time"

	"github.com/sharnoff/chord"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/task"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// PluginProtocolVersion is the current version of the agent<->scheduler plugin in use by this
// autoscaler-agent.
//
// Currently, each autoscaler-agent supports only one version at a time. In the future, this may
// change.
const PluginProtocolVersion api.PluginProtoVersion = api.PluginProtoV1_1

// Runner is per-VM Pod god object responsible for handling everything
//
// It primarily operates as a source of shared data for a number of long-running tasks. For
// additional general information, refer to the comment at the top of this file.
type Runner struct {
	global *agentState
	// status provides the high-level status of the Runner. Reading or updating the status requires
	// holding podStatus.lock. Updates are typically done handled by the setStatus method.
	status *podStatus

	// logger is the shared logger for this Runner, giving all log lines a unique, relevant prefix
	logger RunnerLogger

	// vm is non-nil for the duration of all background tasks, but is expected to be nil before
	// r.getInitialVMInfo is called by (*Runner).Run
	//
	// FIXME: the value of vm.Using() is sometimes inaccurate - we need to prematurely mark
	// resources as being used so that a new scheduler won't send a Register request at the same
	// time as we're updating the Informant/NeonVM with an increase that was already approved. This
	// is also tied to handling migration; there's a comment specifically about migration that
	// should handle this.
	//
	// This field MUST NOT be updated without holding BOTH lock AND vmStateLock.
	//
	// This field MAY be read while holding EITHER lock OR vmStateLock.
	vm      *api.VmInfo
	podName api.PodName
	podIP   string

	// lock guards the values of all mutable fields. The immutable fields are:
	// - global
	// - status
	// - vmStateLock
	// - podName
	// - podIP
	// - logger
	// lock MUST NOT be held while interacting with the network. The appropriate synchronization to
	// ensure we don't send conflicting requests is provided by schedulerRequestLock and
	// vmStateLock.
	lock util.ChanMutex

	// vmStateLock must be held across any set of requests that seeks to change the VM's resource
	// allocations.
	//
	// If both scheduler.requestLock and vmStateLock are required, then vmStateLock MUST be acquired
	// before scheduler.requestLock.
	//
	// vmStateLock DOES NOT protect vm.Cpu, vm.Mem, or any of its other fields. It ONLY serves to
	// coordinate requests so that we don't have multiple threads trying to set its values to
	// different things at the same time.
	vmStateLock util.ChanMutex

	// lastMetrics stores the most recent metrics we've received from the VM
	//
	// This field is exclusively set by the getMetricsLoop background worker, and will never change
	// from non-nil to nil. The data behind each pointer is immutable, but the value of the pointer
	// itself is not.
	lastMetrics *api.Metrics
	// requestedUpscale provides information about any requested upscaling by a VM informant
	//
	// This value is reset whenever we start a new informant server
	requestedUpscale api.MoreResources

	// scheduler is the current scheduler that we're communicating with, or nil if there isn't one.
	// Each scheduler's info field is immutable. When a scheduler is replaced, only the pointer
	// value here is updated; the original Scheduler remains unchanged.
	scheduler *Scheduler
	server    *InformantServer
	// informant holds the most recent InformantDesc that an InformantServer has received in its
	// normal operation. If there has been at least one InformantDesc received, this field will not
	// be nil.
	//
	// This field really should not be used except for providing RunnerState. The correct interface
	// is through server.Informant(), which does all the appropriate error handling if the
	// connection to the informant is not in a suitable state.
	informant *api.InformantDesc
	// computeUnit is the latest Compute Unit reported by a scheduler. It may be nil, if we haven't
	// been able to contact one yet.
	computeUnit *api.Resources

	// lastApproved is the last resource allocation that a scheduler has approved. It may be nil, if
	// we haven't been able to contact one yet.
	lastApproved *api.Resources

	// lastSchedulerError provides the error that occurred - if any - during the most recent request
	// to the current scheduler. This field is not nil only when scheduler is not nil.
	lastSchedulerError error

	// lastInformantError provides the error that occurred - if any - during the most recent request
	// to the VM informant.
	//
	// This field MUST NOT be updated without holding BOTH lock AND server.requestLock.
	lastInformantError error
}

// Scheduler stores relevant state for a particular scheduler that a Runner is (or has) connected to
//
// Each Scheduler has an associated background worker that checks for deadlocks and panics if it
// cannot acquire requestLock within a reasonable delay.
//
// Scheduler has some magic to clean up its deadlock checker. Here's what's going on: Basically, a
// pointer to schedulerInternal is expected to function /kind of/ like a weak pointer to Scheduler.
// All non-background usage is through Scheduler, and all background usage is through
// schedulerInternal - so we attach a finalizer to Scheduler that cancels the context of the
// background workers once the Scheduler gets GC'd.
//
// Any direct usage of schedulerInternal outside of the construction of a Scheduler MUST be
// considered invalid, and rejected as such.
type Scheduler struct {
	*schedulerInternal
}

type schedulerInternal struct {
	// runner is the parent Runner interacting with this Scheduler instance
	//
	// This field is immutable but the data behind the pointer is not.
	runner *Runner

	logger RunnerLogger

	// info holds the immutable information we use to connect to and describe the scheduler
	info schedulerInfo

	// registered is true only once a call to this Scheduler's Register() method has been made
	//
	// All methods that make a request to the scheduler will first call Register() if registered is
	// false.
	//
	// This field MUST NOT be updated without holding BOTH requestLock AND runner.lock
	//
	// This field MAY be read while holding EITHER requestLock OR runner.lock.
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
	// This field MUST NOT be updated without holding BOTH requestLock AND runner.lock.
	//
	// This field MAY be read while holding EITHER requestLock OR runner.lock.
	fatalError error

	// fatal is used for signalling that fatalError has been set (and so we should look for a new
	// scheduler)
	fatal util.SignalSender

	// requestLock must be held for the duration of any request to this scheduler.
	//
	// If both requestLock and runner.lock are required, requestLock MUST be acquired first. This
	// means that typical acquisition of both, given only the Runner, has the following flow:
	//
	//    sched := func() *Scheduler {
	//        r.lock.Lock()
	//        defer r.lock.Unlock()
	//        return r.scheduler
	//    }
	//
	//    if sched == nil { /* error handling */ }
	//
	//    sched.requestLock.Lock()
	//    defer sched.requestLock.Unlock()
	//    // re-acquire r.lock:
	//    r.lock.Lock()
	//    defer r.lock.Unlock()
	//
	// Of course, this complication does have a distinct benefit: It means that - while holding
	// requestLock - functions are allowed to release and re-acquire runner.lock.
	requestLock util.ChanMutex
}

// RunnerState is the serializable state of the Runner, extracted by its State method
type RunnerState struct {
	LogPrefix          string                `json:"logPrefix"`
	PodIP              string                `json:"podIP"`
	VM                 api.VmInfo            `json:"vm"`
	LastMetrics        *api.Metrics          `json:"lastMetrics"`
	Scheduler          *SchedulerState       `json:"scheduler"`
	Server             *InformantServerState `json:"server"`
	Informant          *api.InformantDesc    `json:"informant"`
	ComputeUnit        *api.Resources        `json:"computeUnit"`
	LastApproved       *api.Resources        `json:"lastApproved"`
	LastSchedulerError error                 `json:"lastSchedulerError"`
	LastInformantError error                 `json:"lastInformantError"`
	Tasks              task.TaskTree         `json:"tasks"`
}

// SchedulerState is the state of a Scheduler, constructed as part of a Runner's State Method
type SchedulerState struct {
	LogPrefix  string        `json:"logPrefix"`
	Info       schedulerInfo `json:"info"`
	Registered bool          `json:"registered"`
	FatalError error         `json:"fatalError"`
}

func (r *Runner) State(groupHandle task.SubgroupHandle) RunnerState {
	r.lock.Lock()
	defer r.lock.Unlock()

	var scheduler *SchedulerState
	if r.scheduler != nil {
		scheduler = &SchedulerState{
			LogPrefix:  r.scheduler.logger.prefix,
			Info:       r.scheduler.info,
			Registered: r.scheduler.registered,
			FatalError: r.scheduler.fatalError,
		}
	}

	var serverState *InformantServerState
	if r.server != nil {
		serverState = &InformantServerState{
			Desc:            r.server.desc,
			SeqNum:          r.server.seqNum,
			ReceivedIDCheck: r.server.receivedIDCheck,
			MadeContact:     r.server.madeContact.Load(),
			ProtoVersion:    r.server.protoVersion,
			Mode:            r.server.mode,
			ExitStatus:      r.server.exitStatus,
		}
	}

	return RunnerState{
		LastMetrics:        r.lastMetrics,
		Scheduler:          scheduler,
		Server:             serverState,
		Informant:          r.informant,
		ComputeUnit:        r.computeUnit,
		LastApproved:       r.lastApproved,
		LastSchedulerError: r.lastSchedulerError,
		LastInformantError: r.lastInformantError,
		VM:                 *r.vm,
		PodIP:              r.podIP,
		LogPrefix:          r.logger.prefix,
		Tasks:              groupHandle.TaskTree(),
	}
}

func MakeShutdownContext() (context.Context, context.CancelFunc) {
	// TODO: make this timeout configurable
	return context.WithTimeout(context.TODO(), 10*time.Second)
}

func (r *Runner) Spawn(tm task.Manager, vmName string) task.SubgroupHandle {
	// Handle panics by marking ourselves as such and shutting down everything else
	tm = tm.WithPanicHandler(func(taskName string, stackTrace chord.StackTrace) {
		r.setStatus(func(stat *podStatus) {
			stat.panicked = true
			stat.done = true
			stat.errored = fmt.Errorf("task %s panicked", taskName)
		})
		task.LogPanicAndShutdown(tm, MakeShutdownContext)
	})
	// Don't use the top-level error handler
	tm = tm.WithShutdownErrorHandler(nil)

	return tm.SpawnAsSubgroup(fmt.Sprintf("runner-%v", r.podName), func(tm task.Manager) {
		err := r.Run(tm, vmName)

		r.setStatus(func(stat *podStatus) {
			r.status.done = true
			// don't overwrite a prior panic
			if r.status.errored == nil {
				r.status.errored = err
			}
		})

		if err != nil {
			r.logger.Errorf("Ended with error: %s", err)
		} else {
			r.logger.Infof("Ended without error")
		}
	})
}

func (r *Runner) setStatus(with func(*podStatus)) {
	r.status.lock.Lock()
	defer r.status.lock.Unlock()
	with(r.status)
}

// Run is the main entrypoint to the long-running per-VM pod tasks
func (r *Runner) Run(tm task.Manager, vmName string) (err error) {
	defer func() {
		ctx, cancel := MakeShutdownContext()
		defer cancel()
		shutdownErr := tm.Shutdown(ctx)
		if err == nil && shutdownErr != nil {
			err = shutdownErr
		}
	}()

	initVmInfo, err := r.getInitialVMInfo(tm.Context(), vmName)
	if err != nil {
		return
	}

	// Updating r.vm has to be while r.lock is held, in case r.State() is concurrently called
	func() {
		r.lock.Lock()
		defer r.lock.Unlock()

		r.vm = initVmInfo
	}()

	schedulerWatch, scheduler, err := watchSchedulerUpdates(
		tm, r.logger, r.global.schedulerEventBroker, r.global.schedulerStore,
	)
	if err != nil {
		return fmt.Errorf("Error starting scheduler watcher: %w", err)
	}

	if scheduler == nil {
		r.logger.Warningf("No initial scheduler found")
	} else {
		r.logger.Infof(
			"Got initial scheduler pod %v (UID = %v) with IP %v",
			scheduler.PodName, scheduler.UID, scheduler.IP,
		)
		schedulerWatch.Using(*scheduler)
	}

	// signal when r.lastMetrics is updated
	sendMetricsSignal, recvMetricsSignal := util.NewCondChannelPair()
	// signal when new schedulers are *registered*
	sendSchedSignal, recvSchedSignal := util.NewCondChannelPair()
	// signal when r.informant is updated
	sendInformantUpd, recvInformantUpd := util.NewCondChannelPair()
	// signal when the informant requests upscaling
	sendUpscaleRequested, recvUpscaleRequested := util.NewCondChannelPair()

	r.logger.Infof("Starting background workers")

	// FIXME: make these timeouts/delays separately defined constants, or configurable
	mainDeadlockChecker := r.lock.DeadlockChecker(250*time.Millisecond, time.Second)
	vmDeadlockChecker := r.vmStateLock.DeadlockChecker(30*time.Second, time.Second)

	tm.Spawn("deadlock-checker-main", mainDeadlockChecker)
	tm.Spawn("deadlock-checker-VM", vmDeadlockChecker)
	tm.Spawn("track-scheduler", func(tm task.Manager) {
		r.trackSchedulerLoop(tm, scheduler, schedulerWatch, sendSchedSignal)
	})
	tm.Spawn("get-metrics", func(tm task.Manager) {
		r.getMetricsLoop(tm.Context(), sendMetricsSignal, recvInformantUpd)
	})
	tm.Spawn("handle-VM-resources", func(tm task.Manager) {
		r.handleVMResources(tm.Context(), recvMetricsSignal, recvUpscaleRequested, recvSchedSignal)
	})
	tm.Spawn("informant server loop", func(tm task.Manager) {
		r.serveInformantLoop(tm, sendInformantUpd, sendUpscaleRequested)
	})

	// Note: Run doesn't terminate unless the parent context is cancelled - either because the VM
	// pod was deleted, or the autoscaler-agent is exiting.
	<-tm.Context().Done()
	return nil
}

func (r *Runner) getInitialVMInfo(ctx context.Context, vmName string) (*api.VmInfo, error) {
	// In order to smoothly handle cases where the VM is missing, we perform a List request instead
	// of a Get, with a FieldSelector that limits the result just to the target VM, if it exists.

	name := api.PodName{Name: vmName, Namespace: r.podName.Namespace}

	opts := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", name.Name),
	}
	list, err := r.global.vmClient.NeonvmV1().VirtualMachines(name.Namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("Error listing VM %v: %w", name, err)
	}

	if len(list.Items) > 1 {
		return nil, fmt.Errorf("List for VM %v returned > 1 item", name)
	} else if len(list.Items) == 0 {
		return nil, nil
	}

	vmInfo, err := api.ExtractVmInfo(&list.Items[0])
	if err != nil {
		return nil, fmt.Errorf("Error extracting VmInfo from %v: %w", name, err)
	}

	return vmInfo, nil
}

//////////////////////
// Background tasks //
//////////////////////

// getMetricsLoop repeatedly attempts to fetch metrics from the VM
//
// Every time metrics are successfully fetched, the value of r.lastMetrics is updated and newMetrics
// is signalled. The update to r.lastMetrics and signal on newMetrics occur without releasing r.lock
// in between.
func (r *Runner) getMetricsLoop(
	ctx context.Context,
	newMetrics util.CondChannelSender,
	updatedInformant util.CondChannelReceiver,
) {
	timeout := time.Second * time.Duration(r.global.config.Metrics.RequestTimeoutSeconds)
	waitBetweenDuration := time.Second * time.Duration(r.global.config.Metrics.SecondsBetweenRequests)

	// FIXME: make this configurable
	minWaitDuration := time.Second

	for {
		metrics, err := r.doMetricsRequestIfEnabled(ctx, timeout, updatedInformant.Consume)
		if err != nil {
			r.logger.Errorf("Error making metrics request: %s", err)
			goto next
		} else if metrics == nil {
			goto next
		}

		r.logger.Infof("Got metrics %+v", *metrics)

		func() {
			r.lock.Lock()
			defer r.lock.Unlock()
			r.lastMetrics = metrics
			newMetrics.Send()
		}()

	next:
		waitBetween := time.After(waitBetweenDuration)
		minWait := time.After(minWaitDuration)

		select {
		case <-ctx.Done():
			return
		case <-minWait:
		}

		// After waiting for the required minimum, allow shortcutting the normal wait if the
		// informant was updated
		select {
		case <-ctx.Done():
			return
		case <-updatedInformant.Recv():
			r.logger.Infof("Shortcutting normal metrics wait because informant was updated")
		case <-waitBetween:
		}

	}
}

// handleVMResources is the primary background worker responsible for updating the desired state of
// the VM and communicating with the other components to make that happen, if possible.
//
// A new desired state is calculated when signalled on updatedMetrics or newScheduler.
//
// It may not be obvious at first, so: The reason why we try again when signalled on newScheduler,
// even though scheduler registration is handled separately, is that we might've had a prior desired
// increase that wasn't possible at the time (because the scheduler was unavailable) but is now
// possible, without the metrics being updated.
func (r *Runner) handleVMResources(
	ctx context.Context,
	updatedMetrics util.CondChannelReceiver,
	upscaleRequested util.CondChannelReceiver,
	registeredScheduler util.CondChannelReceiver,
) {
	for {
		var reason VMUpdateReason

		select {
		case <-ctx.Done():
			return
		case <-updatedMetrics.Recv():
			reason = UpdatedMetrics
		case <-upscaleRequested.Recv():
			reason = UpscaleRequested
		case <-registeredScheduler.Recv():
			reason = RegisteredScheduler
		}

		// FIXME: make maxRetries configurable
		maxRetries := uint(1)
		err := r.updateVMResources(
			ctx, reason, maxRetries, updatedMetrics.Consume, registeredScheduler.Consume,
		)
		if err != nil {
			if ctx.Err() != nil {
				r.logger.Warningf("Error updating VM resources: %s", err)
				return
			}

			r.logger.Errorf("Error updating VM resources: %s", err)
		}
	}
}

// serveInformantLoop repeatedly creates an InformantServer to handle communications with the VM
// informant
//
// This function directly sets the value of r.server and indirectly sets r.informant.
func (r *Runner) serveInformantLoop(
	tm task.Manager,
	updatedInformant util.CondChannelSender,
	upscaleRequested util.CondChannelSender,
) {
	// variables set & accessed across loop iterations
	var (
		normalRetryWait <-chan time.Time
		minRetryWait    <-chan time.Time
		lastStart       time.Time
	)

	// Loop-invariant duration constants
	minWait := time.Second * time.Duration(r.global.config.Informant.RetryServerMinWaitSeconds)
	normalWait := time.Second * time.Duration(r.global.config.Informant.RetryServerNormalWaitSeconds)
	retryRegister := time.Second * time.Duration(r.global.config.Informant.RegisterRetrySeconds)

retryServer:
	for {
		// On each (re)try, unset the informant's requested upscale. We need to do this *before*
		// starting the server, because otherwise it's possible for a racy /try-upscale request to
		// sneak in before we reset it, which would cause us to incorrectly ignore the request.
		if upscaleRequested.Unsend() {
			r.logger.Infof("Cancelled existing 'upscale requested' signal due to informant server restart")
		}

		if normalRetryWait != nil {
			r.logger.Infof("Retrying informant server in %s", normalWait)
			select {
			case <-tm.Context().Done():
				return
			case <-normalRetryWait:
			}
		}

		if minRetryWait != nil {
			select {
			case <-minRetryWait:
				r.logger.Infof("Retrying informant server")
			default:
				r.logger.Infof(
					"Informant server ended quickly, only %s ago. Respecting minimum wait of %s",
					time.Since(lastStart), minWait,
				)
				select {
				case <-tm.Context().Done():
					return
				case <-minRetryWait:
				}
			}
		}

		normalRetryWait = nil // only "long wait" if an error occurred
		minRetryWait = time.After(minWait)
		lastStart = time.Now()

		server, exited, err := NewInformantServer(tm, r, updatedInformant, upscaleRequested)
		if tm.Context().Err() != nil {
			if err != nil {
				r.logger.Warningf("Error starting informant server, context cancelled: %s", err)
			}
			return
		} else if err != nil {
			normalRetryWait = time.After(normalWait)
			r.logger.Errorf("Error starting informant server: %s", err)
			continue retryServer
		}

		// Update r.server:
		func() {
			r.lock.Lock()
			defer r.lock.Unlock()

			var kind string
			if r.server == nil {
				kind = "Setting"
			} else {
				kind = "Updating"
			}

			r.logger.Infof("%s initial informant server, desc = %+v", kind, server.desc)
			r.server = server
		}()

		r.logger.Infof("Registering with informant")

		// Try to register with the informant:
	retryRegister:
		for {
			err := server.RegisterWithInformant(tm.Context())
			if err == nil {
				break // all good; wait for the server to finish.
			} else if tm.Context().Err() != nil {
				if err != nil {
					r.logger.Warningf("Error registering with informant, context cancelled: %s", err)
				}
				return
			}

			// Server exited; can't just retry registering.
			if server.ExitStatus() != nil {
				normalRetryWait = time.After(normalWait)
				continue retryServer
			}

			// Wait before retrying registering
			r.logger.Infof("Retrying registering with informant after %s", retryRegister)
			select {
			case <-time.After(retryRegister):
				continue retryRegister
			case <-tm.Context().Done():
				return
			}
		}

		// Wait for the server to finish
		select {
		case <-tm.Context().Done():
			return
		case <-exited.Recv():
		}

		// Server finished
		exitStatus := server.ExitStatus()
		if exitStatus == nil {
			panic(errors.New("Informant server signalled end but ExitStatus() == nil"))
		}

		if !exitStatus.RetryShouldFix {
			normalRetryWait = time.After(normalWait)
		}

		continue retryServer
	}
}

// trackSchedulerLoop listens on the schedulerWatch, keeping r.scheduler up-to-date and signalling
// on registeredScheduler whenever a new Scheduler is successfully registered
func (r *Runner) trackSchedulerLoop(
	tm task.Manager,
	init *schedulerInfo,
	schedulerWatch schedulerWatch,
	registeredScheduler util.CondChannelSender,
) {
	// pre-declare a bunch of variables because we have some gotos here.
	var (
		lastStart   time.Time
		minWait     time.Duration    = 5 * time.Second // minimum time we have to wait between scheduler starts
		okForNew    <-chan time.Time                   // channel that sends when we've waited long enough for a new scheduler
		currentInfo schedulerInfo
		fatal       util.SignalReceiver
		newFatal    chan util.SignalReceiver = make(chan util.SignalReceiver)
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

	r.logger.Infof(
		"Updating scheduler to pod %v (UID = %s) with IP %s",
		currentInfo.PodName, currentInfo.UID, currentInfo.IP,
	)

	// Set the current scheduler
	//
	// each scheduler's handling runs in its own task "subgroup", so their cleanup can be bundled
	// together.
	tm.SpawnAsSubgroup(fmt.Sprintf("schedloop-%s", currentInfo.UID), func(tm task.Manager) {
		sendFatal, recvFatal := util.NewSingleSignalPair()

		// the blocking receive on newFatal guarantees we can read currentInfo.UID without holding a lock
		logger := RunnerLogger{
			prefix: fmt.Sprintf("%sScheduler %s: ", r.logger.prefix, currentInfo.UID),
		}

		sched := &Scheduler{
			&schedulerInternal{
				runner:      r,
				logger:      logger,
				info:        currentInfo,
				registered:  false,
				fatalError:  nil,
				fatal:       sendFatal,
				requestLock: util.NewChanMutex(),
			},
		}

		// FIXME: make these timeouts/delays separately defined constants, or configurable
		tm.Spawn("deadlock-checker", sched.requestLock.DeadlockChecker(5*time.Second, time.Second))
		// shut down the deadlock checker once the object is no longer in use
		runtime.SetFinalizer(sched, func(obj any) {
			ctx, cancel := MakeShutdownContext()
			defer cancel()
			if err := tm.Shutdown(ctx); err != nil {
				logger.Errorf("Error shutting down scheduler: %s", err)
			}
		})

		// Acquire the requestLock for the initial sched.Register(). Responsibility for unlocking it
		// is passed to the goroutine.
		select {
		case <-sched.requestLock.WaitLock():
		default:
			panic(errors.New("call to sched.requestLock.Lock() immediately after construction should succeed"))
		}

		responsibleForRequestLock := true
		defer func() {
			if responsibleForRequestLock {
				sched.requestLock.Unlock()
			}
		}()

		func() {
			r.lock.Lock()
			defer r.lock.Unlock()

			r.scheduler = sched
			r.lastSchedulerError = nil
		}()

		// only send after we've updated r.scheduler
		newFatal <- recvFatal

		responsibleForRequestLock = false
		tm.Spawn("Register()", func(tm task.Manager) {
			defer sched.requestLock.Unlock()

			if err := sched.Register(tm.Context(), registeredScheduler.Send); err != nil {
				if tm.Context().Err() != nil {
					logger.Warningf("Error registering (but context is done): %s", err)
				} else {
					logger.Errorf("Error registering: %s", err)
				}
			}
		})
	})

	select {
	case fatal = <-newFatal:
	case <-tm.Context().Done():
		return
	}

	// Start watching for the current scheduler to be deleted or have fatally errored
	for {
		select {
		case <-tm.Context().Done():
			return
		case <-fatal.Recv():
			r.logger.Infof("Waiting for new scheduler because current fatally errored")
			failed = true
			goto waitForNewScheduler
		case info := <-schedulerWatch.Deleted:
			matched := func() bool {
				r.lock.Lock()
				defer r.lock.Unlock()

				if r.scheduler.info.UID != info.UID {
					r.logger.Infof(
						"Scheduler candidate pod %v was deleted, but we aren't using it (its UID = %v, ours = %v)",
						info.PodName, info.UID, r.scheduler.info.UID,
					)
					return false
				}

				r.logger.Infof(
					"Scheduler pod %v (UID = %v) was deleted, ending further communication",
					r.scheduler.info.PodName, r.scheduler.info.UID,
				)

				r.scheduler = nil
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
			r.logger.Infof(
				"Scheduler %s quickly, only %s ago. Respecting minimum wait of %s",
				endingMode, time.Since(lastStart), minWait,
			)
			select {
			case <-tm.Context().Done():
				return
			case <-okForNew:
			}
		}
	}

	// Actually watch for a new scheduler
	select {
	case <-tm.Context().Done():
		return
	case newInfo := <-schedulerWatch.ReadyQueue:
		currentInfo = newInfo
		goto startScheduler
	}
}

//////////////////////////////////////////
// Lower-level implementation functions //
//////////////////////////////////////////

// doMetricsRequestIfEnabled makes a single metrics request to the VM informant, returning it
//
// This method expects that the Runner is not locked.
func (r *Runner) doMetricsRequestIfEnabled(
	ctx context.Context,
	timeout time.Duration,
	clearNewInformantSignal func(),
) (*api.Metrics, error) {
	r.logger.Infof("Attempting metrics request")

	// FIXME: the region where the lock is held should be extracted into a separate method, called
	// something like buildMetricsRequest().

	r.lock.Lock()
	locked := true
	defer func() {
		if locked {
			r.lock.Unlock()
		}
	}()

	// Only clear the signal once we've locked, so that we're not racing.
	//
	// We don't *need* to do this, but its only cost is a small amount of code complexity, and it's
	// nice to have have the guarantees around not racing.
	clearNewInformantSignal()

	if r.server == nil || r.server.mode != InformantServerRunning {
		var state = "unset"
		if r.server != nil {
			state = string(r.server.mode)
		}

		r.logger.Infof("Cannot make metrics request because informant server is %s", state)
		return nil, nil
	}

	if r.informant == nil {
		panic(errors.New("r.informant == nil but r.server.mode == InformantServerRunning"))
	}

	var url string
	var handle func(body []byte) (*api.Metrics, error)

	switch {
	case r.informant.MetricsMethod.Prometheus != nil:
		url = fmt.Sprintf("http://%s:%d/metrics", r.podIP, r.informant.MetricsMethod.Prometheus.Port)
		handle = func(body []byte) (*api.Metrics, error) {
			m, err := api.ReadMetrics(body, r.global.config.Metrics.LoadMetricPrefix)
			if err != nil {
				err = fmt.Errorf("Error reading metrics from prometheus output: %w", err)
			}
			return &m, err
		}
	default:
		// Ok to panic here because this should be handled by the informant server
		panic(errors.New("server's InformantDesc has unknown metrics method"))
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, bytes.NewReader(nil))
	if err != nil {
		panic(fmt.Errorf("Error constructing metrics request to %q: %w", url, err))
	}

	// Unlock while we perform the request:
	locked = false
	r.lock.Unlock()

	r.logger.Infof("Making metrics request to %q", url)

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

	return handle(body)
}

// VMUpdateReason provides context to (*Runner).updateVMResources about why an update to the VM's
// resources has been requested
type VMUpdateReason string

const (
	UpdatedMetrics      VMUpdateReason = "metrics"
	UpscaleRequested    VMUpdateReason = "upscale requested"
	RegisteredScheduler VMUpdateReason = "scheduler"
)

// atomicUpdateState holds some pre-validated data for (*Runner).updateVMResources, fetched
// atomically (i.e. all at once, while holding r.lock) with the (*Runner).atomicState method
//
// Because atomicState is able to return nil when there isn't yet enough information to update the
// VM's resources, some validation is already guaranteed by representing the data without pointers.
type atomicUpdateState struct {
	computeUnit      api.Resources
	metrics          api.Metrics
	vm               api.VmInfo
	lastApproved     api.Resources
	requestedUpscale api.MoreResources
}

// updateVMResources is responsible for the high-level logic that orchestrates a single update to
// the VM's resources - or possibly just informing the scheduler that nothing's changed.
//
// It's possible for r.computeUnit to change between initially calculating the target resources and
// re-acquring the scheduler, to send them. If that happens, maxRetries allows us to reattempt the
// operation up to a fixed number of times, without releasing r.vmStateLock.
//
// This method sometimes returns nil if the reason we couldn't perform the update was solely because
// other information was missing (e.g., we haven't yet contacted a scheduler). In these cases, an
// appropriate message is logged.
func (r *Runner) updateVMResources(
	ctx context.Context,
	reason VMUpdateReason,
	maxRetries uint,
	clearUpdatedMetricsSignal func(),
	clearNewSchedulerSignal func(),
) error {
	// Acquiring this lock *may* take a while, so we'll allow it to be interrupted by ctx
	if err := r.vmStateLock.TryLock(ctx); err != nil {
		return err
	}
	defer r.vmStateLock.Unlock()

	r.logger.Infof("Updating VM resources: reason = %q, maxRetries = %d", reason, maxRetries)

	// FIXME: The body of the loop should be extracted into a separate method.

	// note: in order to have better error/log messages, the actual "retry or exit" check is in the
	// middle of the loop body.
	var retryCount uint
retry:
	for ; ; retryCount += 1 {
		// state variables
		var (
			target api.Resources
			capped api.Resources // target, but capped by r.lastApproved
		)

		state, err := func() (*atomicUpdateState, error) {
			r.lock.Lock()
			defer r.lock.Unlock()

			clearUpdatedMetricsSignal()
			clearNewSchedulerSignal()

			state := r.getStateForVMUpdate(reason)
			if state == nil {
				// if state == nil, the reason why we can't do the operation was already logged.
				return nil, nil
			} else if r.scheduler != nil && r.scheduler.fatalError != nil {
				r.logger.Warningf("Unable to update VM resources because scheduler had a prior fatal error")
				return nil, nil
			}

			// Calculate the current and desired state of the VM
			target = state.desiredVMState(true) // note: this sets the state value in the loop body

			current := state.vm.Using()

			if target != current {
				r.logger.Infof("Target VM state %+v different from current %+v", target, current)
			} else {
				r.logger.Infof("Target VM state is current %+v", target)
			}

			// Check if there's resources that can (or must) be updated before talking to the
			// scheduler.
			//
			// During typical operation, this only occurs when the target state corresponds to fewer
			// compute units than the current state. However, this can also happen when:
			//
			// * lastApproved and target are both greater than the VM's state; or
			// * VM's state doesn't match the compute unit and only one resource is being decreased
			//
			// To make handling these edge-cases smooth, the code here is more generic than typical
			// operation requires.

			// note: r.atomicState already checks the validity of r.lastApproved - namely that it has no
			// values less than r.vm.Using().
			capped = target.Min(state.lastApproved) // note: this sets the state value in the loop body

			// If there's any values with capped.V > current.V, we need to update r.vm BEFORE
			// releasing r.lock, to prevent the following otherwise-possible sequence of events:
			//
			//   1. initial state: old := r.vm.Using(), less than lastApproved
			//   2. us: r.lock.Lock()
			//   3. us: calculate capped := target resources based on usage
			//   4. us: r.lock.Unlock() // <- BAD, we can't do this.
			//   5. sched watcher: receive delete event
			//   6. sched watcher: receive add event for new scheduler
			//   7. sched watcher: r.lock.Lock()
			//   8. sched watcher: store old := r.vm.Using(), start sending a Register request with it
			//   9. sched watcher: r.lock.Unlock()
			//  10. us: send NeonVM request to set value of VM as capped
			//  11. sched watcher: Register request completes, set lastApproved = old (!)
			//
			// The final couple steps would leave us AND the scheduler in an incorrect state. So we
			// need to preemptively update r.vm.
			//
			// FIXME: Ideally, we'd have some notion of "confirmed" and "unconfirmed" values for the
			// VM's state - in case the NeonVM request fails (so we don't overbill, etc).

			preemptiveIncrease := current.Max(capped)
			r.vm.SetUsing(preemptiveIncrease)

			return state, nil
		}()

		// note: state == nil means that there's some other reason we couldn't do the operation that
		// was already logged.
		if err != nil || state == nil {
			return err
		}

		// If there's an update that can be done immediately, do it! Typically, capped will
		// represent the resources we'd like to downscale.
		if capped != state.vm.Using() {
			// If our downscale gets rejected, calculate a new target
			rejectedDownscale := func() (newTarget api.Resources, _ error) {
				target = state.desiredVMState(false /* don't allow downscaling */)
				return target.Min(state.lastApproved), nil
			}

			nowUsing, err := r.doVMUpdate(ctx, state.vm.Using(), capped, rejectedDownscale)
			if err != nil {
				return fmt.Errorf("Error doing VM update 1: %w", err)
			} else if nowUsing == nil {
				// From the comment above doVMUpdate:
				//
				// > If the VM informant is required and unavailable (or becomes unavailable), this
				// > method will: return nil, nil; log an appropriate warning; and reset the VM's
				// > state to its current value.
				//
				// So we should just return nil. We can't update right now, and there isn't anything
				// left to log.
				return nil
			}

			state.vm.SetUsing(*nowUsing)
		}

		// Re-fetch the scheduler, to (a) inform it of the current state, and (b) request an
		// increase, if we want one.
		//
		// If we can't reach the scheduler, then we've already done everything we can. Emit a
		// warning and exit. We'll get notified to retry when a new one comes online.
		//
		// If the value of r.computeUnit has changed (i.e. a new scheduler appeared and changed it),
		// then we might want to retry with the changed computeUnit.
		sched, computeUnit := func() (*Scheduler, *api.Resources) {
			r.lock.Lock()
			defer r.lock.Unlock()

			return r.scheduler, r.computeUnit
		}()

		clearNewSchedulerSignal()

		if computeUnit == nil {
			panic(errors.New("invalid state: computeUnit was previously non-nil but is now nil"))
		} else if sched == nil {
			r.logger.Warningf("Unable to complete updating VM resources: no scheduler registered")
			return nil
		} else if *computeUnit != state.computeUnit {
			if retryCount < maxRetries {
				r.logger.Warningf(
					"Retrying (%d of %d) updating VM resources: compute unit changed from %+v to %+v",
					retryCount+1, maxRetries, state.computeUnit, *computeUnit,
				)
				continue retry
			} else {
				r.logger.Warningf("Couldn't update VM resources: Compute Unit changed but no retries left")
				return nil
			}
		}

		// Acquire the scheduler lock and try to make a request
		//
		// This anonymous function returns nil, nil if an error occurred that was already logged.
		permit, err := func() (*api.Resources, error) {
			sched.requestLock.Lock()
			defer sched.requestLock.Unlock()

			// It might have taken a while to acquire the lock. If the scheduler is no longer
			// current, then we shouldn't try to make a request with it.
			isCurrent := func() bool {
				r.lock.Lock()
				defer r.lock.Unlock()
				return r.scheduler == sched
			}()

			if !isCurrent {
				r.logger.Warningf("Unable to complete updating VM resources: scheduler became old")
				return nil, nil
			}

			// if the scheduler is unregistered *after* we acquired its requestLock, then the
			// initial Register request failed.
			if !sched.registered {
				if err := sched.Register(ctx, func() {}); err != nil {
					sched.logger.Errorf("Error re-attempting register: %s", err)
					r.logger.Warningf("Unable to complete updating VM resources: scheduler Register failed")
					return nil, nil
				}
			}

			request := api.AgentRequest{
				ProtoVersion: PluginProtocolVersion,
				Pod:          r.podName,
				Resources:    target,
				Metrics:      &state.metrics, // FIXME: the metrics here *might* be a little out of date.
			}
			response, err := sched.DoRequest(ctx, &request)
			if err != nil {
				sched.logger.Errorf("Request failed: %s", err)
				r.logger.Warningf("Unable to complete updating VM resources: scheduler request failed")
				return nil, nil
			}
			if response.Migrate != nil {
				// FIXME: honestly? panicking is kind of ok as a basic solution. We should figure
				// out what to *actually* do here though. Probably move sched.requestLock into
				// Runner itself, and have that guard requests to both the scheduler *and* NeonVM,
				// with required checks against a Runner.migrating field. That would be simple
				// *enough*, for the most part.
				panic(errors.New("migration handling unimplemented"))
			}

			// We have a big comment about why preemptively increasing r.vm is necessary up above.
			// Refer to that for more information.

			return &response.Permit, nil
		}()

		if permit == nil || err != nil {
			return err
		}

		// sched.DoRequest should have validated the permit, meaning that it's not less than the
		// current resource usage.
		vmUsing := state.vm.Using()
		if permit.HasFieldLessThan(vmUsing) {
			panic(errors.New("invalid state: permit less than what's in use"))
		} else if permit.HasFieldGreaterThan(target) {
			panic(errors.New("invalid state: permit greater than target"))
		}

		if *permit == vmUsing {
			if vmUsing != target {
				r.logger.Infof("Scheduler denied increase, staying at %+v", vmUsing)
			}

			// nothing to do
			return nil
		} else /* permit > vmUsing */ {
			if *permit != target {
				r.logger.Infof("Scheduler capped increase to %+v", *permit)
			} else {
				r.logger.Infof("Scheduler allowed increase to %+v", *permit)
			}

			rejectedDownscale := func() (newTarget api.Resources, _ error) {
				panic(errors.New("rejectedDownscale called but request should be increasing, not decreasing"))
			}
			if _, err := r.doVMUpdate(ctx, vmUsing, *permit, rejectedDownscale); err != nil {
				return fmt.Errorf("Error doing VM update 2: %w", err)
			}

			return nil
		}
	}
}

// getStateForVMUpdate produces the atomicUpdateState for updateVMResources
//
// This method MUST be called while holding r.lock.
func (r *Runner) getStateForVMUpdate(updateReason VMUpdateReason) *atomicUpdateState {
	if r.lastMetrics == nil {
		if updateReason == UpdatedMetrics {
			panic(errors.New("invalid state: metrics signalled but r.lastMetrics == nil"))
		}

		r.logger.Warningf("Unable to update VM resources because we haven't received metrics yet")
		return nil
	} else if r.computeUnit == nil {
		if updateReason == RegisteredScheduler {
			// note: the scheduler that was registered might not be the scheduler we just got!
			// However, r.computeUnit is never supposed to go from non-nil to nil, so that doesn't
			// actually matter.
			panic(errors.New("invalid state: registered scheduler signalled but r.computeUnit == nil"))
		}

		// note: as per the docs on r.computeUnit, this should only occur when we haven't yet talked
		// to a scheduler.
		r.logger.Warningf("Unable to update VM resources because r.computeUnit hasn't been set yet")
		return nil
	} else if r.lastApproved == nil {
		panic(errors.New("invalid state: r.computeUnit != nil but r.lastApproved == nil"))
	}

	// Check that the VM's current usage is <= lastApproved
	if vmUsing := r.vm.Using(); vmUsing.HasFieldGreaterThan(*r.lastApproved) {
		panic(fmt.Errorf(
			"invalid state: r.vm has resources greater than r.lastApproved (%+v vs %+v)",
			vmUsing, *r.lastApproved,
		))
	}

	return &atomicUpdateState{
		computeUnit:      *r.computeUnit,
		metrics:          *r.lastMetrics,
		vm:               *r.vm,
		lastApproved:     *r.lastApproved,
		requestedUpscale: r.requestedUpscale,
	}
}

// desiredVMState calculates what the resource allocation to the VM should be, given the metrics and
// current state.
//
// FIXME: This should have *some* access to prior scaling decisions, so that we can e.g. use slower
// scaling to start, and accelerate it over time.
//
// FIXME: Even factoring in the above, this implementation is *pretty bad*.
func (s *atomicUpdateState) desiredVMState(allowDecrease bool) api.Resources {
	// There's some annoying edge cases that this function has to be able to handle properly. For
	// the sake of completeness, they are:
	//
	// 1. s.vm.Using() is not a multiple of s.computeUnit
	// 2. s.vm.Max() is less than s.computeUnit (or: has at least one resource that is)
	// 3. s.vm.Using() is a fractional multiple of s.computeUnit, but !allowDecrease and rounding up
	//    is greater than s.vm.Max()
	// 4. s.vm.Using() is much larger than s.vm.Min() and not a multiple of s.computeUnit, but load
	//    is low so we should just decrease *anyways*.
	//
	// ---
	// Now, it's worth noting that we only *barely* handle the edge cases above. We don't do it
	// well, but we do handle them in a protocol-compliant way, and that's what counts! Eventually,
	// this function will be rewritten, this note removed, and all will be well.

	lowerBoundCU, upperBoundCU := s.computeUnitsBounds()

	currentCU := upperBoundCU

	// if we don't have an even compute unit *and* we're allowed to decrease, pick the middle.
	if lowerBoundCU != upperBoundCU && allowDecrease {
		currentCU = lowerBoundCU + (upperBoundCU-lowerBoundCU+1)/2 // +1 so we round up
	}

	goalCU := currentCU
	if s.metrics.LoadAverage1Min > 0.9*float32(s.vm.Cpu.Use) {
		goalCU *= 2
	} else if s.metrics.LoadAverage1Min < 0.4*float32(s.vm.Cpu.Use) && allowDecrease {
		goalCU /= 2
	}

	// Update goalCU based on any requested upscaling
	goalCU = util.Max(goalCU, s.requiredCUForRequestedUpscaling())

	// resources for the desired "goal" compute units
	goal := s.computeUnit.Mul(goalCU)

	// bound goal by the minimum and maximum resource amounts for the VM
	result := goal.Min(s.vm.Max()).Max(s.vm.Min())

	// Check that the result is sound.
	//
	// With the current (naive) implementation, this is trivially ok. In future versions, it might
	// not be so simple, so it's good to have this integrity check here.
	if result.HasFieldGreaterThan(s.vm.Max()) {
		panic(fmt.Errorf(
			"produced invalid desiredVMState: result has field greater than max. this = %+v", *s,
		))
	} else if result.HasFieldLessThan(s.vm.Min()) {
		panic(fmt.Errorf(
			"produced invalid desiredVMState: result has field less than min. this = %+v", *s,
		))
	}

	return result
}

// computeUnitsBounds returns the minimum and maximum number of Compute Units required to fit each
// resource for the VM's current allocation
//
// Under typical operation, this will just return two equal values, both of which are equal to the
// VM's current number of Compute Units. However, if the VM's resource allocation doesn't cleanly
// divide to a multiple of the Compute Unit, the upper and lower bounds will be different. This can
// happen when the Compute Unit is changed, or when the VM's maximum or minimum resource allocations
// has previously prevented it from being set to a multiple of the Compute Unit.
func (s *atomicUpdateState) computeUnitsBounds() (uint16, uint16) {
	// (x + M-1) / M is equivalent to ceil(x/M), as long as M != 0, which is already guaranteed by
	// the
	minCPUUnits := (s.vm.Cpu.Use + s.computeUnit.VCPU - 1) / s.computeUnit.VCPU
	minMemUnits := (s.vm.Mem.Use + s.computeUnit.Mem - 1) / s.computeUnit.Mem

	return util.Min(minCPUUnits, minMemUnits), util.Max(minCPUUnits, minMemUnits)
}

// requiredCUForRequestedUpscaling returns the minimum Compute Units required to abide by the
// requested upscaling, if there is any.
//
// If there's no requested upscaling, then this method will return zero.
//
// This method does not respect any bounds on Compute Units placed by the VM's maximum or minimum
// resource allocation.
func (s *atomicUpdateState) requiredCUForRequestedUpscaling() uint16 {
	var required uint16

	// note: floor(x / M) + 1 gives the minimum integer value greater than x / M.

	if s.requestedUpscale.Cpu {
		required = util.Max(required, s.vm.Cpu.Use/s.computeUnit.VCPU+1)
	}
	if s.requestedUpscale.Memory {
		required = util.Max(required, s.vm.Mem.Use/s.computeUnit.Mem+1)
	}

	return required
}

// doVMUpdate handles updating the VM's resources from current to target WITHOUT CHECKING WITH THE
// SCHEDULER. It is the caller's responsibility to ensure that target is not greater than
// r.lastApproved, and check with the scheduler if necessary.
//
// If the VM informant is required and unavailable (or becomes unavailable), this method will:
// return nil, nil; log an appropriate warning; and reset the VM's state to its current value.
//
// If some resources in target are less than current, and the VM informant rejects the proposed
// downscaling, rejectedDownscale will be called. If it returns an error, that error will be
// returned and the update will be aborted. Otherwise, the returned newTarget will be used.
//
// FIXME: If the requested change is only upscaling, this method does not check that the VM
// informant is available before making the request to NeonVM, which means we can incorrectly
// cause changes to the VM while not enabled. This should be fixed in a broader rework, along with a
// handful of other items.
//
// This method MUST be called while holding r.vmStateLock AND NOT r.lock.
func (r *Runner) doVMUpdate(
	ctx context.Context,
	current api.Resources,
	target api.Resources,
	rejectedDownscale func() (newTarget api.Resources, _ error),
) (*api.Resources, error) {
	r.logger.Infof("Attempting VM update %+v -> %+v", current, target)

	// helper handling function to reset r.vm to reflect the actual current state. Must not be
	// called while holding r.lock.
	resetVMTo := func(amount api.Resources) {
		r.lock.Lock()
		defer r.lock.Unlock()

		r.vm.SetUsing(amount)
	}

	// If there's any fields that are being downscaled, request that from the VM informant.
	downscaled := current.Min(target)
	if downscaled != current {
		resp, err := r.doInformantDownscale(ctx, downscaled)
		if err != nil || resp == nil /* resp = nil && err = nil when the error has been handled */ {
			return nil, err
		}

		if !resp.Ok {
			newTarget, err := rejectedDownscale()
			if err != nil {
				resetVMTo(current)
				return nil, err
			} else if newTarget.HasFieldLessThan(current) {
				panic(fmt.Errorf(
					"rejectedDownscale returned new target less than current: %+v has field less than %+v",
					newTarget, current,
				))
			}

			if newTarget != target {
				r.logger.Infof("VM update: rejected downscale changed target to %+v", newTarget)
			}

			target = newTarget
		}
	}

	// Make the NeonVM request
	r.logger.Infof("Making NeonVM request for %+v", target)
	patches := []util.JSONPatch{{
		Op:    util.PatchReplace,
		Path:  "/spec/guest/cpus/use",
		Value: target.VCPU,
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

	// We couldn't update the VM
	if err != nil {
		// If the context was cancelled, we generally don't need to worry about whether setting r.vm
		// back to current is sound. All operations on this VM are done anyways.
		if ctx.Err() != nil {
			resetVMTo(current) // FIXME: yeah, even though the comment above says "don't worry", maybe worry?
			return nil, fmt.Errorf("Error making VM patch request: %w", err)
		}

		// Otherwise, something went wrong *in the request itself*. This probably leaves us in an
		// inconsistent state, so we're best off ending all further operations. The correct way to
		// fatally error is by panicking - our infra here ensures it won't take down any other
		// runners.
		panic(fmt.Errorf("Unexpected VM patch request failure: %w", err))
	}

	// We scaled. If we run into an issue around further communications with the informant, then
	// it'll be left with an inconsistent state - there's not really anything we can do about that,
	// unfortunately.
	resetVMTo(target)

	upscaled := target // we already handled downscaling; only upscaling can be left
	if upscaled.HasFieldGreaterThan(current) {
		// Unset fields in r.requestedUpscale if we've handled it.
		//
		// Essentially, for each field F, set:
		//
		//     r.requestedUpscale.F = r.requestedUpscale && !(upscaled.F > current.F)
		func() {
			r.lock.Lock()
			defer r.lock.Unlock()

			r.requestedUpscale = r.requestedUpscale.And(upscaled.IncreaseFrom(current).Not())
		}()

		if ok, err := r.doInformantUpscale(ctx, upscaled); err != nil || !ok {
			return nil, err
		}
	}

	r.logger.Infof("Updated VM %+v -> %+v", current, target)

	// Everything successful.
	return &target, nil
}

// doInformantDownscale is a convenience wrapper around (*InformantServer).Downscale that locks r,
// checks if r.server is nil, and does the request.
//
// Some errors are logged by this method instead of being returned. If that happens, this method
// returns nil, nil.
//
// This method MUST NOT be called while holding r.lock.
func (r *Runner) doInformantDownscale(ctx context.Context, to api.Resources) (*api.DownscaleResult, error) {
	msg := "Error requesting informant downscale"

	server := func() *InformantServer {
		r.lock.Lock()
		defer r.lock.Unlock()
		return r.server
	}()
	if server == nil {
		return nil, fmt.Errorf("%s: InformantServer is not set (this should not occur after startup)", msg)
	}

	resp, err := server.Downscale(ctx, to)
	if err != nil {
		if IsNormalInformantError(err) {
			r.logger.Warningf("%s: %s", msg, err)
			return nil, nil
		} else {
			return nil, fmt.Errorf("%s: %w", msg, err)
		}
	}

	return resp, nil
}

// doInformantDownscale is a convenience wrapper around (*InformantServer).Upscale that locks r,
// checks if r.server is nil, and does the request.
//
// Some errors are logged by this method instead of being returned. If that happens, this method
// returns false, nil.
//
// This method MUST NOT be called while holding r.lock.
func (r *Runner) doInformantUpscale(ctx context.Context, to api.Resources) (ok bool, _ error) {
	msg := "Error notifying informant of upscale"

	server := func() *InformantServer {
		r.lock.Lock()
		defer r.lock.Unlock()
		return r.server
	}()
	if server == nil {
		return false, fmt.Errorf("%s: InformantServer is not set (this should not occur after startup)", msg)
	}

	if err := server.Upscale(ctx, to); err != nil {
		if IsNormalInformantError(err) {
			r.logger.Warningf("%s: %s", msg, err)
			return false, nil
		} else {
			return false, fmt.Errorf("%s: %w", msg, err)
		}
	}

	return true, nil
}

// Register performs the initial request required to register with a scheduler
//
// This method is called immediately after the Scheduler is created, and may be called
// subsequent times if the initial request fails.
//
// signalOk will be called if the request succeeds, with s.runner.lock held - but only if
// s.runner.scheduler == s.
//
// This method MUST be called while holding s.requestLock AND NOT s.runner.lock
func (s *Scheduler) Register(ctx context.Context, signalOk func()) error {
	metrics, resources := func() (*api.Metrics, api.Resources) {
		s.runner.lock.Lock()
		defer s.runner.lock.Unlock()

		return s.runner.lastMetrics, s.runner.vm.Using()
	}()

	req := api.AgentRequest{
		ProtoVersion: PluginProtocolVersion,
		Pod:          s.runner.podName,
		Resources:    resources,
		Metrics:      metrics,
	}
	if _, err := s.DoRequest(ctx, &req); err != nil {
		return err
	}

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	s.registered = true
	if s.runner.scheduler == s {
		signalOk()
	}

	return nil
}

// SendRequest implements all of the tricky logic for requests sent to the scheduler plugin
//
// This method checks:
// * That the response is semantically valid
// * That the response matches with the state of s.runner.vm, if s.runner.scheduler == s
//
// This method may set:
// * s.fatalError
// * s.runner.{computeUnit,lastApproved,lastSchedulerError}, if s.runner.scheduler == s
//
// FIXME: when support for migration is added back, this method needs to similarly handle that.
//
// This method MUST be called while holding s.requestLock AND NOT s.runner.lock. s.requestLock will
// not be released during a call to this method.
func (s *Scheduler) DoRequest(ctx context.Context, reqData *api.AgentRequest) (*api.PluginResponse, error) {
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

	s.logger.Infof("Sending AgentRequest: %s", string(reqBody))

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, s.handleRequestError(reqData, fmt.Errorf("Error doing request: %w", err))
	}
	defer response.Body.Close()

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

	s.logger.Infof("Received PluginResponse: %s", string(respBody))

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	if err := s.validatePluginResponse(reqData, &respData); err != nil {
		// Fatal, because an invalid response indicates mismatched state, so we can't assume
		// anything about the plugin's state.
		return nil, s.handleFatalError(reqData, fmt.Errorf("Semantically invalid response: %w", err))
	}

	// if this scheduler is still current, update all the relevant fields in s.runner
	if s.runner.scheduler == s {
		s.runner.computeUnit = &respData.ComputeUnit
		s.runner.lastApproved = &respData.Permit
		s.runner.lastSchedulerError = nil
	}

	return &respData, nil
}

// validatePluginResponse checks that the PluginResponse is valid for the AgentRequest that was
// sent.
//
// This method will not update any fields in s or s.runner.
//
// This method MUST be called while holding s.requestLock AND s.runner.lock.
func (s *Scheduler) validatePluginResponse(
	req *api.AgentRequest,
	resp *api.PluginResponse,
) error {
	isCurrent := s.runner.scheduler == s

	if err := req.Resources.ValidateNonZero(); err != nil {
		panic(fmt.Errorf("we created an invalid AgentRequest.Resources: %w", err))
	}

	// Errors from resp alone
	if err := resp.Permit.ValidateNonZero(); err != nil {
		return fmt.Errorf("Invalid permit: %w", err)
	}
	if err := resp.ComputeUnit.ValidateNonZero(); err != nil {
		return fmt.Errorf("Invalid compute unit: %w", err)
	}

	// Errors from resp in connection with the prior request
	if resp.Permit.HasFieldGreaterThan(req.Resources) {
		return fmt.Errorf(
			"Permit has resources greater than request (%+v vs. %+v)",
			resp.Permit, req.Resources,
		)
	}

	// Errors from resp in connection with the prior request AND the VM state
	if isCurrent {
		if vmUsing := s.runner.vm.Using(); resp.Permit.HasFieldLessThan(vmUsing) {
			return fmt.Errorf("Permit has resources less than VM (%+v vs %+v)", resp.Permit, vmUsing)
		}
	}

	if !isCurrent && resp.Migrate != nil {
		s.logger.Warningf("scheduler is no longer current, but its response signalled migration")
	}

	return nil
}

// handlePreRequestError appropriately handles updating the Scheduler and its Runner's state to
// reflect that an error occurred. It returns the error passed to it
//
// This method will update s.runner.lastSchedulerError if s.runner.scheduler == s.
//
// This method MUST be called while holding s.requestLock AND NOT s.runner.lock.
func (s *Scheduler) handlePreRequestError(err error) error {
	if err == nil {
		panic(errors.New("handlePreRequestError called with nil error"))
	}

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	if s.runner.scheduler == s {
		s.runner.lastSchedulerError = err
	}

	return err
}

// handleRequestError appropriately handles updating the Scheduler and its Runner's state to reflect
// that an error occurred while making a request. It returns the error passed to it
//
// This method will update s.runner.{lastApproved,lastSchedulerError} if s.runner.scheduler == s.
//
// This method MUST be called while holding s.requestLock AND NOT s.runner.lock.
func (s *Scheduler) handleRequestError(req *api.AgentRequest, err error) error {
	if err == nil {
		panic(errors.New("handleRequestError called with nil error"))
	}

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	if s.runner.scheduler == s {
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
// This method MUST be called while holding s.requestLock AND NOT s.runner.lock.
func (s *Scheduler) handleFatalError(req *api.AgentRequest, err error) error {
	if err == nil {
		panic(errors.New("handleFatalError called with nil error"))
	}

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	s.fatalError = err

	if s.runner.scheduler == s {
		s.runner.lastSchedulerError = err
		// for reasoning on lastApproved, see handleRequestError.
		lastApproved := s.runner.vm.Using()
		s.runner.lastApproved = &lastApproved
	}

	return err
}
