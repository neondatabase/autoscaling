package plugin

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/tychoish/fun/pubsub"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	scheme "k8s.io/client-go/kubernetes/scheme"
	rest "k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

const Name = "AutoscaleEnforcer"
const LabelPluginCreatedMigration = "autoscaling.neon.tech/created-by-scheduler"
const ConfigMapNamespace = "kube-system"
const ConfigMapName = "scheduler-plugin-config"
const ConfigMapKey = "autoscaler-enforcer-config.json"
const InitConfigMapTimeoutSeconds = 5

// AutoscaleEnforcer is the scheduler plugin to coordinate autoscaling
type AutoscaleEnforcer struct {
	logger *zap.Logger

	handle   framework.Handle
	vmClient *vmclient.Clientset
	state    pluginState
	metrics  PromMetrics

	// vmStore provides access the current-ish state of VMs in the cluster. If something's missing,
	// it can be updated with Resync().
	//
	// Keeping this store allows us to make sure that our event handling is never out-of-sync
	// because all the information is coming from the same source.
	vmStore IndexedVMStore
	// nodeStore is doing roughly the same thing as with vmStore, but for Nodes.
	nodeStore IndexedNodeStore
}

// abbreviations, because these types are pretty verbose
type IndexedVMStore = watch.IndexedStore[vmapi.VirtualMachine, *watch.NameIndex[vmapi.VirtualMachine]]
type IndexedNodeStore = watch.IndexedStore[corev1.Node, *watch.FlatNameIndex[corev1.Node]]

// Compile-time checks that AutoscaleEnforcer actually implements the interfaces we want it to
var _ framework.Plugin = (*AutoscaleEnforcer)(nil)
var _ framework.PreFilterPlugin = (*AutoscaleEnforcer)(nil)
var _ framework.PostFilterPlugin = (*AutoscaleEnforcer)(nil)
var _ framework.FilterPlugin = (*AutoscaleEnforcer)(nil)
var _ framework.ScorePlugin = (*AutoscaleEnforcer)(nil)
var _ framework.ReservePlugin = (*AutoscaleEnforcer)(nil)

func NewAutoscaleEnforcerPlugin(ctx context.Context, logger *zap.Logger, config *Config) func(runtime.Object, framework.Handle) (framework.Plugin, error) {
	return func(obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
		return makeAutoscaleEnforcerPlugin(ctx, logger, obj, h, config)
	}
}

// NewAutoscaleEnforcerPlugin produces the initial AutoscaleEnforcer plugin to be used by the
// scheduler
func makeAutoscaleEnforcerPlugin(
	ctx context.Context,
	logger *zap.Logger,
	_obj runtime.Object,
	h framework.Handle,
	config *Config,
) (framework.Plugin, error) {
	// obj can be used for taking in configuration. it's a bit tricky to figure out, and we don't
	// quite need it yet.
	logger.Info("Initializing plugin")

	// create the NeonVM client
	if err := vmapi.AddToScheme(scheme.Scheme); err != nil {
		return nil, err
	}
	vmConfig := rest.CopyConfig(h.KubeConfig())
	// The handler's ContentType is not the default "application/json" (it's protobuf), so we need
	// to set it back to JSON because NeonVM doesn't support protobuf.
	vmConfig.ContentType = "application/json"
	vmClient, err := vmclient.NewForConfig(vmConfig)
	if err != nil {
		return nil, fmt.Errorf("Error creating NeonVM client: %w", err)
	}

	p := AutoscaleEnforcer{
		logger: logger.Named("plugin"),

		handle:   h,
		vmClient: vmClient,
		// remaining fields are set by p.readClusterState and p.makePrometheusRegistry
		state: pluginState{ //nolint:exhaustruct // see above.
			lock:                      util.NewChanMutex(),
			ongoingMigrationDeletions: make(map[util.NamespacedName]int),
			conf:                      config,
		},
		metrics:   PromMetrics{},      //nolint:exhaustruct // set by makePrometheusRegistry
		vmStore:   IndexedVMStore{},   //nolint:exhaustruct // set below
		nodeStore: IndexedNodeStore{}, //nolint:exhaustruct // set below
	}

	if p.state.conf.DumpState != nil {
		logger.Info("Starting 'dump state' server")
		if err := p.startDumpStateServer(ctx, logger.Named("dump-state")); err != nil {
			return nil, fmt.Errorf("Error starting 'dump state' server: %w", err)
		}
	}

	// Start watching Pod/VM events, adding them to a shared queue to process them in order
	queue := pubsub.NewUnlimitedQueue[func()]()
	pushToQueue := func(logger *zap.Logger, f func()) {
		if err := queue.Add(f); err != nil {
			logger.Warn("Error adding to pod/VM event queue", zap.Error(err))
		}
	}

	hlogger := logger.Named("handlers")
	nwc := nodeWatchCallbacks{
		submitNodeDeletion: func(logger *zap.Logger, nodeName string) {
			pushToQueue(logger, func() { p.handleNodeDeletion(hlogger, nodeName) })
		},
	}
	pwc := podWatchCallbacks{
		submitStarted: func(logger *zap.Logger, pod *corev1.Pod) {
			pushToQueue(logger, func() { p.handleStarted(hlogger, pod) })
		},
		submitDeletion: func(logger *zap.Logger, name util.NamespacedName) {
			pushToQueue(logger, func() { p.handleDeletion(hlogger, name) })
		},
		submitStartMigration: func(logger *zap.Logger, podName, migrationName util.NamespacedName, source bool) {
			pushToQueue(logger, func() { p.handlePodStartMigration(logger, podName, migrationName, source) })
		},
		submitEndMigration: func(logger *zap.Logger, podName, migrationName util.NamespacedName) {
			pushToQueue(logger, func() { p.handlePodEndMigration(logger, podName, migrationName) })
		},
	}
	vwc := vmWatchCallbacks{
		submitDisabledScaling: func(logger *zap.Logger, pod util.NamespacedName) {
			pushToQueue(logger, func() { p.handleVMDisabledScaling(hlogger, pod) })
		},
		submitBoundsChanged: func(logger *zap.Logger, vm *api.VmInfo, podName string) {
			pushToQueue(logger, func() { p.handleUpdatedScalingBounds(hlogger, vm, podName) })
		},
		submitNonAutoscalingVmUsageChanged: func(logger *zap.Logger, vm *api.VmInfo, podName string) {
			pushToQueue(logger, func() { p.handleNonAutoscalingUsageChange(hlogger, vm, podName) })
		},
	}
	mwc := migrationWatchCallbacks{
		submitMigrationFinished: func(vmm *vmapi.VirtualMachineMigration) {
			// When cleaning up migrations, we don't want to process those events synchronously.
			// So instead, we'll spawn a goroutine to delete the completed migration.
			go p.cleanupMigration(hlogger, vmm)
		},
	}

	watchMetrics := watch.NewMetrics("autoscaling_plugin_watchers")

	logger.Info("Starting node watcher")
	nodeStore, err := p.watchNodeEvents(ctx, logger, watchMetrics, nwc)
	if err != nil {
		return nil, fmt.Errorf("Error starting node watcher: %w", err)
	}

	p.nodeStore = watch.NewIndexedStore(nodeStore, watch.NewFlatNameIndex[corev1.Node]())

	logger.Info("Starting pod watcher")
	podStore, err := p.watchPodEvents(ctx, logger, watchMetrics, pwc)
	if err != nil {
		return nil, fmt.Errorf("Error starting pod watcher: %w", err)
	}

	podIndex := watch.NewIndexedStore(podStore, watch.NewNameIndex[corev1.Pod]())

	logger.Info("Starting VM watcher")
	vmStore, err := p.watchVMEvents(ctx, logger, watchMetrics, vwc, podIndex)
	if err != nil {
		return nil, fmt.Errorf("Error starting VM watcher: %w", err)
	}

	logger.Info("Starting VM Migration watcher")
	if _, err := p.watchMigrationEvents(ctx, logger, watchMetrics, mwc); err != nil {
		return nil, fmt.Errorf("Error starting VM Migration watcher: %w", err)
	}

	p.vmStore = watch.NewIndexedStore(vmStore, watch.NewNameIndex[vmapi.VirtualMachine]())

	// makePrometheusRegistry sets p.metrics, which we need to do before calling readClusterState,
	// because we set metrics for each node while we build the state.
	promReg := p.makePrometheusRegistry()
	watchMetrics.MustRegister(promReg)

	// ... but before handling the events, read the current cluster state:
	logger.Info("Reading initial cluster state")
	if err = p.readClusterState(ctx, logger); err != nil {
		return nil, fmt.Errorf("Error reading cluster state: %w", err)
	}

	go func() {
		for {
			callback, err := queue.Wait(ctx) // NB: Wait pulls from the front of the queue
			if err != nil {
				logger.Info("Stopped waiting on pod/VM queue", zap.Error(err))
				break
			}

			callback()
		}
	}()

	if err := util.StartPrometheusMetricsServer(ctx, logger.Named("prometheus"), 9100, promReg); err != nil {
		return nil, fmt.Errorf("Error starting prometheus server: %w", err)
	}

	if err := p.startPermitHandler(ctx, logger.Named("agent-handler")); err != nil {
		return nil, fmt.Errorf("permit handler: %w", err)
	}

	// Periodically check that we're not deadlocked
	go func() {
		defer func() {
			if err := recover(); err != nil {
				logger.Panic("deadlock checker for AutoscaleEnforcer.state.lock panicked", zap.String("error", fmt.Sprint(err)))
			}
		}()

		p.state.lock.DeadlockChecker(time.Second, 5*time.Second)(ctx)
	}()

	logger.Info("Plugin initialization complete")
	return &p, nil
}

// Name returns the name of the AutoscaleEnforcer plugin
//
// Required for framework.Plugin
func (e *AutoscaleEnforcer) Name() string {
	return Name
}

// getVmInfo is a helper for the plugin-related functions
//
// This function returns nil, nil if the pod is not associated with a NeonVM virtual machine.
func (e *AutoscaleEnforcer) getVmInfo(logger *zap.Logger, pod *corev1.Pod, action string) (*api.VmInfo, error) {
	vmName := util.TryPodOwnerVirtualMachine(pod)
	if vmName == nil {
		return nil, nil
	}

	accessor := func(index *watch.NameIndex[vmapi.VirtualMachine]) (*vmapi.VirtualMachine, bool) {
		return index.Get(vmName.Namespace, vmName.Name)
	}

	vm, ok := e.vmStore.GetIndexed(accessor)
	if !ok {
		logger.Warn(
			"VM is missing from local store. Relisting",
			zap.Object("pod", util.GetNamespacedName(pod)),
			zap.Object("virtualmachine", vmName),
		)

		// Use a reasonable timeout on the relist request, so that if the VM store is broken, we
		// won't block forever.
		//
		// FIXME: make this configurable
		timeout := 5 * time.Second
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		select {
		case <-e.vmStore.Relist():
		case <-timer.C:
			return nil, fmt.Errorf("Timed out waiting on VM store relist (timeout = %s)", timeout)
		}

		// retry fetching the VM, now that we know it's been synced.
		vm, ok = e.vmStore.GetIndexed(accessor)
		if !ok {
			// if the VM is still not present after relisting, then either it's already been deleted
			// or there's a deeper problem.
			return nil, errors.New("Could not find VM for pod, even after relist")
		}
	}

	vmInfo, err := api.ExtractVmInfo(logger, vm)
	if err != nil {
		e.handle.EventRecorder().Eventf(
			vm,              // regarding
			pod,             // related
			"Warning",       // eventtype
			"ExtractVmInfo", // reason
			action,          // action
			"Failed to extract autoscaling info about VM: %s", // node
			err,
		)
		return nil, fmt.Errorf("Error extracting VM info: %w", err)
	}

	return vmInfo, nil
}

// checkSchedulerName asserts that the SchedulerName field of a Pod matches what we're expecting,
// otherwise returns a non-nil framework.Status to return (and also logs the error)
func (e *AutoscaleEnforcer) checkSchedulerName(logger *zap.Logger, pod *corev1.Pod) *framework.Status {
	if e.state.conf.SchedulerName != pod.Spec.SchedulerName {
		err := fmt.Errorf(
			"Mismatched SchedulerName for pod: our config has %q, but the pod has %q",
			e.state.conf.SchedulerName, pod.Spec.SchedulerName,
		)
		logger.Error("Pod failed scheduler name check", zap.Error(err))
		return framework.NewStatus(framework.Error, err.Error())
	}
	return nil
}

// PreFilter is called at the start of any Pod's filter cycle. We use it in combination with
// PostFilter (which is only called on failure) to provide metrics for pods that are rejected by
// this process.
func (e *AutoscaleEnforcer) PreFilter(
	ctx context.Context,
	state *framework.CycleState,
	pod *corev1.Pod,
) (_ *framework.PreFilterResult, status *framework.Status) {
	ignored := e.state.conf.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("PreFilter", ignored)
	defer func() {
		e.metrics.IncFailIfNotSuccess("PreFilter", ignored, status)
	}()

	return nil, nil
}

// PreFilterExtensions is required for framework.PreFilterPlugin, and can return nil if it's not used
func (e *AutoscaleEnforcer) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

// PostFilter is used by us for metrics on filter cycles that reject a Pod by filtering out all
// applicable nodes.
//
// Quoting the docs for PostFilter:
//
// > These plugins are called after Filter phase, but only when no feasible nodes were found for the
// > pod.
//
// Required for framework.PostFilterPlugin
func (e *AutoscaleEnforcer) PostFilter(
	ctx context.Context,
	state *framework.CycleState,
	pod *corev1.Pod,
	filteredNodeStatusMap framework.NodeToStatusMap,
) (_ *framework.PostFilterResult, status *framework.Status) {
	ignored := e.state.conf.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("PostFilter", ignored)
	defer func() {
		e.metrics.IncFailIfNotSuccess("PostFilter", ignored, status)
	}()

	logger := e.logger.With(zap.String("method", "Filter"), util.PodNameFields(pod))
	logger.Error("Pod rejected by all Filter method calls")

	return nil, nil // PostFilterResult is optional, nil Status is success.
}

// Filter gives our plugin a chance to signal that a pod shouldn't be put onto a particular node
//
// Required for framework.FilterPlugin
func (e *AutoscaleEnforcer) Filter(
	ctx context.Context,
	state *framework.CycleState,
	pod *corev1.Pod,
	nodeInfo *framework.NodeInfo,
) (status *framework.Status) {
	ignored := e.state.conf.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("Filter", ignored)
	defer func() {
		e.metrics.IncFailIfNotSuccess("Filter", ignored, status)
	}()

	nodeName := nodeInfo.Node().Name // TODO: nodes also have namespaces? are they used at all?

	logger := e.logger.With(zap.String("method", "Filter"), zap.String("node", nodeName), util.PodNameFields(pod))
	logger.Info("Handling Filter request")

	if ignored {
		logger.Warn("Received Filter request for pod in ignored namespace, continuing anyways.")
	}

	vmInfo, err := e.getVmInfo(logger, pod, "Filter")
	if err != nil {
		logger.Error("Error getting VM info for Pod", zap.Error(err))
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("Error getting pod vmInfo: %s", err),
		)
	}

	var podResources api.Resources
	if vmInfo != nil {
		podResources = vmInfo.Using()
	} else {
		podResources = extractPodResources(pod)
	}

	// Check that the SchedulerName matches what we're expecting
	if status := e.checkSchedulerName(logger, pod); status != nil {
		return status
	}

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	node, err := e.state.getOrFetchNodeState(ctx, logger, e.metrics, e.nodeStore, nodeName)
	if err != nil {
		logger.Error("Error getting node state", zap.Error(err))
		return framework.NewStatus(
			framework.Error,
			fmt.Sprintf("Error getting node state: %s", err),
		)
	}

	// The pod will get resources according to vmInfo.{Cpu,Mem}.Use reserved for it when it does get
	// scheduled. Now we can check whether this node has capacity for the pod.
	//
	// Technically speaking, the VM pods in nodeInfo might not match what we have recorded for the
	// node -- simply because during preemption, the scheduler tries to see whether it could
	// schedule the pod if other stuff was preempted, and gives us what the state WOULD be after
	// preemption.
	//
	// So we have to actually count up the resource usage of all pods in nodeInfo:
	var nodeTotal api.Resources

	// As we process all pods, we should record all the pods that aren't present in both nodeInfo
	// and e.state's maps, so that we can log any inconsistencies instead of silently using
	// *potentially* bad data. Some differences are expected, but on the whole this extra
	// information should be helpful.
	missedPods := make(map[util.NamespacedName]struct{})
	for name := range node.pods {
		missedPods[name] = struct{}{}
	}

	var includedIgnoredPods []util.NamespacedName

	for _, podInfo := range nodeInfo.Pods {
		pn := util.NamespacedName{Name: podInfo.Pod.Name, Namespace: podInfo.Pod.Namespace}
		if podState, ok := e.state.pods[pn]; ok {
			nodeTotal.VCPU += podState.cpu.Reserved
			nodeTotal.Mem += podState.mem.Reserved
			delete(missedPods, pn)
		} else {
			name := util.GetNamespacedName(podInfo.Pod)

			if util.PodCompleted(podInfo.Pod) {
				logger.Warn(
					"Skipping completed Pod in Filter node's pods",
					zap.Object("pod", name),
					zap.String("phase", string(podInfo.Pod.Status.Phase)),
				)
				continue
			}

			if !e.state.conf.ignoredNamespace(podInfo.Pod.Namespace) {
				// FIXME: this gets us duplicated "pod" fields. Not great. But we're using
				// logger.With pretty pervasively, and it's hard to avoid this while using that.
				// For now, we can get around this by including the pod name in an error.
				logger.Error(
					"Unknown-but-not-ignored Pod in Filter node's pods",
					zap.Object("pod", name),
					zap.Error(fmt.Errorf("Pod %v is unknown but not ignored", name)),
				)
			} else {
				includedIgnoredPods = append(includedIgnoredPods, name)
			}

			// We *also* need to count pods in ignored namespaces
			resources := extractPodResources(podInfo.Pod)
			nodeTotal.VCPU += resources.VCPU
			nodeTotal.Mem += resources.Mem
		}
	}

	if len(missedPods) != 0 {
		var missedPodsList []util.NamespacedName
		for name := range missedPods {
			missedPodsList = append(missedPodsList, name)
		}
		logger.Warn("Some known Pods weren't included in Filter NodeInfo", zap.Objects("missedPods", missedPodsList))
	}

	var kind string
	if vmInfo != nil {
		kind = "VM"
	} else {
		kind = "non-VM"
	}

	makeMsg := func(resource, compareOp string, nodeUse, podUse, nodeMax any) string {
		return fmt.Sprintf(
			"node %s usage %v + %s pod %s %v %s node max %v",
			resource, nodeUse, kind, resource, podUse, compareOp, nodeMax,
		)
	}

	allowing := true

	var cpuCompare string
	if nodeTotal.VCPU+podResources.VCPU > node.cpu.Total {
		cpuCompare = ">"
		allowing = false
	} else {
		cpuCompare = "<="
	}
	cpuMsg := makeMsg("vCPU", cpuCompare, nodeTotal.VCPU, podResources.VCPU, node.cpu.Total)

	var memCompare string
	if nodeTotal.Mem+podResources.Mem > node.mem.Total {
		memCompare = ">"
		allowing = false
	} else {
		memCompare = "<="
	}
	memMsg := makeMsg("vCPU", memCompare, nodeTotal.Mem, podResources.Mem, node.mem.Total)

	var message string
	var logFunc func(string, ...zap.Field)
	if allowing {
		message = "Allowing Pod"
		logFunc = logger.Info
	} else {
		message = "Rejecting Pod"
		logFunc = logger.Warn
	}

	logFunc(
		message,
		zap.Objects("includedIgnoredPods", includedIgnoredPods),
		zap.Object("verdict", verdictSet{
			cpu: cpuMsg,
			mem: memMsg,
		}),
	)

	if !allowing {
		return framework.NewStatus(framework.Unschedulable, "Not enough resources for pod")
	} else {
		return nil
	}
}

// Score allows our plugin to express which nodes should be preferred for scheduling new pods onto
//
// Even though this function is given (pod, node) pairs, our scoring is only really dependent on
// values of the node. However, we have special handling for when the pod no longer fits in the node
// (even though it might have during the Filter plugin) - we can't return a failure, because that
// would cause *all* scheduling of the pod to fail, so we instead return the minimum score.
//
// The scores might not be consistent with each other, due to ongoing changes in the node. That's
// ok, because nothing relies on strict correctness here, and they should be approximately correct
// anyways.
//
// Required for framework.ScorePlugin
func (e *AutoscaleEnforcer) Score(
	ctx context.Context,
	state *framework.CycleState,
	pod *corev1.Pod,
	nodeName string,
) (_ int64, status *framework.Status) {
	ignored := e.state.conf.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("Score", ignored)
	defer func() {
		e.metrics.IncFailIfNotSuccess("Score", ignored, status)
	}()

	logger := e.logger.With(zap.String("method", "Score"), zap.String("node", nodeName), util.PodNameFields(pod))
	logger.Info("Handling Score request")

	scoreLen := framework.MaxNodeScore - framework.MinNodeScore

	// Double-check that the SchedulerName matches what we're expecting
	if status := e.checkSchedulerName(logger, pod); status != nil {
		return framework.MinNodeScore, status
	}

	vmInfo, err := e.getVmInfo(logger, pod, "Score")
	if err != nil {
		logger.Error("Error getting VM info for Pod", zap.Error(err))
		return 0, framework.NewStatus(framework.Error, "Error getting info for pod")
	}

	// note: vmInfo may be nil here if the pod does not correspond to a NeonVM virtual machine

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	// Score by total resources available:
	node, err := e.state.getOrFetchNodeState(ctx, logger, e.metrics, e.nodeStore, nodeName)
	if err != nil {
		logger.Error("Error getting node state", zap.Error(err))
		return 0, framework.NewStatus(framework.Error, "Error fetching state for node")
	}

	var resources api.Resources
	if vmInfo != nil {
		resources = vmInfo.Using()
	} else {
		resources = extractPodResources(pod)
	}

	// Special case: return minimum score if we don't have room
	noRoom := resources.VCPU > node.remainingReservableCPU() ||
		resources.Mem > node.remainingReservableMem()
	if noRoom {
		score := framework.MinNodeScore
		logger.Warn("No room on node, giving minimum score (typically handled by Filter method)", zap.Int64("score", score))
		return score, nil
	}

	cpuRemaining := node.remainingReservableCPU()
	cpuTotal := node.cpu.Total
	memRemaining := node.remainingReservableMem()
	memTotal := node.mem.Total

	cpuFraction := 1 - cpuRemaining.AsFloat64()/cpuTotal.AsFloat64()
	memFraction := 1 - memRemaining.AsFloat64()/memTotal.AsFloat64()
	cpuScale := node.cpu.Total.AsFloat64() / e.state.maxTotalReservableCPU.AsFloat64()
	memScale := node.mem.Total.AsFloat64() / e.state.maxTotalReservableMem.AsFloat64()

	nodeConf := e.state.conf.NodeConfig

	// Refer to the comments in nodeConfig for more. Also, see: https://www.desmos.com/calculator/wg8s0yn63s
	calculateScore := func(fraction, scale float64) (float64, int64) {
		y0 := nodeConf.MinUsageScore
		y1 := nodeConf.MaxUsageScore
		xp := nodeConf.ScorePeak

		score := float64(1) // if fraction == nodeConf.ScorePeak
		if fraction < nodeConf.ScorePeak {
			score = y0 + (1-y0)/xp*fraction
		} else if fraction > nodeConf.ScorePeak {
			score = y1 + (1-y1)/(1-xp)*(1-fraction)
		}

		score *= scale

		return score, framework.MinNodeScore + int64(float64(scoreLen)*score)
	}

	cpuFScore, cpuIScore := calculateScore(cpuFraction, cpuScale)
	memFScore, memIScore := calculateScore(memFraction, memScale)

	score := util.Min(cpuIScore, memIScore)
	logger.Info(
		"Scored pod placement for node",
		zap.Int64("score", score),
		zap.Object("verdict", verdictSet{
			cpu: fmt.Sprintf(
				"%d remaining reservable of %d total => fraction=%g, scale=%g => score=(%g :: %d)",
				cpuRemaining, cpuTotal, cpuFraction, cpuScale, cpuFScore, cpuIScore,
			),
			mem: fmt.Sprintf(
				"%d remaining reservable of %d total => fraction=%g, scale=%g => score=(%g :: %d)",
				memRemaining, memTotal, memFraction, memScale, memFScore, memIScore,
			),
		}),
	)

	return score, nil
}

// NormalizeScore weights scores uniformly in the range [minScore, trueScore], where
// minScore is framework.MinNodeScore + 1.
func (e *AutoscaleEnforcer) NormalizeScore(
	ctx context.Context,
	state *framework.CycleState,
	pod *corev1.Pod,
	scores framework.NodeScoreList,
) (status *framework.Status) {
	ignored := e.state.conf.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("NormalizeScore", ignored)
	defer func() {
		e.metrics.IncFailIfNotSuccess("NormalizeScore", ignored, status)
	}()

	logger := e.logger.With(zap.String("method", "NormalizeScore"), util.PodNameFields(pod))
	logger.Info("Handling NormalizeScore request")

	for _, node := range scores {
		nodeScore := node.Score
		nodeName := node.Name

		// rand.Intn will panic if we pass in 0
		if nodeScore == 0 {
			logger.Info("Ignoring node as it was assigned a score of 0", zap.String("node", nodeName))
			continue
		}

		// This is different from framework.MinNodeScore. We use framework.MinNodeScore
		// to indicate that a pod should not be placed on a node. The lowest
		// actual score we assign a node is thus framework.MinNodeScore + 1
		minScore := framework.MinNodeScore + 1

		// We want to pick a score in the range [minScore, score], so use
		// score _+ 1_ - minscore, as rand.Intn picks a number in the _half open_
		// range [0, n)
		newScore := int64(rand.Intn(int(nodeScore+1-minScore))) + minScore
		logger.Info(
			"Randomly choosing newScore from range [minScore, trueScore]",
			zap.String("node", nodeName),
			zap.Int64("newScore", newScore),
			zap.Int64("minScore", minScore),
			zap.Int64("trueScore", nodeScore),
		)
		node.Score = newScore
	}
	return nil
}

// ScoreExtensions is required for framework.ScorePlugin, and can return nil if it's not used.
// However, we do use it, to randomize scores.
func (e *AutoscaleEnforcer) ScoreExtensions() framework.ScoreExtensions {
	if e.state.conf.RandomizeScores {
		return e
	} else {
		return nil
	}
}

// Reserve signals to our plugin that a particular pod will (probably) be bound to a node, giving us
// a chance to both (a) reserve the resources it needs within the node and (b) reject the pod if
// there aren't enough.
//
// Required for framework.ReservePlugin
func (e *AutoscaleEnforcer) Reserve(
	ctx context.Context,
	state *framework.CycleState,
	pod *corev1.Pod,
	nodeName string,
) (status *framework.Status) {
	ignored := e.state.conf.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("Reserve", ignored)
	defer func() {
		e.metrics.IncFailIfNotSuccess("Reserve", ignored, status)
	}()

	logger := e.logger.With(zap.String("method", "Reserve"), zap.String("node", nodeName), util.PodNameFields(pod))
	if migrationName := util.TryPodOwnerVirtualMachineMigration(pod); migrationName != nil {
		logger = logger.With(zap.Object("virtualmachinemigration", *migrationName))
	}

	logger.Info("Handling Reserve request")

	if ignored {
		// Generally, we shouldn't be getting plugin requests for resources that are ignored.
		logger.Warn("Ignoring Reserve request for pod in ignored namespace")
		return nil // success; allow the Pod onto the node.
	}

	// Double-check that the SchedulerName matches what we're expecting
	if status := e.checkSchedulerName(logger, pod); status != nil {
		return status
	}

	ok, verdict, err := e.reserveResources(ctx, logger, pod, "Reserve", true)
	if err != nil {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
	}

	if ok {
		logger.Info("Allowing reserve Pod", zap.Object("verdict", verdict))
		return nil // nil is success
	} else {
		logger.Error("Rejecting reserve Pod (not enough resources)", zap.Object("verdict", verdict))
		return framework.NewStatus(framework.Unschedulable, "Not enough resources to reserve non-VM pod")
	}
}

// Unreserve marks a pod as no longer on-track to being bound to a node, so we can release the
// resources we previously reserved for it.
//
// Required for framework.ReservePlugin.
//
// Note: the documentation for ReservePlugin indicates that Unreserve both (a) must be idempotent
// and (b) may be called without a previous call to Reserve for the same pod.
func (e *AutoscaleEnforcer) Unreserve(
	ctx context.Context,
	state *framework.CycleState,
	pod *corev1.Pod,
	nodeName string,
) {
	ignored := e.state.conf.ignoredNamespace(pod.Namespace)
	e.metrics.IncMethodCall("Unreserve", ignored)

	podName := util.GetNamespacedName(pod)

	logger := e.logger.With(zap.String("method", "Unreserve"), zap.String("node", nodeName), util.PodNameFields(pod))
	logger.Info("Handling Unreserve request")

	if ignored {
		// Generally, we shouldn't be getting plugin requests for resources that are ignored.
		logger.Warn("Ignoring Unreserve request for pod in ignored namespace")
		return
	}

	logFields, kind, migrating, verdict := e.unreserveResources(logger, podName)

	logger.With(logFields...).Info(
		fmt.Sprintf("Unreserved %s Pod", kind),
		zap.Bool("migrating", migrating),
		zap.Object("verdict", verdict),
	)
}
