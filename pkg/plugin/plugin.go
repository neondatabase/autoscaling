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
}

// abbreviation, because this type is pretty verbose
type IndexedVMStore = watch.IndexedStore[vmapi.VirtualMachine, *watch.NameIndex[vmapi.VirtualMachine]]

// Compile-time checks that AutoscaleEnforcer actually implements the interfaces we want it to
var _ framework.Plugin = (*AutoscaleEnforcer)(nil)
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
		// remaining fields are set by p.readClusterState and p.startPrometheusServer
		state: pluginState{ //nolint:exhaustruct // see above.
			lock: util.NewChanMutex(),
			conf: config,
		},
		metrics: PromMetrics{},                                                                      //nolint:exhaustruct // set by startPrometheusServer
		vmStore: watch.IndexedStore[vmapi.VirtualMachine, *watch.NameIndex[vmapi.VirtualMachine]]{}, // set below.
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
	submitVMPodDeletion := func(logger *zap.Logger, pod util.NamespacedName) {
		pushToQueue(logger, func() { p.handleVMDeletion(hlogger, pod) })
	}
	submitNonVMPodDeletion := func(logger *zap.Logger, name util.NamespacedName) {
		pushToQueue(logger, func() { p.handlePodDeletion(hlogger, name) })
	}
	submitVMDisabledScaling := func(logger *zap.Logger, pod util.NamespacedName) {
		pushToQueue(logger, func() { p.handleVMDisabledScaling(hlogger, pod) })
	}
	submitVMBoundsChanged := func(logger *zap.Logger, vm *api.VmInfo, podName string) {
		pushToQueue(logger, func() { p.handleUpdatedScalingBounds(hlogger, vm, podName) })
	}
	submitNonAutoscalingVmUsageChanged := func(logger *zap.Logger, vm *api.VmInfo, podName string) {
		pushToQueue(logger, func() { p.handleNonAutoscalingUsageChange(hlogger, vm, podName) })
	}

	watchMetrics := watch.NewMetrics("autoscaling_plugin_watchers")

	logger.Info("Starting pod watcher")
	if err := p.watchPodEvents(ctx, logger, watchMetrics, submitVMPodDeletion, submitNonVMPodDeletion); err != nil {
		return nil, fmt.Errorf("Error starting pod watcher: %w", err)
	}

	logger.Info("Starting VM watcher")
	vmStore, err := p.watchVMEvents(ctx, logger, watchMetrics, submitVMDisabledScaling, submitVMBoundsChanged, submitNonAutoscalingVmUsageChanged)
	if err != nil {
		return nil, fmt.Errorf("Error starting VM watcher: %w", err)
	}

	p.vmStore = watch.NewIndexedStore(vmStore, watch.NewNameIndex[vmapi.VirtualMachine]())

	// ... but before handling the events, read the current cluster state:
	logger.Info("Reading initial cluster state")
	if err = p.readClusterState(ctx, logger); err != nil {
		return nil, fmt.Errorf("Error reading cluster state: %w", err)
	}

	go func() {
		iter := queue.Iterator()
		for iter.Next(ctx) {
			callback := iter.Value()
			callback()
		}
		if err := iter.Close(); err != nil {
			logger.Info("Stopped waiting on pod/VM queue", zap.Error(err))
		}
	}()

	promReg := p.makePrometheusRegistry()
	watchMetrics.MustRegister(promReg)

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
func getVmInfo(logger *zap.Logger, vmStore IndexedVMStore, pod *corev1.Pod) (*api.VmInfo, error) {
	var vmName util.NamespacedName
	vmName.Namespace = pod.Namespace

	var ok bool
	vmName.Name, ok = pod.Labels[LabelVM]
	if !ok {
		return nil, nil
	}

	vm, ok := vmStore.GetIndexed(func(index *watch.NameIndex[vmapi.VirtualMachine]) (*vmapi.VirtualMachine, bool) {
		return index.Get(vmName.Namespace, vmName.Name)
	})
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
		case <-vmStore.Relist():
		case <-timer.C:
			return nil, fmt.Errorf("Timed out waiting on VM store relist (timeout = %s)", timeout)
		}

		// retry fetching the VM, now that we know it's been synced.
		vm, ok = vmStore.GetIndexed(func(index *watch.NameIndex[vmapi.VirtualMachine]) (*vmapi.VirtualMachine, bool) {
			return index.Get(vmName.Namespace, vmName.Name)
		})
		if !ok {
			// if the VM is still not present after relisting, then either it's already been deleted
			// or there's a deeper problem.
			return nil, errors.New("Could not find VM for pod, even after relist")
		}
	}

	vmInfo, err := api.ExtractVmInfo(logger, vm)
	if err != nil {
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

// Filter gives our plugin a chance to signal that a pod shouldn't be put onto a particular node
//
// Required for framework.FilterPlugin
func (e *AutoscaleEnforcer) Filter(
	ctx context.Context,
	state *framework.CycleState,
	pod *corev1.Pod,
	nodeInfo *framework.NodeInfo,
) *framework.Status {
	e.metrics.pluginCalls.WithLabelValues("filter").Inc()

	nodeName := nodeInfo.Node().Name // TODO: nodes also have namespaces? are they used at all?

	logger := e.logger.With(zap.String("method", "Filter"), zap.String("node", nodeName), util.PodNameFields(pod))
	logger.Info("Handling Filter request")

	vmInfo, err := getVmInfo(logger, e.vmStore, pod)
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

	node, err := e.state.getOrFetchNodeState(ctx, logger, e.handle, nodeName)
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

	for _, podInfo := range nodeInfo.Pods {
		pn := util.NamespacedName{Name: podInfo.Pod.Name, Namespace: podInfo.Pod.Namespace}
		if podState, ok := e.state.podMap[pn]; ok {
			totalNodeVCPU += podState.vCPU.Reserved
			totalNodeMem += podState.memSlots.Reserved
		} else if otherPodState, ok := e.state.otherPods[pn]; ok {
			oldRes := otherResources
			otherResources = oldRes.addPod(&e.state.conf.MemSlotSize, otherPodState.resources)
			totalNodeVCPU += otherResources.ReservedCPU - oldRes.ReservedCPU
			totalNodeMem += otherResources.ReservedMemSlots - oldRes.ReservedMemSlots
		}
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

	var resultString string
	if allowing {
		resultString = "Allowing"
	} else {
		resultString = "Rejecting"
	}

	logger.Info(
		fmt.Sprintf("%s Pod", resultString),
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
) (int64, *framework.Status) {
	e.metrics.pluginCalls.WithLabelValues("score").Inc()

	logger := e.logger.With(zap.String("method", "Score"), zap.String("node", nodeName), util.PodNameFields(pod))
	logger.Info("Handling Score request")

	scoreLen := framework.MaxNodeScore - framework.MinNodeScore

	vmInfo, err := getVmInfo(logger, e.vmStore, pod)
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
	node, err := e.state.getOrFetchNodeState(ctx, logger, e.handle, nodeName)
	if err != nil {
		logger.Error("Error getting node state", zap.Error(err))
		return 0, framework.NewStatus(framework.Error, "Error fetching state for node")
	}

	// Special case: return minimum score if we don't have room
	noRoom := vmInfo != nil &&
		(vmInfo.Cpu.Use > node.remainingReservableCPU() ||
			vmInfo.Mem.Use > node.remainingReservableMemSlots())
	if noRoom {
		return framework.MinNodeScore, nil
	}

	totalMilliCpu := int64(node.totalReservableCPU())
	totalMem := int64(node.totalReservableMemSlots())
	maxTotalMilliCpu := int64(e.state.maxTotalReservableCPU)
	maxTotalMem := int64(e.state.maxTotalReservableMemSlots)

	// The ordering of multiplying before dividing is intentional; it allows us to get an exact
	// result, because scoreLen and total will both be small (i.e. their product fits within an int64)
	scoreCpu := framework.MinNodeScore + scoreLen*totalMilliCpu/maxTotalMilliCpu
	scoreMem := framework.MinNodeScore + scoreLen*totalMem/maxTotalMem

	// return the minimum of the two resources scores
	if scoreCpu < scoreMem {
		return scoreCpu, nil
	} else {
		return scoreMem, nil
	}
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
) *framework.Status {
	e.metrics.pluginCalls.WithLabelValues("reserve").Inc()

	pName := util.GetNamespacedName(pod)
	logger := e.logger.With(zap.String("method", "Score"), zap.String("node", nodeName), util.PodNameFields(pod))
	logger.Info("Handling Reserve request")

	vmInfo, err := getVmInfo(logger, e.vmStore, pod)
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

	node, err := e.state.getOrFetchNodeState(ctx, logger, e.handle, nodeName)
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
				"need %v vCPU (%v -> %v raw), have %v available (%s)",
				addCpu, &oldNodeRes.RawCPU, &newNodeRes.RawCPU, node.remainingReservableCPU(), cpuShortVerdict,
			)
			memVerdict := fmt.Sprintf(
				"need %v mem slots (%v -> %v raw), have %d available (%s)",
				addMem, &oldNodeRes.RawMemory, &newNodeRes.RawMemory, node.remainingReservableMemSlots(), memShortVerdict,
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

		e.logger.Info(
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

		cpuVerdict := fmt.Sprintf("need %v vCPU, have %v available (%s)", vmInfo.Cpu.Use, node.remainingReservableCPU(), cpuShortVerdict)
		memVerdict := fmt.Sprintf("need %v mem slots, have %v available (%s)", vmInfo.Mem.Use, node.remainingReservableMemSlots(), memShortVerdict)

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
	e.metrics.pluginCalls.WithLabelValues("unreserve").Inc()

	podName := util.GetNamespacedName(pod)

	logger := e.logger.With(zap.String("method", "Filter"), zap.String("node", nodeName), util.PodNameFields(pod))
	logger.Info("Handling Unreserve request")

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
	} else {
		logger.Warn("Cannot find pod in podMap in otherPods")
		return
	}
}
