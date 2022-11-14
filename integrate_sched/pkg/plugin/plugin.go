package plugin

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	klog "k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	scheme "k8s.io/client-go/kubernetes/scheme"
	rest "k8s.io/client-go/rest"

	virtclient "github.com/neondatabase/virtink/pkg/generated/clientset/versioned"
	virtapi "github.com/neondatabase/virtink/pkg/apis/virt/v1alpha1"

	"github.com/neondatabase/autoscaling/pkg/api"
)

const Name = "AutoscaleEnforcer"
const LabelVM = "virtink.io/vm.name"
const LabelInitVCPU = "autoscaler/init-vcpu"
const LabelTestingOnlyAlwaysMigrate = "autoscaler/testing-only-always-migrate"
const ConfigMapNamespace = "kube-system"
const ConfigMapName = "scheduler-plugin-config"
const ConfigMapKey = "autoscaler-enforcer-config.json"
const PluginConfigPath = "/etc/kubernetes/autoscale-scheduler-config/autoscale-enforcer-config.json"
const InitConfigMapTimeoutSeconds = 5

// AutoscaleEnforcer is the scheduler plugin to coordinate autoscaling
type AutoscaleEnforcer struct {
	handle framework.Handle
	virtClient *virtclient.Clientset
	state  pluginState
}

// NewAutoscaleEnforcerPlugin produces the initial AutoscaleEnforcer plugin to be used by the
// scheduler
func NewAutoscaleEnforcerPlugin(obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	// ^ obj can be used for taking in configuration. it's a bit tricky to figure out, and we don't
	// quite need it yet.
	klog.Info("[autoscale-enforcer] Initializing plugin")

	// create the virtink client
	virtapi.AddToScheme(scheme.Scheme)
	vmConfig := rest.CopyConfig(h.KubeConfig())
	// The handler's ContentType is not the default "application/json" (it's protobuf), so we need
	// to set it back to JSON because Virtink doesn't support protobuf.
	vmConfig.ContentType = "application/json"
	virtClient, err := virtclient.NewForConfig(vmConfig)
	if err != nil {
		return nil, fmt.Errorf("Error creating virtink client: %s", err)
	}

	p := AutoscaleEnforcer{
		handle: h,
		virtClient: virtClient,
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

	// Run this once, to check that it works ...
	if err := p.removeOldMigrations(context.Background()); err != nil {
		return nil, fmt.Errorf("Error performing initial removal of old migrations")
	}

	// ... and then start it running in a loop:
	go func() {
		ctx := context.Background()
		for {
			time.Sleep(5 * time.Second)

			if err := p.removeOldMigrations(ctx); err != nil {
				klog.Errorf("[autoscale-enforcer] Error removing old migrations: %s", err)
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

// Filter gives our plugin a chance to signal that a pod shouldn't be put onto a particular node
//
// Required for framework.FilterPlugin
func (e *AutoscaleEnforcer) Filter(
	ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeInfo *framework.NodeInfo,
) *framework.Status {
	nodeName := nodeInfo.Node().Name // TODO: nodes also have namespaces? are they used at all?

	pName := api.PodName{Name: pod.Name, Namespace: pod.Namespace}
	klog.Infof("[autoscale-enforcer] Filter: Handling request for pod %v, node %s", pName, nodeName)

	podInfo, err := getPodInfo(ctx, pod)
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
			totalNodeVCPU, podInfo.initVCPU, compareOp, nodeTotalReservableCPU,
		)
	}

	if totalNodeVCPU + podInfo.initVCPU > nodeTotalReservableCPU {
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

	podInfo, err := getPodInfo(ctx, pod)
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
	if podInfo.initVCPU <= node.remainingReservableCPU() {
		newNodeReservedCPU := node.vCPU.reserved + podInfo.initVCPU
		klog.Infof(
			"[autoscale-enforcer] Allowing pod %v (%d vCPU) in node %s: node.reservedCPU %d -> %d",
			pName, podInfo.initVCPU, nodeName, node.vCPU.reserved, newNodeReservedCPU,
		)

		node.vCPU.reserved = newNodeReservedCPU
		ps := &podState{
			name: pName,
			vmName: podInfo.vmName,
			node: node,
			vCPU: podResourceState[uint16]{reserved: podInfo.initVCPU, pressure: 0},
			testingOnlyAlwaysMigrate: podInfo.alwaysMigrate,
			mqIndex: -1,
		}
		node.pods[pName] = ps
		e.state.podMap[pName] = ps
		return nil // nil is success
	} else {
		err := fmt.Errorf(
			"Not enough available vCPU to reserve for pod init vCPU: need %d but have %d (of %d) remaining",
			podInfo.initVCPU, node.remainingReservableCPU(), node.totalReservableCPU(),
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
	oldPressure := ps.node.vCPU.pressure
	ps.node.vCPU.reserved -= ps.vCPU.reserved
	ps.node.vCPU.pressure -= ps.vCPU.pressure

	klog.Infof(
		"[autoscale-enforcer] Unreserved pod %v (%d vCPU) from node %s: node.vCPU.reserved %d -> %d, node.vCPU.pressure %d -> %d",
		pName, ps.vCPU.reserved, ps.node.name, oldReserved, ps.node.vCPU.reserved, oldPressure, ps.node.vCPU.pressure,
	)
}
