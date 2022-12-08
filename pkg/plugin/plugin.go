package plugin

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	scheme "k8s.io/client-go/kubernetes/scheme"
	rest "k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	vmapi "github.com/neondatabase/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
)

const Name = "AutoscaleEnforcer"
const LabelVM = vmapi.VirtualMachineNameLabel
const ConfigMapNamespace = "kube-system"
const ConfigMapName = "scheduler-plugin-config"
const ConfigMapKey = "autoscaler-enforcer-config.json"
const InitConfigMapTimeoutSeconds = 5

// AutoscaleEnforcer is the scheduler plugin to coordinate autoscaling
type AutoscaleEnforcer struct {
	handle   framework.Handle
	vmClient *vmclient.Clientset
	state    pluginState
}

// Compile-time checks that AutoscaleEnforcer actually implements the interfaces we want it to
var _ framework.Plugin = (*AutoscaleEnforcer)(nil)
var _ framework.FilterPlugin = (*AutoscaleEnforcer)(nil)
var _ framework.ScorePlugin = (*AutoscaleEnforcer)(nil)
var _ framework.ReservePlugin = (*AutoscaleEnforcer)(nil)

// NewAutoscaleEnforcerPlugin produces the initial AutoscaleEnforcer plugin to be used by the
// scheduler
func NewAutoscaleEnforcerPlugin(obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	// ^ obj can be used for taking in configuration. it's a bit tricky to figure out, and we don't
	// quite need it yet.
	klog.Info("[autoscale-enforcer] Initializing plugin")

	// create the NeonVM client
	vmapi.AddToScheme(scheme.Scheme)
	vmConfig := rest.CopyConfig(h.KubeConfig())
	// The handler's ContentType is not the default "application/json" (it's protobuf), so we need
	// to set it back to JSON because NeonVM doesn't support protobuf.
	vmConfig.ContentType = "application/json"
	vmClient, err := vmclient.NewForConfig(vmConfig)
	if err != nil {
		return nil, fmt.Errorf("Error creating NeonVM client: %s", err)
	}

	p := AutoscaleEnforcer{
		handle:   h,
		vmClient: vmClient,
		state: pluginState{
			nodeMap:   make(map[string]*nodeState),
			podMap:    make(map[api.PodName]*podState),
			otherPods: make(map[api.PodName]*otherPodState),
			// zero values are ok for the rest here
		},
	}

	if err := p.setConfigAndStartWatcher(); err != nil {
		klog.Errorf("Error starting config watcher: %s", err)
		return nil, err
	}

	vmDeletions := make(chan api.PodName)
	podDeletions := make(chan api.PodName)
	if err := p.watchPodDeletions(context.Background(), vmDeletions, podDeletions); err != nil {
		return nil, fmt.Errorf("Error starting VM deletion watcher: %s", err)
	}

	// TODO: this is a little clumsy; both channels are sent on by the same goroutine and both
	// consumed by this one. This should be changed, probably with handleVMDeletion and
	// handlePodDeletion being merged into one function.
	go func() {
		for {
			select {
			case name := <-vmDeletions:
				p.handleVMDeletion(name)
			case name := <-podDeletions:
				p.handlePodDeletion(name)
			}
		}
	}()

	go p.runPermitHandler()

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
func getVmInfo(ctx context.Context, vmClient *vmclient.Clientset, pod *corev1.Pod) (*api.VmInfo, error) {
	vmName, ok := pod.Labels[LabelVM]
	if !ok {
		return nil, nil
	}

	vm, err := vmClient.NeonvmV1().VirtualMachines(pod.Namespace).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("Error getting VM object %s:%s: %w", pod.Namespace, vmName, err)
	}

	vmInfo, err := api.ExtractVmInfo(vm)
	if err != nil {
		return nil, fmt.Errorf("Error extracting VM info: %w", err)
	}

	return vmInfo, nil
}

// checkSchedulerName asserts that the SchedulerName field of a Pod matches what we're expecting,
// otherwise returns a non-nil framework.Status to return (and also logs the error)
//
// This method expects e.state.lock to be held when it is called. It will not release the lock.
func (e *AutoscaleEnforcer) checkSchedulerName(pod *corev1.Pod) *framework.Status {
	if e.state.conf.SchedulerName != pod.Spec.SchedulerName {
		podName := api.PodName{Name: pod.Name, Namespace: pod.Namespace}
		err := fmt.Sprintf(
			"Mismatched SchedulerName for pod %v: our config has %q, but the pod has %q",
			podName, e.state.conf.SchedulerName, pod.Spec.SchedulerName,
		)
		klog.Errorf("[autoscale-enforcer] %s", err)
		return framework.NewStatus(framework.Error, err)
	}
	return nil
}

// Filter gives our plugin a chance to signal that a pod shouldn't be put onto a particular node
//
// Required for framework.FilterPlugin
func (e *AutoscaleEnforcer) Filter(
	ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeInfo *framework.NodeInfo,
) *framework.Status {
	nodeName := nodeInfo.Node().Name // TODO: nodes also have namespaces? are they used at all?

	pName := api.PodName{Name: pod.Name, Namespace: pod.Namespace}
	klog.Infof("[autoscale-enforcer] Filter: Handling request for pod %v, node %s", pName, nodeName)

	vmInfo, err := getVmInfo(ctx, e.vmClient, pod)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Filter: Error getting pod %v vmInfo: %s", pName, err)
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("Error getting pod vmInfo: %s", err),
		)
	} else if vmInfo == nil {
		// this pod isn't a VM. We don't care about it as much - it'll still be affected by scoring,
		// which *kind of* overlaps with filtering.
		return nil
	}

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	// Check that the SchedulerName matches what we're expecting
	if status := e.checkSchedulerName(pod); status != nil {
		return status
	}

	// Check whether the pod's memory slot size matches the scheduler's. If it doesn't, reject it.
	if !vmInfo.Mem.SlotSize.Equal(e.state.conf.MemSlotSize) {
		err := fmt.Errorf("expected %v, found %v", e.state.conf.MemSlotSize, vmInfo.Mem.SlotSize)
		klog.Warningf("[autoscale-enforcer] Filter: VM for pod %v has bad MemSlotSize: %w", pName, err)
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("VM for pod has bad MemSlotSize: %v", err),
		)
	}

	node, err := e.state.getOrFetchNodeState(ctx, e.handle, nodeName)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Filter: Error getting node %s state: %s", nodeName, err)
		return framework.NewStatus(
			framework.Error,
			fmt.Sprintf("Error getting node state: %s", err),
		)
	}

	// The pod will resources according to vmInfo.{Cpu,Mem}.Use reserved for it when it does get
	// scheduled. Now we can check whether this node has capacity for the pod.
	//
	// Technically speaking, the VM pods in nodeInfo might not match what we have recorded for the
	// node -- simply because during preemption, the scheduler tries to see whether it could
	// schedule the pod if other stuff was preempted, and gives us what the state WOULD be after
	// preemption.
	//
	// So we have to actually count up the resource usage of all pods in nodeInfo:
	var totalNodeVCPU, totalNodeMem uint16
	for _, podInfo := range nodeInfo.Pods {
		pn := api.PodName{Name: podInfo.Pod.Name, Namespace: podInfo.Pod.Namespace}
		if podState, ok := e.state.podMap[pn]; ok {
			totalNodeVCPU += podState.vCPU.reserved
			totalNodeMem += podState.memSlots.reserved
		}
	}

	nodeTotalReservableCPU := node.totalReservableCPU()
	var status *framework.Status
	var resultString string

	var msg string
	setMsg := func(compareOp string) {
		msg = fmt.Sprintf(
			"proposed node vCPU usage %d + pod init vCPU %d %s node max %d",
			totalNodeVCPU, vmInfo.Cpu.Use, compareOp, nodeTotalReservableCPU,
		)
	}

	if totalNodeVCPU+vmInfo.Cpu.Use > nodeTotalReservableCPU {
		setMsg(">")
		status = framework.NewStatus(
			framework.Unschedulable,
			fmt.Sprintf("Not enough resources for pod: %s", msg),
		)
		resultString = "rejecting"
	} else {
		status = nil // explicitly setting this; nil is success.
		resultString = "allowing"
		setMsg("<=")
	}

	klog.Infof(
		"[autoscale-enforcer] Filter: %s pod %v in node %s: %s",
		resultString, pName, nodeName, msg,
	)

	return status
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
	ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeName string,
) (int64, *framework.Status) {
	scoreLen := framework.MaxNodeScore - framework.MinNodeScore

	podName := api.PodName{Name: pod.Name, Namespace: pod.Namespace}
	vmInfo, err := getVmInfo(ctx, e.vmClient, pod)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Score: Error getting info for pod %v: %w", podName, err)
		return 0, framework.NewStatus(framework.Error, "Error getting info for pod")
	}

	// note: vmInfo may be nil here if the pod does not correspond to a NeonVM virtual machine

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	// Double-check that the SchedulerName matches what we're expecting
	if status := e.checkSchedulerName(pod); status != nil {
		return framework.MinNodeScore, status
	}

	// Score by total resources available:
	node, err := e.state.getOrFetchNodeState(ctx, e.handle, nodeName)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Score: Error fetching state for node %s: %w", nodeName, err)
		return 0, framework.NewStatus(framework.Error, "Error fetching state for node")
	}

	// Special case: return minimum score if we don't have room
	noRoom := vmInfo != nil &&
		(vmInfo.Cpu.Use > node.remainingReservableCPU() ||
			vmInfo.Mem.Use > node.remainingReservableMemSlots())
	if noRoom {
		return framework.MinNodeScore, nil
	}

	totalCpu := int64(node.totalReservableCPU())
	totalMem := int64(node.totalReservableMemSlots())
	maxTotalCpu := int64(e.state.maxTotalReservableCPU)
	maxTotalMem := int64(e.state.maxTotalReservableMemSlots)

	// The ordering of multiplying before dividing is intentional; it allows us to get an exact
	// result, because scoreLen and total will both be small (i.e. their product fits within an int64)
	scoreCpu := framework.MinNodeScore + scoreLen*totalCpu/maxTotalCpu
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
	ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeName string,
) *framework.Status {
	pName := api.PodName{Name: pod.Name, Namespace: pod.Namespace}
	klog.Infof("[autoscale-enforcer] Reserve: Handling request for pod %v, node %s", pName, nodeName)

	vmInfo, err := getVmInfo(ctx, e.vmClient, pod)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Error getting pod %v info: %s", pName, err)
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("Error getting pod info: %s", err),
		)
	}

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	// Double-check that the SchedulerName matches what we're expecting
	if status := e.checkSchedulerName(pod); status != nil {
		return status
	}

	// Double-check that the VM's memory slot size still matches ours. This should be ensured by
	// our implementation of Filter, but this would be a *pain* to debug if it went wrong somehow.
	if vmInfo != nil && !vmInfo.Mem.SlotSize.Equal(e.state.conf.MemSlotSize) {
		err := fmt.Errorf(
			"expected %v, found %v (this should have been caught during Filter)",
			e.state.conf.MemSlotSize, vmInfo.Mem.SlotSize,
		)
		klog.Errorf("[autoscale-enforcer] Reserve: VM for pod %v has bad MemSlotSize: %w", pName, err)
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("VM for pod has bad MemSlotSize: %v", err),
		)
	}

	node, err := e.state.getOrFetchNodeState(ctx, e.handle, nodeName)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Error getting node %s state: %s", nodeName, err)
		return framework.NewStatus(
			framework.Error,
			fmt.Sprintf("Error getting node state: %s", err),
		)
	}

	// if this is a non-VM pod, use a different set of information for it.
	if vmInfo == nil {
		podResources, err := extractPodOtherPodResourceState(pod)
		if err != nil {
			klog.Errorf("[autoscale-enforcer] Error getting non-VM pod %v state: %v", pName, err)
			return framework.NewStatus(
				framework.UnschedulableAndUnresolvable,
				fmt.Sprintf("Error getting non-VM pod info: %v", err),
			)
		}

		oldNodeRes := node.otherResources
		newNodeRes := node.otherResources.addPod(&e.state.conf.MemSlotSize, podResources)

		addCpu := newNodeRes.reservedCpu - oldNodeRes.reservedCpu
		addMem := newNodeRes.reservedMemSlots - oldNodeRes.reservedMemSlots

		if addCpu <= node.remainingReservableCPU() && addMem <= node.remainingReservableMemSlots() {
			oldNodeCpuReserved := node.vCPU.reserved
			oldNodeMemReserved := node.memSlots.reserved

			node.otherResources = newNodeRes
			node.vCPU.reserved += addCpu
			node.memSlots.reserved += addMem

			ps := &otherPodState{
				name:      pName,
				node:      node,
				resources: podResources,
			}
			node.otherPods[pName] = ps
			e.state.otherPods[pName] = ps

			fmtString := "[autoscale-enforcer] Allowing non-VM pod %v (%v raw cpu, %v raw mem) in node %s:\n" +
				"\tvCPU: node reserved %d -> %d, node other resources %d -> %d rounded (%v -> %v raw)\n" +
				"\t mem: node reserved %d -> %d, node other resources %d -> %d slots (%v -> %v raw)"
			klog.Infof(
				fmtString,
				// allowing non-VM pod %v (%v raw cpu, %v raw mem) in node %s
				pName, &podResources.rawCpu, &podResources.rawMemory, nodeName,
				// vCPU: node reserved %d -> %d, node other resources %d -> %d rounded (%v -> %v raw)
				oldNodeCpuReserved, node.vCPU.reserved, oldNodeRes.reservedCpu, newNodeRes.reservedCpu, &oldNodeRes.rawCpu, &newNodeRes.rawCpu,
				// mem: node reserved %d -> %d, node other resources %d -> %d slots (%v -> %v raw)
				oldNodeMemReserved, node.memSlots.reserved, oldNodeRes.reservedMemSlots, newNodeRes.reservedMemSlots, &oldNodeRes.rawMemory, &newNodeRes.rawMemory,
			)

			return nil // nil is success
		} else {
			fmtString := "Not enough resources to reserve for non-VM pod: " +
				"need %d vCPU (%v -> %v raw), %d mem slots (%v -> %v raw) but " +
				"have %d vCPu, %d mem slots remaining"
			err := fmt.Errorf(
				fmtString,
				// need %d vCPU (%v -> %v raw), %d mem slots (%v -> %v raw) but
				addCpu, &oldNodeRes.rawCpu, &newNodeRes.rawCpu, addMem, &oldNodeRes.rawMemory, &newNodeRes.rawMemory,
				// have %d vCPU, %d mem slots remaining
				node.remainingReservableCPU(), node.remainingReservableMemSlots(),
			)
			klog.Errorf("[autoscale-enforcer] Can't schedule non-VM pod %v: %s", pName, err)
			return framework.NewStatus(framework.Unschedulable, err.Error())
		}
	}

	// Otherwise, it's a VM. Use the vmInfo
	//
	// If there's capacity to reserve the pod, do that. Otherwise, reject the pod. Most capacity
	// checks will be handled in the calls to Filter, but it's possible for another VM to scale up
	// in between the calls to Filter and Reserve, removing the resource availability that we
	// thought we had.
	if vmInfo.Cpu.Use <= node.remainingReservableCPU() && vmInfo.Mem.Use <= node.remainingReservableMemSlots() {
		newNodeReservedCPU := node.vCPU.reserved + vmInfo.Cpu.Use
		newNodeReservedMemSlots := node.memSlots.reserved + vmInfo.Mem.Use

		fmtString := "[autoscale-enforcer] Allowing VM pod %v (%d vCPU, %d mem slots) in node %s: " +
			"node.vCPU.reserved %d -> %d, node.memSlots.reserved %d -> %d"
		klog.Infof(
			fmtString,
			// allowing pod %v (%d vCpu, %d mem slots) in node %s
			pName, vmInfo.Cpu.Use, vmInfo.Mem.Use, nodeName,
			// node vcpu reserved %d -> %d, node mem slots reserved %d -> %d
			node.vCPU.reserved, newNodeReservedCPU, node.memSlots.reserved, newNodeReservedMemSlots,
		)

		node.vCPU.reserved = newNodeReservedCPU
		node.memSlots.reserved = newNodeReservedMemSlots
		ps := &podState{
			name:                     pName,
			vmName:                   vmInfo.Name,
			node:                     node,
			vCPU:                     podResourceState[uint16]{reserved: vmInfo.Cpu.Use, capacityPressure: 0},
			memSlots:                 podResourceState[uint16]{reserved: vmInfo.Mem.Use, capacityPressure: 0},
			testingOnlyAlwaysMigrate: vmInfo.AlwaysMigrate,
			mqIndex:                  -1,
		}
		node.pods[pName] = ps
		e.state.podMap[pName] = ps
		return nil // nil is success
	} else {
		fmtString := "Not enough resources to reserve for VM: " +
			"need %d vCPU, %d mem slots but " +
			"have %d vCPU, %d mem slots remaining"
		err := fmt.Errorf(
			fmtString,
			// need %d vCPU, %d mem slots
			vmInfo.Cpu.Use, vmInfo.Mem.Use,
			// have %d vCPU, %d mem slots
			node.remainingReservableCPU(), node.remainingReservableMemSlots(),
		)

		klog.Errorf("[autoscale-enforcer] Can't schedule VM pod %v: %s", pName, err)
		return framework.NewStatus(framework.Unschedulable, err.Error())
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
	ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeName string,
) {
	pName := api.PodName{Name: pod.Name, Namespace: pod.Namespace}
	klog.Infof("[autoscale-enforcer] Unreserve: Handling request for pod %v, node %s", pName, nodeName)

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	ps, ok := e.state.podMap[pName]
	otherPs, otherOk := e.state.otherPods[pName]
	// FIXME: we should guarantee that we can never have an entry in both maps with the same name.
	// This needs to be handled in Reserve, not here.
	if !ok && otherOk {
		vCPUVerdict, memVerdict := handleDeletedPod(otherPs.node, otherPs.resources, &e.state.conf.MemSlotSize)
		delete(e.state.otherPods, pName)
		delete(otherPs.node.otherPods, pName)

		fmtString := "[autoscale-enforcer] Unreserved non-VM pod from node %s:\n" +
			"\tvCPU verdict: %s\n" +
			"\t mem verdict: %s"
		klog.Infof(fmtString, otherPs.name, otherPs.node.name, vCPUVerdict, memVerdict)
	} else {
		klog.Warningf("[autoscale-enforcer] Unreserve: Cannot find pod %v in podMap or otherPods (this may be normal behavior)", pName)
		return
	}

	// Mark the resources as no longer reserved

	currentlyMigrating := false // Unreserve is never called on bound pods, so it can't be migrating.
	vCPUVerdict := collectResourceTransition(&ps.node.vCPU, &ps.vCPU).
		handleDeleted(currentlyMigrating)
	memVerdict := collectResourceTransition(&ps.node.memSlots, &ps.memSlots).
		handleDeleted(currentlyMigrating)

	// Delete our record of the pod
	delete(e.state.podMap, pName)
	delete(ps.node.pods, pName)
	ps.node.mq.removeIfPresent(ps)

	fmtString := "[autoscale-enforcer] Unreserved VM pod %v from node %s:\n" +
		"\tvCPU verdict: %s\n" +
		"\t mem verdict: %s"
	klog.Infof(fmtString, ps.name, ps.node.name, vCPUVerdict, memVerdict)
}
