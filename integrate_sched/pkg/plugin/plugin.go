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
			nodeMap: make(map[string]*nodeState),
			podMap:  make(map[api.PodName]*podState),
			// zero values are ok for the rest here
		},
	}

	if err := p.setConfigAndStartWatcher(); err != nil {
		klog.Errorf("Error starting config watcher: %s", err)
		return nil, err
	}

	vmDeletions := make(chan api.PodName)
	if err := p.watchVMDeletions(context.Background(), vmDeletions); err != nil {
		return nil, fmt.Errorf("Error starting VM deletion watcher: %s", err)
	}

	go func() {
		for name := range vmDeletions {
			p.handleVMDeletion(name)
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
func getVmInfo(ctx context.Context, vmClient *vmclient.Clientset, pod *corev1.Pod) (*api.VmInfo, string, error) {
	vmName, ok := pod.Labels[LabelVM]
	if !ok {
		return nil, "", fmt.Errorf("Pod is not a VM (missing %s label)", LabelVM)
	}

	vm, err := vmClient.NeonvmV1().VirtualMachines(pod.Namespace).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("Error getting VM object %s:%s: %w", pod.Namespace, vmName, err)
	}

	vmInfo, err := api.ExtractVmInfo(vm)
	if err != nil {
		return nil, "", fmt.Errorf("Error extracting VM info: %w", err)
	}

	return vmInfo, vmName, nil
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

	vmInfo, _, err := getVmInfo(ctx, e.vmClient, pod)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Filter: Error getting pod %v init vCPU: %s", pName, err)
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("Error getting pod init vCPU: %s", err),
		)
	}

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	node, err := e.state.getOrFetchNodeState(ctx, e.handle, nodeName)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Filter: Error getting node %s state: %s", nodeName, err)
		return framework.NewStatus(
			framework.Error,
			fmt.Sprintf("Error getting node state: %s", err),
		)
	}

	// The pod will need podInitVCPU vCPUs reserved for it when it does get scheduled. Now we can
	// check whether this node has capacity for the pod.
	//
	// Technically speaking, the VM pods in nodeInfo might not match what we have recorded for the
	// node -- simply because during preemption, the scheduler tries to see whether it could
	// schedule the pod if other stuff was preempted, and gives us what the state WOULD be after
	// preemption.
	//
	// So we have to actually count up the resource usage of all pods in nodeInfo:
	totalNodeVCPU := uint16(0)
	for _, podInfo := range nodeInfo.Pods {
		pn := api.PodName{Name: podInfo.Pod.Name, Namespace: podInfo.Pod.Namespace}
		if podState, ok := e.state.podMap[pn]; ok {
			totalNodeVCPU += podState.vCPU.reserved
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
	vmInfo, _, err := getVmInfo(ctx, e.vmClient, pod)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Score: Error getting info for pod %v: %w", podName, err)
		return 0, framework.NewStatus(framework.Error, "Error getting info for pod")
	}

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	// Score by total resources available:
	node, err := e.state.getOrFetchNodeState(ctx, e.handle, nodeName)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Score: Error fetching state for node %s: %w", nodeName, err)
		return 0, framework.NewStatus(framework.Error, "Error fetching state for node")
	}

	// Special case: return minimum score if we don't have room
	if vmInfo.Cpu.Use > node.remainingReservableCPU() {
		return framework.MinNodeScore, nil
	}

	total := int64(node.totalReservableCPU())
	maxTotal := int64(e.state.maxTotalReservableCPU)

	// The ordering of multiplying before dividing is intentional; it allows us to get an exact
	// result, because scoreLen and total will both be small (i.e. their product fits within an int64)
	score := framework.MinNodeScore + scoreLen*total/maxTotal
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
	ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeName string,
) *framework.Status {
	pName := api.PodName{Name: pod.Name, Namespace: pod.Namespace}
	klog.Infof("[autoscale-enforcer] Reserve: Handling request for pod %v, node %s", pName, nodeName)

	vmInfo, vmName, err := getVmInfo(ctx, e.vmClient, pod)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Error getting pod %v info: %s", pName, err)
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("Error getting pod info: %s", err),
		)
	}

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	node, err := e.state.getOrFetchNodeState(ctx, e.handle, nodeName)
	if err != nil {
		klog.Errorf("[autoscale-enforcer] Error getting node %s state: %s", nodeName, err)
		return framework.NewStatus(
			framework.Error,
			fmt.Sprintf("Error getting node state: %s", err),
		)

	}

	// If there's capactiy to reserve the pod, do that. Otherwise, reject the pod. Most capacity
	// checks will be handled in the calls to Filter, but it's possible for another VM to scale up
	// in between the calls to Filter and Reserve, removing the resource availability that we
	// thought we had.
	if vmInfo.Cpu.Use <= node.remainingReservableCPU() {
		newNodeReservedCPU := node.vCPU.reserved + vmInfo.Cpu.Use
		klog.Infof(
			"[autoscale-enforcer] Allowing pod %v (%d vCPU) in node %s: node.reservedCPU %d -> %d",
			pName, vmInfo.Cpu.Use, nodeName, node.vCPU.reserved, newNodeReservedCPU,
		)

		node.vCPU.reserved = newNodeReservedCPU
		ps := &podState{
			name:                     pName,
			vmName:                   vmName,
			node:                     node,
			vCPU:                     podResourceState[uint16]{reserved: vmInfo.Cpu.Use, capacityPressure: 0},
			testingOnlyAlwaysMigrate: vmInfo.AlwaysMigrate,
			mqIndex:                  -1,
		}
		node.pods[pName] = ps
		e.state.podMap[pName] = ps
		return nil // nil is success
	} else {
		err := fmt.Errorf(
			"Not enough available vCPU to reserve for pod init vCPU: need %d but have %d (of %d) remaining",
			vmInfo.Cpu.Use, node.remainingReservableCPU(), node.totalReservableCPU(),
		)

		klog.Errorf("[autoscale-enforcer] Can't schedule pod %v: %s", pName, err)
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
	if !ok {
		klog.Warningf("[autoscale-enforcer] Unreserve: Cannot find pod %v in podMap (this may be normal behavior)", pName)
		return
	}

	// Mark the vCPU as no longer reserved and delete the pod
	delete(e.state.podMap, pName)
	delete(ps.node.pods, pName)
	ps.node.mq.removeIfPresent(ps)
	oldReserved := ps.node.vCPU.reserved
	oldPressure := ps.node.vCPU.capacityPressure
	ps.node.vCPU.reserved -= ps.vCPU.reserved
	ps.node.vCPU.capacityPressure -= ps.vCPU.capacityPressure

	klog.Infof(
		"[autoscale-enforcer] Unreserved pod %v (%d vCPU) from node %s: node.vCPU.reserved %d -> %d, node.vCPU.capacityPressure %d -> %d",
		pName, ps.vCPU.reserved, ps.node.name, oldReserved, ps.node.vCPU.reserved, oldPressure, ps.node.vCPU.capacityPressure,
	)
}
