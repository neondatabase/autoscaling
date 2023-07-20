package plugin

import (
	"context"
	"errors"
	"fmt"
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
const LabelVM = vmapi.VirtualMachineNameLabel
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
	obj runtime.Object,
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
		submitPodStarted: func(logger *zap.Logger, pod *corev1.Pod) {
			pushToQueue(logger, func() { p.handlePodStarted(hlogger, pod) })
		},
		submitVMDeletion: func(logger *zap.Logger, pod util.NamespacedName) {
			pushToQueue(logger, func() { p.handleVMDeletion(hlogger, pod) })
		},
		submitPodDeletion: func(logger *zap.Logger, name util.NamespacedName) {
			pushToQueue(logger, func() { p.handlePodDeletion(hlogger, name) })
		},
		submitPodStartMigration: func(logger *zap.Logger, podName, migrationName util.NamespacedName, source bool) {
			pushToQueue(logger, func() { p.handlePodStartMigration(logger, podName, migrationName, source) })
		},
		submitPodEndMigration: func(logger *zap.Logger, podName, migrationName util.NamespacedName) {
			pushToQueue(logger, func() { p.handlePodEndMigration(logger, podName, migrationName) })
		},
	}
	vwc := vmWatchCallbacks{
		submitVMDisabledScaling: func(logger *zap.Logger, pod util.NamespacedName) {
			pushToQueue(logger, func() { p.handleVMDisabledScaling(hlogger, pod) })
		},
		submitVMBoundsChanged: func(logger *zap.Logger, vm *api.VmInfo, podName string) {
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
	var vmName util.NamespacedName
	vmName.Namespace = pod.Namespace

	var ok bool
	vmName.Name, ok = pod.Labels[LabelVM]
	if !ok {
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
//
// This method expects e.state.lock to be held when it is called. It will not release the lock.
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

	var otherPodInfo podOtherResourceState
	if vmInfo == nil {
		otherPodInfo, err = extractPodOtherPodResourceState(pod)
		if err != nil {
			logger.Error("Error extracing resource state for non-VM pod", zap.Error(err))
			return framework.NewStatus(
				framework.UnschedulableAndUnresolvable,
				fmt.Sprintf("Error getting pod info: %s", err),
			)
		}
	}

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	// Check that the SchedulerName matches what we're expecting
	if status := e.checkSchedulerName(logger, pod); status != nil {
		return status
	}

	// Check whether the pod's memory slot size matches the scheduler's. If it doesn't, reject it.
	if vmInfo != nil && !vmInfo.Mem.SlotSize.Equal(e.state.conf.MemSlotSize) {
		err := fmt.Errorf("expected %v, found %v", e.state.conf.MemSlotSize, vmInfo.Mem.SlotSize)
		logger.Error("VM for Pod has a bad MemSlotSize", zap.Error(err))
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("VM for pod has bad MemSlotSize: %v", err),
		)
	}

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
	var totalNodeVCPU vmapi.MilliCPU
	var totalNodeMem uint16
	var otherResources nodeOtherResourceState

	otherResources.MarginCPU = node.otherResources.MarginCPU
	otherResources.MarginMemory = node.otherResources.MarginMemory

	// As we process all pods, we should record all the pods that aren't present in both nodeInfo
	// and e.state's maps, so that we can log any inconsistencies instead of silently using
	// *potentially* bad data. Some differences are expected, but on the whole this extra
	// information should be helpful.
	missedPods := make(map[util.NamespacedName]struct{})
	for name := range node.pods {
		missedPods[name] = struct{}{}
	}
	for name := range node.otherPods {
		missedPods[name] = struct{}{}
	}

	var unknownPods []util.NamespacedName

	for _, podInfo := range nodeInfo.Pods {
		pn := util.NamespacedName{Name: podInfo.Pod.Name, Namespace: podInfo.Pod.Namespace}
		if podState, ok := e.state.podMap[pn]; ok {
			totalNodeVCPU += podState.vCPU.Reserved
			totalNodeMem += podState.memSlots.Reserved
			delete(missedPods, pn)
		} else if otherPodState, ok := e.state.otherPods[pn]; ok {
			oldRes := otherResources
			otherResources = oldRes.addPod(&e.state.conf.MemSlotSize, otherPodState.resources)
			totalNodeVCPU += otherResources.ReservedCPU - oldRes.ReservedCPU
			totalNodeMem += otherResources.ReservedMemSlots - oldRes.ReservedMemSlots
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

			unknownPods = append(unknownPods, name)

			if !e.state.conf.ignoredNamespace(podInfo.Pod.Namespace) {
				// FIXME: this gets us duplicated "pod" fields. Not great. But we're using
				// logger.With pretty pervasively, and it's hard to avoid this while using that.
				// For now, we can get around this by including the pod name in an error.
				logger.Error(
					"Unknown-but-not-ignored Pod in Filter node's pods",
					zap.Object("pod", name),
					zap.Error(fmt.Errorf("Pod %v is unknown but not ignored", name)),
				)
			}

			// We *also* need to count pods in ignored namespaces
			resources, err := extractPodOtherPodResourceState(podInfo.Pod)
			if err != nil {
				// FIXME: Same duplicate "pod" field issue as above; same temporary solution.
				logger.Error(
					"Error extracting resource state for non-VM Pod",
					zap.Object("pod", name),
					zap.Error(fmt.Errorf("Error extracting resource state for %v: %w", name, err)),
				)
				continue
			}

			oldRes := otherResources
			otherResources = oldRes.addPod(&e.state.conf.MemSlotSize, resources)
			totalNodeVCPU += otherResources.ReservedCPU - oldRes.ReservedCPU
			totalNodeMem += otherResources.ReservedMemSlots - oldRes.ReservedMemSlots
		}
	}

	if len(missedPods) != 0 {
		var missedPodsList []util.NamespacedName
		for name := range missedPods {
			missedPodsList = append(missedPodsList, name)
		}
		logger.Warn("Some known Pods weren't included in Filter NodeInfo", zap.Objects("missedPods", missedPodsList))
	}
	if len(unknownPods) != 0 {
		logger.Warn("Received unknown pods from Filter NodeInfo", zap.Objects("unknownPods", unknownPods))
	}

	nodeTotalReservableCPU := node.totalReservableCPU()
	nodeTotalReservableMemSots := node.totalReservableMemSlots()

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
	var cpuMsg string
	var memMsg string

	if vmInfo != nil {
		var cpuCompare string
		if totalNodeVCPU+vmInfo.Cpu.Use > nodeTotalReservableCPU {
			cpuCompare = ">"
			allowing = false
		} else {
			cpuCompare = "<="
		}
		cpuMsg = makeMsg("vCPU", cpuCompare, totalNodeVCPU, vmInfo.Cpu.Use, nodeTotalReservableCPU)

		var memCompare string
		if totalNodeMem+vmInfo.Mem.Use > nodeTotalReservableMemSots {
			memCompare = ">"
			allowing = false
		} else {
			memCompare = "<="
		}
		memMsg = makeMsg("memSlots", memCompare, totalNodeMem, vmInfo.Mem.Use, nodeTotalReservableMemSots)
	} else {
		newRes := otherResources.addPod(&e.state.conf.MemSlotSize, otherPodInfo)
		cpuIncr := newRes.ReservedCPU - otherResources.ReservedCPU
		memIncr := newRes.ReservedMemSlots - otherResources.ReservedMemSlots

		var cpuCompare string
		if totalNodeVCPU+cpuIncr > nodeTotalReservableCPU {
			cpuCompare = ">"
			allowing = false
		} else {
			cpuCompare = "<="
		}
		cpuMsg = makeMsg("vCPU", cpuCompare, totalNodeVCPU, cpuIncr, nodeTotalReservableCPU)

		var memCompare string
		if totalNodeMem+memIncr > nodeTotalReservableMemSots {
			memCompare = ">"
			allowing = false
		} else {
			memCompare = "<="
		}
		memMsg = makeMsg("memSlots", memCompare, totalNodeMem, memIncr, nodeTotalReservableMemSots)
	}

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

	vmInfo, err := e.getVmInfo(logger, pod, "Score")
	if err != nil {
		logger.Error("Error getting VM info for Pod", zap.Error(err))
		return 0, framework.NewStatus(framework.Error, "Error getting info for pod")
	}

	// note: vmInfo may be nil here if the pod does not correspond to a NeonVM virtual machine

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	// Double-check that the SchedulerName matches what we're expecting
	if status := e.checkSchedulerName(logger, pod); status != nil {
		return framework.MinNodeScore, status
	}

	// Score by total resources available:
	node, err := e.state.getOrFetchNodeState(ctx, logger, e.metrics, e.nodeStore, nodeName)
	if err != nil {
		logger.Error("Error getting node state", zap.Error(err))
		return 0, framework.NewStatus(framework.Error, "Error fetching state for node")
	}

	// Special case: return minimum score if we don't have room
	noRoom := vmInfo != nil &&
		(vmInfo.Cpu.Use > node.remainingReservableCPU() ||
			vmInfo.Mem.Use > node.remainingReservableMemSlots())
	if noRoom {
		score := framework.MinNodeScore
		logger.Warn("No room on node, giving minimum score (typically handled by Filter method)", zap.Int64("score", score))
		return score, nil
	}

	remainingCPU := node.remainingReservableCPU()
	remainingMem := node.remainingReservableMemSlots()
	totalCPU := e.state.maxTotalReservableCPU
	totalMem := e.state.maxTotalReservableMemSlots

	// The ordering of multiplying before dividing is intentional; it allows us to get an exact
	// result, because scoreLen and total will both be small (i.e. their product fits within an int64)
	cpuScore := framework.MinNodeScore + scoreLen*int64(remainingCPU)/int64(totalCPU)
	memScore := framework.MinNodeScore + scoreLen*int64(remainingMem)/int64(totalMem)

	score := util.Min(cpuScore, memScore)
	logger.Info(
		"Scored pod placement for node",
		zap.Int64("score", score),
		zap.Object("verdict", verdictSet{
			cpu: fmt.Sprintf("%d remaining reservable of %d total => score is %d", remainingCPU, totalCPU, cpuScore),
			mem: fmt.Sprintf("%d remaining reservable of %d total => score is %d", remainingMem, totalMem, memScore),
		}),
	)

	return score, nil
}

// ScoreExtensions is required for framework.ScorePlugin, and can return nil if it's not used
func (e *AutoscaleEnforcer) ScoreExtensions() framework.ScoreExtensions {
	return nil
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

	pName := util.GetNamespacedName(pod)
	logger := e.logger.With(zap.String("method", "Reserve"), zap.String("node", nodeName), util.PodNameFields(pod))
	if migrationName := tryMigrationOwnerReference(pod); migrationName != nil {
		logger = logger.With(zap.Object("virtualmachinemigration", *migrationName))
	}

	logger.Info("Handling Reserve request")

	if ignored {
		// Generally, we shouldn't be getting plugin requests for resources that are ignored.
		logger.Warn("Ignoring Reserve request for pod in ignored namespace")
		return nil // success; allow the Pod onto the node.
	}

	vmInfo, err := e.getVmInfo(logger, pod, "Reserve")
	if err != nil {
		logger.Error("Error getting VM info for pod", zap.Error(err))
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("Error getting pod vmInfo: %s", err),
		)
	}

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	// Double-check that the SchedulerName matches what we're expecting
	if status := e.checkSchedulerName(logger, pod); status != nil {
		return status
	}

	// Double-check that the VM's memory slot size still matches ours. This should be ensured by
	// our implementation of Filter, but this would be a *pain* to debug if it went wrong somehow.
	if vmInfo != nil && !vmInfo.Mem.SlotSize.Equal(e.state.conf.MemSlotSize) {
		err := fmt.Errorf(
			"expected %v, found %v (this should have been caught during Filter)",
			e.state.conf.MemSlotSize, vmInfo.Mem.SlotSize,
		)
		logger.Error("VM for Pod has a bad MemSlotSize", zap.Error(err))
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("VM for pod has bad MemSlotSize: %v", err),
		)
	}

	node, err := e.state.getOrFetchNodeState(ctx, logger, e.metrics, e.nodeStore, nodeName)
	if err != nil {
		logger.Error("Error getting node state", zap.Error(err))
		return framework.NewStatus(
			framework.Error,
			fmt.Sprintf("Error getting node state: %s", err),
		)
	}

	// if this is a non-VM pod, use a different set of information for it.
	if vmInfo == nil {
		podResources, err := extractPodOtherPodResourceState(pod)
		if err != nil {
			logger.Error("Error extracing resource state for non-VM pod", zap.Error(err))
			return framework.NewStatus(
				framework.UnschedulableAndUnresolvable,
				fmt.Sprintf("Error getting non-VM pod info: %v", err),
			)
		}

		oldNodeRes := node.otherResources
		newNodeRes := node.otherResources.addPod(&e.state.conf.MemSlotSize, podResources)

		addCpu := newNodeRes.ReservedCPU - oldNodeRes.ReservedCPU
		addMem := newNodeRes.ReservedMemSlots - oldNodeRes.ReservedMemSlots

		if addCpu <= node.remainingReservableCPU() && addMem <= node.remainingReservableMemSlots() {
			oldNodeCpuReserved := node.vCPU.Reserved
			oldNodeMemReserved := node.memSlots.Reserved

			node.otherResources = newNodeRes
			node.vCPU.Reserved += addCpu
			node.memSlots.Reserved += addMem

			ps := &otherPodState{
				name:      pName,
				node:      node,
				resources: podResources,
			}
			node.otherPods[pName] = ps
			e.state.otherPods[pName] = ps

			cpuVerdict := fmt.Sprintf(
				"node reserved %d -> %d, node other resources %d -> %d rounded (%v -> %v raw, %v margin)",
				oldNodeCpuReserved, node.vCPU.Reserved, oldNodeRes.ReservedCPU, newNodeRes.ReservedCPU, &oldNodeRes.RawCPU, &newNodeRes.RawCPU, newNodeRes.MarginCPU,
			)
			memVerdict := fmt.Sprintf(
				"node reserved %d -> %d, node other resources %d -> %d slots (%v -> %v raw, %v margin)",
				oldNodeMemReserved, node.memSlots.Reserved, oldNodeRes.ReservedMemSlots, newNodeRes.ReservedMemSlots, &oldNodeRes.RawMemory, &newNodeRes.RawMemory, newNodeRes.MarginMemory,
			)

			logger.Info(
				"Allowing reserve non-VM pod",
				zap.Object("verdict", verdictSet{
					cpu: cpuVerdict,
					mem: memVerdict,
				}),
			)

			node.updateMetrics(e.metrics, e.state.memSlotSizeBytes())

			return nil // nil is success
		} else {
			cpuShortVerdict := "NOT ENOUGH"
			if addCpu <= node.remainingReservableCPU() {
				cpuShortVerdict = "OK"
			}
			memShortVerdict := "NOT ENOUGH"
			if addMem <= node.remainingReservableMemSlots() {
				memShortVerdict = "OK"
			}

			cpuVerdict := fmt.Sprintf(
				"need %v (%v -> %v raw), %v of %v used, so %v available (%s)",
				addCpu, &oldNodeRes.RawCPU, &newNodeRes.RawCPU, node.vCPU.Reserved, node.totalReservableCPU(), node.remainingReservableCPU(), cpuShortVerdict,
			)
			memVerdict := fmt.Sprintf(
				"need %v (%v -> %v raw), %v of %v used, so %v available (%s)",
				addMem, &oldNodeRes.RawMemory, &newNodeRes.RawMemory, node.memSlots.Reserved, node.totalReservableMemSlots(), node.remainingReservableMemSlots(), memShortVerdict,
			)

			logger.Error(
				"Can't reserve non-VM pod (not enough resources)",
				zap.Object("verdict", verdictSet{
					cpu: cpuVerdict,
					mem: memVerdict,
				}),
			)
			return framework.NewStatus(framework.Unschedulable, "Not enough resources to reserve non-VM pod")
		}
	}

	// Otherwise, it's a VM. Use the vmInfo
	//
	// If there's capacity to reserve the pod, do that. Otherwise, reject the pod. Most capacity
	// checks will be handled in the calls to Filter, but it's possible for another VM to scale up
	// in between the calls to Filter and Reserve, removing the resource availability that we
	// thought we had.
	if vmInfo.Cpu.Use <= node.remainingReservableCPU() && vmInfo.Mem.Use <= node.remainingReservableMemSlots() {
		newNodeReservedCPU := node.vCPU.Reserved + vmInfo.Cpu.Use
		newNodeReservedMemSlots := node.memSlots.Reserved + vmInfo.Mem.Use

		cpuVerdict := fmt.Sprintf("node reserved %v + %v -> %v", node.vCPU.Reserved, vmInfo.Cpu.Use, newNodeReservedCPU)
		memVerdict := fmt.Sprintf("node reserved %v + %v -> %v", node.memSlots.Reserved, vmInfo.Mem.Use, newNodeReservedMemSlots)

		logger.Info(
			"Allowing reserve VM pod",
			zap.Object("verdict", verdictSet{
				cpu: cpuVerdict,
				mem: memVerdict,
			}),
		)

		node.vCPU.Reserved = newNodeReservedCPU
		node.memSlots.Reserved = newNodeReservedMemSlots
		ps := &podState{
			name:   pName,
			vmName: vmInfo.NamespacedName(),
			node:   node,
			vCPU: podResourceState[vmapi.MilliCPU]{
				Reserved:         vmInfo.Cpu.Use,
				Buffer:           0,
				CapacityPressure: 0,
				Min:              vmInfo.Cpu.Min,
				Max:              vmInfo.Cpu.Max,
			},
			memSlots: podResourceState[uint16]{
				Reserved:         vmInfo.Mem.Use,
				Buffer:           0,
				CapacityPressure: 0,
				Min:              vmInfo.Mem.Min,
				Max:              vmInfo.Mem.Max,
			},
			testingOnlyAlwaysMigrate: vmInfo.AlwaysMigrate,
			mostRecentComputeUnit:    nil,
			metrics:                  nil,
			mqIndex:                  -1,
			migrationState:           nil,
		}
		node.pods[pName] = ps
		e.state.podMap[pName] = ps

		node.updateMetrics(e.metrics, e.state.memSlotSizeBytes())

		return nil // nil is success
	} else {
		cpuShortVerdict := "NOT ENOUGH"
		if vmInfo.Cpu.Use <= node.remainingReservableCPU() {
			cpuShortVerdict = "OK"
		}
		memShortVerdict := "NOT ENOUGH"
		if vmInfo.Mem.Use <= node.remainingReservableMemSlots() {
			memShortVerdict = "OK"
		}

		cpuVerdict := fmt.Sprintf(
			"need %v, %v of %v used, so %v available (%s)",
			vmInfo.Cpu.Use, node.vCPU.Reserved, node.totalReservableCPU(), node.remainingReservableCPU(), cpuShortVerdict,
		)
		memVerdict := fmt.Sprintf(
			"need %v, %v of %v used, so %v available (%s)",
			vmInfo.Mem.Use, node.memSlots.Reserved, node.totalReservableMemSlots(), node.remainingReservableMemSlots(), memShortVerdict,
		)

		logger.Error(
			"Can't reserve VM pod (not enough resources)",
			zap.Object("verdict", verdictSet{
				cpu: cpuVerdict,
				mem: memVerdict,
			}),
		)
		return framework.NewStatus(framework.Unschedulable, "Not enough resources to reserve VM pod")
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

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	ps, ok := e.state.podMap[podName]
	otherPs, otherOk := e.state.otherPods[podName]
	// FIXME: we should guarantee that we can never have an entry in both maps with the same name.
	// This needs to be handled in Reserve, not here.
	if !ok && otherOk {
		vCPUVerdict, memVerdict := handleDeletedPod(otherPs.node, otherPs.resources, &e.state.conf.MemSlotSize)
		delete(e.state.otherPods, podName)
		delete(otherPs.node.otherPods, podName)

		logger.Info(
			"Unreserved non-VM pod",
			zap.Object("verdict", verdictSet{
				cpu: vCPUVerdict,
				mem: memVerdict,
			}),
		)

		otherPs.node.updateMetrics(e.metrics, e.state.memSlotSizeBytes())
	} else if ok && !otherOk {
		// Mark the resources as no longer reserved

		currentlyMigrating := false // Unreserve is never called on bound pods, so it can't be migrating.
		vCPUVerdict := collectResourceTransition(&ps.node.vCPU, &ps.vCPU).
			handleDeleted(currentlyMigrating)
		memVerdict := collectResourceTransition(&ps.node.memSlots, &ps.memSlots).
			handleDeleted(currentlyMigrating)

		// Delete our record of the pod
		delete(e.state.podMap, podName)
		delete(ps.node.pods, podName)
		ps.node.mq.removeIfPresent(ps)

		logger.Info(
			"Unreserved Pod",
			zap.Object("verdict", verdictSet{
				cpu: vCPUVerdict,
				mem: memVerdict,
			}),
		)

		ps.node.updateMetrics(e.metrics, e.state.memSlotSizeBytes())
	} else {
		logger.Warn("Cannot find pod in podMap or otherPods")
		return
	}
}
