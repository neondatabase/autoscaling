package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tychoish/fun/pubsub"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/agent/schedwatch"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

// agentState is the global state for the autoscaler agent
//
// All fields are immutable, except pods.
type agentState struct {
	// lock guards access to pods
	lock util.ChanMutex
	pods map[util.NamespacedName]*podState

	informantMuxServer *HttpMuxServer

	// A base logger to pass around, so we can recreate the logger for a Runner on restart, without
	// running the risk of leaking keys.
	baseLogger *zap.Logger

	podIP                string
	config               *Config
	kubeClient           *kubernetes.Clientset
	vmClient             *vmclient.Clientset
	schedulerEventBroker *pubsub.Broker[schedwatch.WatchEvent]
	schedulerStore       *watch.Store[corev1.Pod]
	metrics              PromMetrics
}

func (r MainRunner) newAgentState(
	baseLogger *zap.Logger,
	podIP string,
	broker *pubsub.Broker[schedwatch.WatchEvent],
	schedulerStore *watch.Store[corev1.Pod],
	informantMuxServer *HttpMuxServer,
) (*agentState, *prometheus.Registry) {
	state := &agentState{
		lock:                 util.NewChanMutex(),
		pods:                 make(map[util.NamespacedName]*podState),
		informantMuxServer:   informantMuxServer,
		baseLogger:           baseLogger,
		config:               r.Config,
		kubeClient:           r.KubeClient,
		vmClient:             r.VMClient,
		podIP:                podIP,
		schedulerEventBroker: broker,
		schedulerStore:       schedulerStore,
		metrics:              PromMetrics{}, //nolint:exhaustruct // set below
	}

	var promReg *prometheus.Registry
	state.metrics, promReg = makePrometheusParts(state)

	return state, promReg
}

func vmIsOurResponsibility(vm *vmapi.VirtualMachine, config *Config, nodeName string) bool {
	return vm.Status.Node == nodeName &&
		(vm.Status.Phase.IsAlive() && vm.Status.Phase != vmapi.VmMigrating) &&
		vm.Status.PodIP != "" &&
		api.HasAutoscalingEnabled(vm) &&
		vm.Spec.SchedulerName == config.Scheduler.SchedulerName
}

func (s *agentState) Stop() {
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, pod := range s.pods {
		pod.stop()
	}
}

func (s *agentState) handleEvent(ctx context.Context, logger *zap.Logger, event vmEvent) {
	logger = logger.With(
		zap.Object("event", event),
		zap.Object("virtualmachine", event.vmInfo.NamespacedName()),
		zap.Object("pod", util.NamespacedName{Namespace: event.vmInfo.Namespace, Name: event.podName}),
	)

	if err := s.lock.TryLock(ctx); err != nil {
		logger.Warn("Context canceled while starting to handle event", zap.Error(err))
		return
	}
	defer s.lock.Unlock()

	podName := util.NamespacedName{Namespace: event.vmInfo.Namespace, Name: event.podName}
	state, hasPod := s.pods[podName]

	// nb: we add the "pod" key for uniformity, even though it's derived from the event
	if event.kind != vmEventAdded && !hasPod {
		logger.Error("Received event for pod that isn't present", zap.Object("pod", podName))
		return
	} else if event.kind == vmEventAdded && hasPod {
		logger.Error("Received add event for pod that's already present", zap.Object("pod", podName))
		return
	}

	switch event.kind {
	case vmEventDeleted:
		state.stop()
		// mark the status as deleted, so that it gets removed from metrics.
		state.status.update(s, func(stat podStatus) podStatus {
			stat.deleted = true
			delete(s.pods, podName) // Do the removal while synchronized, because we can :)
			return stat
		})
	case vmEventUpdated:
		state.status.update(s, func(stat podStatus) podStatus {
			now := time.Now()
			stat.vmInfo = event.vmInfo
			stat.endpointID = event.endpointID
			stat.endpointAssignedAt = &now
			state.vmInfoUpdated.Send()

			return stat
		})
	case vmEventAdded:
		s.handleVMEventAdded(ctx, event, podName)
	default:
		panic(errors.New("bad event: unexpected event kind"))
	}
}

func (s *agentState) handleVMEventAdded(
	ctx context.Context,
	event vmEvent,
	podName util.NamespacedName,
) {
	runnerCtx, cancelRunnerContext := context.WithCancel(ctx)

	now := time.Now()

	status := &lockedPodStatus{
		mu: sync.Mutex{},
		podStatus: podStatus{
			deleted:            false,
			endState:           nil,
			previousEndStates:  nil,
			vmInfo:             event.vmInfo,
			endpointID:         event.endpointID,
			endpointAssignedAt: &now,
			state:              "", // Explicitly set state to empty so that the initial state update does no decrement
			stateUpdatedAt:     now,

			startTime:                   now,
			lastSuccessfulInformantComm: nil,
		},
	}

	// Empty update to trigger updating metrics and state.
	status.update(s, func(s podStatus) podStatus { return s })

	restartCount := 0
	runner := s.newRunner(event.vmInfo, podName, event.podIP, restartCount)
	runner.status = status

	txVMUpdate, rxVMUpdate := util.NewCondChannelPair()

	s.pods[podName] = &podState{
		podName:       podName,
		stop:          cancelRunnerContext,
		runner:        runner,
		status:        status,
		vmInfoUpdated: txVMUpdate,
	}
	s.metrics.runnerStarts.Inc()
	logger := s.loggerForRunner(event.vmInfo.NamespacedName(), podName)
	runner.Spawn(runnerCtx, logger, rxVMUpdate)
}

// FIXME: make these timings configurable.
const (
	RunnerRestartMinWaitSeconds = 5
	RunnerRestartMaxWaitSeconds = 10
)

// TriggerRestartIfNecessary restarts the Runner for podName, after a delay if necessary.
//
// NB: runnerCtx is the context *passed to the new Runner*. It is only used here to end our restart
// process early if it's already been canceled. logger is not passed, and so can be handled a bit
// more freely.
func (s *agentState) TriggerRestartIfNecessary(runnerCtx context.Context, logger *zap.Logger, podName util.NamespacedName, podIP string) {
	// Three steps:
	//  1. Check if the Runner needs to restart. If no, we're done.
	//  2. Wait for a random amount of time (between RunnerRestartMinWaitSeconds and RunnerRestartMaxWaitSeconds)
	//  3. Restart the Runner (if it still should be restarted)

	status, ok := func() (*lockedPodStatus, bool) {
		s.lock.Lock()
		defer s.lock.Unlock()
		// note: pod.status has a separate lock, so we're ok to release s.lock
		if pod, ok := s.pods[podName]; ok {
			return pod.status, true
		} else {
			return nil, false
		}
	}()

	if !ok {
		return
	}

	status.mu.Lock()
	defer status.mu.Unlock()

	if status.endState == nil {
		logger.Error("TriggerRestartIfNecessary called with nil endState (should only be called after the pod is finished, when endState != nil)")
		s.metrics.runnerFatalErrors.Inc()
		return
	}

	endTime := status.endState.Time

	if endTime.IsZero() {
		// If we don't check this, we run the risk of spinning on failures.
		logger.Error("TriggerRestartIfNecessary called with zero'd Time for pod")
		s.metrics.runnerFatalErrors.Inc()
		// Continue on, but with the time overridden, so we guarantee our minimum wait.
		endTime = time.Now()
	}

	// keep this for later.
	exitKind := status.endState.ExitKind

	switch exitKind {
	case podStatusExitCanceled:
		logger.Info("Runner's context was canceled; no need to restart")
		return // successful exit, no need to restart.
	case podStatusExitPanicked, podStatusExitErrored:
		// Should restart; continue.
		logger.Info("Runner had abnormal exit kind; it will restart", zap.String("exitKind", string(exitKind)))
	default:
		logger.Error("TriggerRestartIfNecessary called with unexpected ExitKind", zap.String("exitKind", string(exitKind)))
		s.metrics.runnerFatalErrors.Inc()
		return
	}

	// Begin steps (2) and (3) -- wait, then restart.
	var waitDuration time.Duration
	totalRuntime := endTime.Sub(status.startTime)

	// If the runner was running for a while, restart immediately.
	//
	// NOTE: this will have incorrect behavior when the system clock is behaving weirdly, but that's
	// mostly ok. It's ok to e.g. restart an extra time at the switchover to daylight saving time.
	if totalRuntime > time.Second*time.Duration(RunnerRestartMaxWaitSeconds) {
		logger.Info("Runner was running for a long time, restarting immediately", zap.Duration("totalRuntime", totalRuntime))
		waitDuration = 0
	} else /* Otherwise, randomly pick within RunnerRestartMinWait..RunnerRestartMaxWait */ {
		r := util.NewTimeRange(time.Second, RunnerRestartMinWaitSeconds, RunnerRestartMaxWaitSeconds)
		waitDuration = r.Random()
		logger.Info(
			"Runner was not running for long, restarting after delay",
			zap.Duration("totalRuntime", totalRuntime),
			zap.Duration("delay", waitDuration),
		)
	}

	// Run the waiting (if necessary) and restarting in another goroutine, so we're not blocking the
	// caller of this function.
	go func() {
		logCancel := func(logFunc func(string, ...zap.Field), err error) {
			logFunc(
				"Canceling restart of Runner",
				zap.Duration("delay", waitDuration),
				zap.Duration("waitTime", time.Since(endTime)),
				zap.Error(err),
			)
		}

		if waitDuration != 0 {
			select {
			case <-time.After(waitDuration):
			case <-runnerCtx.Done():
				logCancel(logger.Info, runnerCtx.Err())
				return
			}
		}

		s.lock.Lock()
		defer s.lock.Unlock()

		// Need to update pod itself; can't release s.lock. Also, pod *theoretically* may been
		// deleted + restarted since we started, so it's incorrect to hold on to the original
		// podStatus.
		pod, ok := s.pods[podName]
		if !ok {
			logCancel(logger.Warn, errors.New("no longer present in pod map"))
			return
		}

		pod.status.update(s, func(status podStatus) podStatus {
			// Runner was already restarted
			if status.endState == nil {
				addedInfo := "this generally shouldn't happen, but could if there's a new pod with the same name"
				logCancel(logger.Warn, fmt.Errorf("Runner was already restarted (%s)", addedInfo))
				return status
			}

			logger.Info("Restarting runner", zap.String("exitKind", string(exitKind)), zap.Duration("delay", time.Since(endTime)))
			s.metrics.runnerRestarts.Inc()

			restartCount := len(status.previousEndStates) + 1
			runner := s.newRunner(status.vmInfo, podName, podIP, restartCount)
			runner.status = pod.status

			txVMUpdate, rxVMUpdate := util.NewCondChannelPair()
			// note: pod is *podState, so we don't need to re-assign to the map.
			pod.vmInfoUpdated = txVMUpdate
			pod.runner = runner

			status.previousEndStates = append(status.previousEndStates, *status.endState)
			status.endState = nil
			status.startTime = time.Now()

			runnerLogger := s.loggerForRunner(status.vmInfo.NamespacedName(), podName)
			runner.Spawn(runnerCtx, runnerLogger, rxVMUpdate)
			return status
		})
	}()
}

func (s *agentState) loggerForRunner(vmName, podName util.NamespacedName) *zap.Logger {
	return s.baseLogger.Named("runner").With(zap.Object("virtualmachine", vmName), zap.Object("pod", podName))
}

// NB: caller must set Runner.status after creation
func (s *agentState) newRunner(vmInfo api.VmInfo, podName util.NamespacedName, podIP string, restartCount int) *Runner {
	return &Runner{
		global:                          s,
		status:                          nil, // set by calller
		schedulerRespondedWithMigration: false,

		shutdown:         nil, // set by (*Runner).Run
		vm:               vmInfo,
		podName:          podName,
		podIP:            podIP,
		lock:             util.NewChanMutex(),
		requestLock:      util.NewChanMutex(),
		requestedUpscale: api.MoreResources{Cpu: false, Memory: false},

		lastMetrics:        nil,
		scheduler:          nil,
		server:             nil,
		informant:          nil,
		computeUnit:        nil,
		lastApproved:       nil,
		lastSchedulerError: nil,
		lastInformantError: nil,

		backgroundWorkerCount: atomic.Int64{},
		backgroundPanic:       make(chan error),
	}
}

type podState struct {
	podName util.NamespacedName

	stop   context.CancelFunc
	runner *Runner
	status *lockedPodStatus

	vmInfoUpdated util.CondChannelSender
}

type podStateDump struct {
	PodName         util.NamespacedName `json:"podName"`
	Status          podStatusDump       `json:"status"`
	Runner          *RunnerState        `json:"runner,omitempty"`
	CollectionError error               `json:"collectionError,omitempty"`
}

func (p *podState) dump(ctx context.Context) podStateDump {
	status := p.status.dump()
	runner, collectErr := p.runner.State(ctx)
	if collectErr != nil {
		collectErr = fmt.Errorf("error reading runner state: %w", collectErr)
	}
	return podStateDump{
		PodName:         p.podName,
		Status:          status,
		Runner:          runner,
		CollectionError: collectErr,
	}
}

type lockedPodStatus struct {
	mu sync.Mutex

	podStatus
}

type podStatus struct {
	startTime time.Time

	// if true, the corresponding podState is no longer included in the global pod map
	deleted bool

	// if non-nil, the runner is finished
	endState          *podStatusEndState
	previousEndStates []podStatusEndState

	lastSuccessfulInformantComm *time.Time

	// vmInfo stores the latest information about the VM, as given by the global VM watcher.
	//
	// There is also a similar field inside the Runner itself, but it's better to store this out
	// here, where we don't have to rely on the Runner being well-behaved w.r.t. locking.
	vmInfo api.VmInfo

	// endpointID, if non-empty, stores the ID of the endpoint associated with the VM
	endpointID string

	// NB: this value, once non-nil, is never changed.
	endpointAssignedAt *time.Time

	state          runnerMetricState
	stateUpdatedAt time.Time
}

type podStatusDump struct {
	StartTime time.Time `json:"startTime"`

	EndState          *podStatusEndState  `json:"endState"`
	PreviousEndStates []podStatusEndState `json:"previousEndStates"`

	LastSuccessfulInformantComm *time.Time `json:"lastSuccessfulInformantComm"`

	VMInfo api.VmInfo `json:"vmInfo"`

	EndpointID         string     `json:"endpointID"`
	EndpointAssignedAt *time.Time `json:"endpointAssignedAt"`

	State          runnerMetricState `json:"state"`
	StateUpdatedAt time.Time         `json:"stateUpdatedAt"`
}

type podStatusEndState struct {
	// The reason the Runner exited.
	ExitKind podStatusExitKind `json:"exitKind"`
	// If ExitKind is "panicked" or "errored", the error message.
	Error error     `json:"error"`
	Time  time.Time `json:"time"`
}

type podStatusExitKind string

const (
	podStatusExitPanicked podStatusExitKind = "panicked"
	podStatusExitErrored  podStatusExitKind = "errored"
	podStatusExitCanceled podStatusExitKind = "canceled" // top-down signal that the Runner should stop.
)

func (s *lockedPodStatus) update(global *agentState, with func(podStatus) podStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()

	newStatus := with(s.podStatus)
	now := time.Now()

	// Calculate the new state:
	var newState runnerMetricState
	if s.deleted {
		// If deleted, don't change anything.
	} else if s.endState != nil {
		switch s.endState.ExitKind {
		case podStatusExitCanceled:
			// If canceled, don't change the state.
			newState = s.state
		case podStatusExitErrored:
			newState = runnerMetricStateErrored
		case podStatusExitPanicked:
			newState = runnerMetricStatePanicked
		}
	} else if newStatus.informantStuckAt(global.config).Before(now) {
		newState = runnerMetricStateStuck
	} else {
		newState = runnerMetricStateOk
	}

	if !newStatus.deleted {
		newStatus.state = newState
		newStatus.stateUpdatedAt = now
	}

	// Update the metrics:
	// Note: s.state is initialized to the empty string to signify that it's not yet represented in
	// the metrics.
	if !s.deleted && s.state != "" {
		oldIsEndpoint := strconv.FormatBool(s.endpointID != "")
		global.metrics.runnersCount.WithLabelValues(oldIsEndpoint, string(s.state)).Dec()
	}

	if !newStatus.deleted && newStatus.state != "" {
		newIsEndpoint := strconv.FormatBool(newStatus.endpointID != "")
		global.metrics.runnersCount.WithLabelValues(newIsEndpoint, string(newStatus.state)).Inc()
	}

	s.podStatus = newStatus
}

// informantStuckAt returns the time at which the Runner will be marked "stuck"
func (s podStatus) informantStuckAt(config *Config) time.Time {
	startupGracePeriod := time.Second * time.Duration(config.Informant.UnhealthyStartupGracePeriodSeconds)
	unhealthySilencePeriod := time.Second * time.Duration(config.Informant.UnhealthyAfterSilenceDurationSeconds)

	if s.lastSuccessfulInformantComm == nil {
		start := s.startTime

		// For endpoints, we should start the grace period from when the VM was *assigned* the
		// endpoint, rather than when the VM was created.
		if s.endpointID != "" {
			start = *s.endpointAssignedAt
		}

		return start.Add(startupGracePeriod)
	} else {
		return s.lastSuccessfulInformantComm.Add(unhealthySilencePeriod)
	}
}

func (s *lockedPodStatus) periodicallyRefreshState(ctx context.Context, logger *zap.Logger, global *agentState) {
	maxUpdateSeconds := util.Min(
		global.config.Informant.UnhealthyStartupGracePeriodSeconds,
		global.config.Informant.UnhealthyAfterSilenceDurationSeconds,
	)
	// make maxTick a bit less than maxUpdateSeconds for the benefit of consistency and having
	// relatively frequent log messages if things are stuck.
	maxTick := time.Second * time.Duration(maxUpdateSeconds/2)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		// use s.update to trigger re-evaluating the metrics, and simultaneously reset the timer to
		// the next point in time at which the state might have changed, so that we minimize the
		// time between the VM meeting the conditions for being "stuck" and us recognizing it.
		s.update(global, func(stat podStatus) podStatus {
			stuckAt := stat.informantStuckAt(global.config)
			now := time.Now()
			if stuckAt.Before(now) && stat.state != runnerMetricStateErrored && stat.state != runnerMetricStatePanicked {
				if stat.endpointID != "" {
					logger.Warn("Runner with endpoint is currently stuck", zap.String("endpointID", stat.endpointID))
				} else {
					logger.Warn("Runner without endpoint is currently stuck")
				}
				timer.Reset(maxTick)
			} else {
				timer.Reset(util.Min(maxTick, stuckAt.Sub(now)))
			}
			return stat
		})
	}
}

func (s *lockedPodStatus) dump() podStatusDump {
	s.mu.Lock()
	defer s.mu.Unlock()

	var endState *podStatusEndState
	if s.endState != nil {
		es := *s.endState
		endState = &es
	}

	previousEndStates := make([]podStatusEndState, len(s.previousEndStates))
	copy(previousEndStates, s.previousEndStates)

	return podStatusDump{
		EndState:          endState,
		PreviousEndStates: previousEndStates,

		// FIXME: api.VmInfo contains a resource.Quantity - is that safe to copy by value?
		VMInfo:             s.vmInfo,
		EndpointID:         s.endpointID,
		EndpointAssignedAt: s.endpointAssignedAt, // ok to share the pointer, because it's not updated
		StartTime:          s.startTime,

		State:          s.state,
		StateUpdatedAt: s.stateUpdatedAt,

		LastSuccessfulInformantComm: s.lastSuccessfulInformantComm,
	}
}