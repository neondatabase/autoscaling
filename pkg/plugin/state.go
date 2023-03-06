package plugin

// Definitions and helper functions for managing plugin state

import (
	"context"
	"errors"
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// pluginState stores the private state for the plugin, used both within and outside of the
// predefined scheduler plugin points
//
// Accessing the individual fields MUST be done while holding a lock.
type pluginState struct {
	lock util.ChanMutex

	podMap  map[api.PodName]*podState
	nodeMap map[string]*nodeState

	// otherPods stores information about non-VM pods
	otherPods map[api.PodName]*otherPodState

	// maxTotalReservableCPU stores the maximum value of any node's totalReservableCPU(), so that we
	// can appropriately scale our scoring
	maxTotalReservableCPU uint16
	// maxTotalReservableMemSlots is the same as maxTotalReservableCPU, but for memory slots instead
	// of CPU
	maxTotalReservableMemSlots uint16
	// conf stores the current configuration, and is nil if the configuration has not yet been set
	//
	// Proper initialization of the plugin guarantees conf is not nil.
	conf *config
}

// nodeState is the information that we track for a particular
type nodeState struct {
	// name is the name of the node, guaranteed by kubernetes to be unique
	name string

	// vCPU tracks the state of vCPU resources -- what's available and how
	vCPU nodeResourceState[uint16]
	// memSlots tracks the state of memory slots -- what's available and how
	memSlots nodeResourceState[uint16]

	computeUnit *api.Resources

	// pods tracks all the VM pods assigned to this node
	//
	// This includes both bound pods (i.e., pods fully committed to the node) and reserved pods
	// (still may be unreserved)
	pods map[api.PodName]*podState

	// otherPods are the non-VM pods that we're also tracking in this node
	otherPods map[api.PodName]*otherPodState
	// otherResources is the sum resource usage associated with the non-VM pods
	otherResources nodeOtherResourceState

	// mq is the priority queue tracking which pods should be chosen first for migration
	mq migrationQueue
}

// nodeResourceState describes the state of a resource allocated to a node
type nodeResourceState[T any] struct {
	// Total is the Total amount of T available on the node. This value does not change.
	Total T `json:"total"`
	// System is the amount of T pre-reserved for system functions, and cannot be handed out to pods
	// on the node. This amount CAN change on config updates, which may result in more of T than
	// we'd like being already provided to the pods.
	//
	// This is equivalent to the value of this resource's resourceConfig.System, rounded up to the
	// nearest size of the units of T.
	System T `json:"system"`
	// Watermark is the amount of T reserved to pods above which we attempt to reduce usage via
	// migration.
	Watermark T `json:"watermark"`
	// Reserved is the current amount of T reserved to pods. It SHOULD be less than or equal to
	// (Total - System), and we take active measures reduce it once it is above watermark.
	//
	// Reserved MAY be greater than Total on scheduler restart (because of buffering with VM scaling
	// maximums), but (Reserved - Buffer) MUST be less than Total. In general, (Reserved - Buffer)
	// SHOULD be less than or equal to (Total - System), but this can be temporarily violated after
	// restart or config change.
	//
	// For more information, refer to the ARCHITECTURE.md file in this directory.
	//
	// Reserved is always exactly equal to the sum of all of this node's pods' Reserved T.
	Reserved T `json:"reserved"`
	// Buffer *mostly* matters during startup. It tracks the total amount of T that we don't
	// *expect* is currently in use, but is still reserved to the pods because we can't prevent the
	// autoscaler-agents from making use of it.
	//
	// Buffer is always exactly equal to the sum of all this node's pods' Buffer for T.
	Buffer T `json:"buffer"`
	// CapacityPressure is -- roughly speaking -- the amount of T that we're currently denying to
	// pods in this node when they request it, due to not having space in remainingReservableCPU().
	// This value is exactly equal to the sum of each pod's CapacityPressure.
	//
	// This value is used alongside the "logical pressure" (equal to Reserved - Watermark, if
	// nonzero) in tooMuchPressure() to determine if more pods should be migrated off the node to
	// free up pressure.
	CapacityPressure T `json:"capacityPressure"`
	// PressureAccountedFor gives the total pressure expected to be relieved by ongoing migrations.
	// This is equal to the sum of Reserved + CapacityPressure for all pods currently migrating.
	//
	// The value may be larger than CapacityPressure.
	PressureAccountedFor T `json:"pressureAccountedFor"`
}

// nodeOtherResourceState are total resources associated with the non-VM pods in a node
//
// The resources are basically broken up into two groups: the "raw" amounts (which have a finer
// resolution than what we track for VMs) and the "reserved" amounts. The reserved amounts are
// rounded up to the next unit that
type nodeOtherResourceState struct {
	RawCPU    resource.Quantity `json:"rawCPU"`
	RawMemory resource.Quantity `json:"rawMemory"`

	ReservedCPU      uint16 `json:"reservedCPU"`
	ReservedMemSlots uint16 `json:"reservedMemSlots"`

	// MarginCPU and MarginMemory track the amount of other resources we can get "for free" because
	// they were left out when rounding the System usage to fit in integer units of CPUs or memory
	// slots
	//
	// These values are both only changed by configuration changes.
	MarginCPU    *resource.Quantity `json:"marginCPU"`
	MarginMemory *resource.Quantity `json:"marginMemory"`
}

// podState is the information we track for an individual
type podState struct {
	// name is the namespace'd name of the pod
	//
	// name will not change after initialization, so it can be accessed without holding a lock.
	name api.PodName

	// vmName is the name of the VM, as given by the 'vm.neon.tech/name' label.
	vmName string

	// testingOnlyAlwaysMigrate is a test-only debugging flag that, if present in the pod's labels,
	// will always prompt it to mgirate, regardless of whether the VM actually *needs* to.
	testingOnlyAlwaysMigrate bool

	// node provides information about the node that this pod is bound to or reserved onto.
	node *nodeState
	// vCPU is the current state of this pod's vCPU utilization and pressure
	vCPU podResourceState[uint16]
	// memSlots is the current state of this pod's memory slot(s) utilization and pressure
	memSlots podResourceState[uint16]

	// mostRecentComputeUnit stores the "compute unit" that this pod's autoscaler-agent most
	// recently observed (and so, what future AgentRequests are expected to abide by)
	mostRecentComputeUnit *api.Resources

	// metrics is the most recent metrics update we received for this pod. A nil pointer means that
	// we have not yet received metrics.
	metrics *api.Metrics

	// mqIndex stores this pod's index in the migrationQueue. This value is -1 iff metrics is nil or
	// it is currently migrating.
	mqIndex int

	// migrationState gives current information about an ongoing migration, if this pod is currently
	// migrating.
	migrationState *podMigrationState
}

// podMigrationState tracks the information about an ongoing pod's migration
type podMigrationState struct{}

type podResourceState[T any] struct {
	// Reserved is the amount of T that this pod has reserved. It is guaranteed that the pod is
	// using AT MOST Reserved T.
	Reserved T `json:"reserved"`
	// Buffer is the amount of Reserved that we've included in reserved to account for the
	// possibility of unilateral increases by the autoscaler-agent
	//
	// This value is only nonzero during startup (between initial state load and first communication
	// from the autoscaler-agent), and MUST be less than or equal to reserved.
	//
	// After the first communication from the autoscaler-agent, we update Reserved to match its
	// value, and set Buffer to zero.
	Buffer T
	// CapacityPressure is this pod's contribution to this pod's node's CapacityPressure for this
	// resource
	CapacityPressure T
}

// otherPodState tracks a little bit of information for the non-VM pods we're handling
type otherPodState struct {
	name      api.PodName
	node      *nodeState
	resources podOtherResourceState
}

// podOtherResourceState is the resources tracked for a non-VM pod
//
// This is *like* nodeOtherResourceState, but we don't track reserved amounts because they only
// exist at the high-level "total resource usage" scope
type podOtherResourceState struct {
	RawCPU    resource.Quantity `json:"rawCPU"`
	RawMemory resource.Quantity `json:"rawMemory"`
}

// addPod is a convenience method that returns the new resource state if we were to add the given
// pod resources
//
// This is used both to determine if there's enough room for the pod *and* to keep around the
// before and after so that we can use it for logging.
func (r nodeOtherResourceState) addPod(
	memSlotSize *resource.Quantity,
	p podOtherResourceState,
) nodeOtherResourceState {
	newState := nodeOtherResourceState{
		RawCPU:       r.RawCPU.DeepCopy(),
		RawMemory:    r.RawMemory.DeepCopy(),
		MarginCPU:    r.MarginCPU,
		MarginMemory: r.MarginMemory,
		// reserved amounts set by calculateReserved()
		ReservedCPU:      0,
		ReservedMemSlots: 0,
	}

	newState.RawCPU.Add(p.RawCPU)
	newState.RawMemory.Add(p.RawMemory)

	newState.calculateReserved(memSlotSize)

	return newState
}

// subPod is a convenience method that returns the new resource state if we were to remove the given
// pod resources
//
// This *also* happens to be what we use for calculations when actually removing a pod, because it
// allows us to use both the before and after for logging.
func (r nodeOtherResourceState) subPod(
	memSlotSize *resource.Quantity,
	p podOtherResourceState,
) nodeOtherResourceState {
	// Check we aren't underflowing.
	//
	// We're more worried about underflow than overflow because it should *generally* be pretty
	// difficult to get overflow to occur (also because overflow would probably take a slow & steady
	// leak to trigger, which is less useful than underflow.
	if r.RawCPU.Cmp(p.RawCPU) == -1 {
		panic(fmt.Errorf(
			"underflow: cannot subtract %v pod CPU from %v node CPU",
			&p.RawCPU, &r.RawCPU,
		))
	} else if r.RawMemory.Cmp(p.RawMemory) == -1 {
		panic(fmt.Errorf(
			"underflow: cannot subtract %v pod memory from %v node memory",
			&p.RawMemory, &r.RawMemory,
		))
	}

	newState := nodeOtherResourceState{
		RawCPU:       r.RawCPU.DeepCopy(),
		RawMemory:    r.RawMemory.DeepCopy(),
		MarginCPU:    r.MarginCPU,
		MarginMemory: r.MarginMemory,
		// reserved amounts set by calculateReserved()
		ReservedCPU:      0,
		ReservedMemSlots: 0,
	}

	newState.RawCPU.Sub(p.RawCPU)
	newState.RawMemory.Sub(p.RawMemory)

	newState.calculateReserved(memSlotSize)

	return newState
}

// calculateReserved sets the values of r.reservedCpu and r.reservedMemSlots based on the current
// "raw" resource amounts and the memory slot size
func (r *nodeOtherResourceState) calculateReserved(memSlotSize *resource.Quantity) {
	// If rawCpu doesn't exceed the margin we have from rounding up System, set reserved = 0
	if r.RawCPU.Cmp(*r.MarginCPU) <= 0 {
		r.ReservedCPU = 0
	} else {
		// set cupCopy := r.rawCpu - r.marginCpu
		cpuCopy := r.RawCPU.DeepCopy()
		cpuCopy.Sub(*r.MarginCPU)
		// note: Value() rounds up, which is the behavior we want here.
		r.ReservedCPU = uint16(cpuCopy.Value())
	}

	// If rawMemory doesn't exceed the margin ..., set reserved = 0
	if r.RawMemory.Cmp(*r.MarginMemory) <= 0 {
		r.ReservedMemSlots = 0
	} else {
		// set memoryCopy := r.rawMemory - r.marginMemory
		memoryCopy := r.RawMemory.DeepCopy()
		memoryCopy.Sub(*r.MarginMemory)

		memSlotSizeExact := memSlotSize.Value()
		// note: For integer arithmetic, (x + n-1) / n is equivalent to ceil(x/n)
		newReservedMemSlots := (memoryCopy.Value() + memSlotSizeExact - 1) / memSlotSizeExact
		if newReservedMemSlots > math.MaxUint16 {
			panic(fmt.Errorf(
				"new reserved mem slots overflows uint16 (%d > %d)", newReservedMemSlots, math.MaxUint16,
			))
		}
		r.ReservedMemSlots = uint16(newReservedMemSlots)
	}
}

// totalReservableCPU returns the amount of node CPU that may be allocated to VM pods -- i.e.,
// excluding the CPU pre-reserved for system tasks.
func (s *nodeState) totalReservableCPU() uint16 {
	return s.vCPU.Total - s.vCPU.System
}

// totalReservableMemSlots returns the number of memory slots that may be allocated to VM pods --
// i.e., excluding the memory pre-reserved for system tasks.
func (s *nodeState) totalReservableMemSlots() uint16 {
	return s.memSlots.Total - s.memSlots.System
}

// remainingReservableCPU returns the remaining CPU that can be allocated to VM pods
func (s *nodeState) remainingReservableCPU() uint16 {
	return s.totalReservableCPU() - s.vCPU.Reserved
}

// remainingReservableMemSlots returns the remaining number of memory slots that can be allocated to
// VM pods
func (s *nodeState) remainingReservableMemSlots() uint16 {
	return s.totalReservableMemSlots() - s.memSlots.Reserved
}

// tooMuchPressure is used to signal whether the node should start migrating pods out in order to
// relieve some of the pressure
func (s *nodeState) tooMuchPressure() bool {
	if s.vCPU.Reserved <= s.vCPU.Watermark && s.memSlots.Reserved < s.memSlots.Watermark {
		klog.V(1).Infof(
			"[autoscale-enforcer] tooMuchPressure(%s) = false (vCPU: reserved %d < watermark %d, mem: reserved %d < watermark %d)",
			s.name, s.vCPU.Reserved, s.vCPU.Watermark, s.memSlots.Reserved, s.memSlots.Watermark,
		)
		return false
	}

	logicalCpuPressure := util.SaturatingSub(s.vCPU.Reserved, s.vCPU.Watermark)
	logicalMemPressure := util.SaturatingSub(s.memSlots.Reserved, s.memSlots.Watermark)

	// Account for existing slack in the system, to counteract capacityPressure that hasn't been
	// updated yet
	logicalCpuSlack := s.vCPU.Buffer + util.SaturatingSub(s.vCPU.Watermark, s.vCPU.Reserved)
	logicalMemSlack := s.memSlots.Buffer + util.SaturatingSub(s.memSlots.Watermark, s.memSlots.Reserved)

	tooMuchCpu := logicalCpuPressure+s.vCPU.CapacityPressure > s.vCPU.PressureAccountedFor+logicalCpuSlack
	tooMuchMem := logicalMemPressure+s.memSlots.CapacityPressure > s.memSlots.PressureAccountedFor+logicalMemSlack

	result := tooMuchCpu || tooMuchMem

	fmtString := "[autoscale-enforcer] tooMuchPressure(%s) = %v. " +
		"vCPU: {logical: %d (slack: %d), capacity: %d, accountedFor: %d}, " +
		"mem: {logical: %d (slack: %d), capacity: %d, accountedFor: %d}"

	klog.V(1).Infof(
		fmtString,
		// tooMuchPressure(%s) = %v
		s.name, result,
		// vCPU: {logical: %d (slack: %d), capacity: %d, accountedFor: %d}
		logicalCpuPressure, logicalCpuSlack, s.vCPU.CapacityPressure, s.vCPU.PressureAccountedFor,
		// mem: {logical: %d, (slack: %d), capacity: %d, accountedFor: %d}
		logicalMemPressure, logicalMemSlack, s.memSlots.CapacityPressure, s.memSlots.PressureAccountedFor,
	)

	return result
}

// checkOkToMigrate allows us to check that it's still ok to start migrating a pod, after it was
// previously selected for migration
//
// A returned error indicates that the pod's resource usage has changed enough that we should try to
// migrate something else first. The error provides justification for this.
func (s *podState) checkOkToMigrate(oldMetrics api.Metrics) error {
	// TODO. Note: s.metrics may be nil.
	return nil
}

func (s *podState) currentlyMigrating() bool {
	return s.migrationState != nil
}

// this method can only be called while holding a lock. If we don't have the necessary information
// locally, then the lock is released temporarily while we query the API server
//
// A lock will ALWAYS be held on return from this function.
func (s *pluginState) getOrFetchNodeState(
	ctx context.Context,
	handle framework.Handle,
	nodeName string,
) (*nodeState, error) {
	if n, ok := s.nodeMap[nodeName]; ok {
		klog.V(1).Infof("[autoscale-enforcer] Using stored information for node %s", nodeName)
		return n, nil
	}

	// Fetch from the API server. Log is not V(1) because its context may be valuable.
	klog.Infof(
		"[autoscale-enforcer] No local information for node %s, fetching from API server", nodeName,
	)
	s.lock.Unlock() // Unlock to let other goroutines progress while we get the data we need

	var locked bool // In order to prevent double-unlock panics, we always lock on return.
	defer func() {
		if !locked {
			s.lock.Lock()
		}
	}()

	node, err := handle.ClientSet().CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("Error querying node information: %w", err)
	}

	// Re-lock and process API result
	locked = true
	s.lock.Lock()

	// It's possible that the node was already added. Don't double-process nodes if we don't have
	// to.
	if n, ok := s.nodeMap[nodeName]; ok {
		klog.Infof(
			"[autoscale-enforcer] Local information for node %s became available during API call, using it",
			nodeName,
		)
		return n, nil
	}

	n, err := buildInitialNodeState(node, s.conf)
	if err != nil {
		return nil, err
	}

	// update maxTotalReservableCPU and maxTotalReservableMemSlots if there's new maxima
	totalReservableCPU := n.totalReservableCPU()
	if totalReservableCPU > s.maxTotalReservableCPU {
		s.maxTotalReservableCPU = totalReservableCPU
	}
	totalReservableMemSlots := n.totalReservableMemSlots()
	if totalReservableMemSlots > s.maxTotalReservableMemSlots {
		s.maxTotalReservableMemSlots = totalReservableMemSlots
	}

	s.nodeMap[nodeName] = n
	return n, nil
}

// this method must only be called while holding s.lock. It will not be released during this
// function.
//
// Note: buildInitialNodeState does not take any of the pods or VMs on the node into account; it
// only examines the total resources available to the node.
func buildInitialNodeState(node *corev1.Node, conf *config) (*nodeState, error) {
	// Fetch this upfront, because we'll need it a couple times later.
	nodeConf := conf.forNode(node.Name)

	// cpuQ = "cpu, as a K8s resource.Quantity"
	// -A for allocatable, -C for capacity
	var cpuQ *resource.Quantity
	cpuQA := node.Status.Allocatable.Cpu()
	cpuQC := node.Status.Capacity.Cpu()

	if cpuQA != nil {
		// Use Allocatable by default ...
		cpuQ = cpuQA
	} else if cpuQC != nil {
		// ... but use Capacity if Allocatable is not available
		cpuQ = cpuQC
	} else {
		return nil, errors.New("Node has no Allocatable or Capacity CPU limits")
	}

	vCPU, marginCpu, err := nodeConf.vCpuLimits(cpuQ)
	if err != nil {
		return nil, fmt.Errorf("Error calculating vCPU limits for node %s: %w", node.Name, err)
	}

	// memQ = "mem, as a K8s resource.Quantity"
	// -A for allocatable, -C for capacity
	var memQ *resource.Quantity
	memQA := node.Status.Allocatable.Memory()
	memQC := node.Status.Capacity.Memory()

	if memQA != nil {
		memQ = memQA
	} else if memQC != nil {
		memQ = memQC
	} else {
		return nil, errors.New("Node has no Allocatable or Capacity Memory limits")
	}

	memSlots, marginMemory, err := nodeConf.memoryLimits(memQ, &conf.MemSlotSize)
	if err != nil {
		return nil, fmt.Errorf("Error calculating memory slot limits for node %s: %w", node.Name, err)
	}

	n := &nodeState{
		name:      node.Name,
		vCPU:      vCPU,
		memSlots:  memSlots,
		pods:      make(map[api.PodName]*podState),
		otherPods: make(map[api.PodName]*otherPodState),
		otherResources: nodeOtherResourceState{
			RawCPU:           resource.Quantity{},
			RawMemory:        resource.Quantity{},
			ReservedCPU:      0,
			ReservedMemSlots: 0,
			MarginCPU:        marginCpu,
			MarginMemory:     marginMemory,
		},
		computeUnit: &nodeConf.ComputeUnit,
		mq:          migrationQueue{},
	}

	fmtString := "[autoscale-enforcer] Built initial node state for %s:\n" +
		"\tCPU:    total = %d (milli = %d, margin = %v), max reservable = %d, watermark = %d\n" +
		"\tMemory: total = %d slots (raw = %v, margin = %v), max reservable = %d, watermark = %d"

	klog.Infof(
		fmtString,
		// fetched node %s
		node.Name,
		// cpu: total = %d (milli = %d, margin = %v), max reservable = %d, watermark = %d
		n.vCPU.Total, cpuQ.MilliValue(), n.otherResources.MarginCPU, n.totalReservableCPU(), n.vCPU.Watermark,
		// mem: total = %d (raw = %v, margin = %v), max reservable = %d, watermark = %d
		n.memSlots.Total, memQ, n.otherResources.MarginMemory, n.totalReservableMemSlots(), n.memSlots.Watermark,
	)

	return n, nil
}

func extractPodOtherPodResourceState(pod *corev1.Pod) (podOtherResourceState, error) {
	var cpu resource.Quantity
	var mem resource.Quantity

	for i, container := range pod.Spec.Containers {
		// For each resource, use requests if it's provided, or fallback on the limit.

		cpuRequest := container.Resources.Requests.Cpu()
		cpuLimit := container.Resources.Limits.Cpu()
		if cpuRequest.IsZero() && cpuLimit.IsZero() {
			err := fmt.Errorf("containers[%d] (%q) missing resources.requests.cpu AND resources.limits.cpu", i, container.Name)
			return podOtherResourceState{}, err
		} else if cpuRequest.IsZero() /* && !cpuLimit.IsZero() */ {
			cpuRequest = cpuLimit
		}
		cpu.Add(*cpuRequest)

		memRequest := container.Resources.Requests.Memory()
		memLimit := container.Resources.Limits.Memory()
		if memRequest.IsZero() && memLimit.IsZero() {
			err := fmt.Errorf("containers[%d] (%q) missing resources.limits.memory", i, container.Name)
			return podOtherResourceState{}, err
		} else if memRequest.IsZero() /* && !memLimit.IsZero() */ {
			memRequest = memLimit
		}
		mem.Add(*memRequest)
	}

	return podOtherResourceState{RawCPU: cpu, RawMemory: mem}, nil
}

// This method is /basically/ the same as e.Unreserve, but the API is different and it has different
// logs, so IMO it's worthwhile to have this separate.
func (e *AutoscaleEnforcer) handleVMDeletion(podName api.PodName) {
	klog.Infof("[autoscale-enforcer] Handling deletion of VM pod %v", podName)

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.podMap[podName]
	if !ok {
		klog.Warningf("[autoscale-enforcer] delete VM pod: Cannot find pod %v in podMap", podName)
		return
	}

	// Mark the resources as no longer reserved
	currentlyMigrating := pod.currentlyMigrating()

	vCPUVerdict := collectResourceTransition(&pod.node.vCPU, &pod.vCPU).
		handleDeleted(currentlyMigrating)
	memVerdict := collectResourceTransition(&pod.node.memSlots, &pod.memSlots).
		handleDeleted(currentlyMigrating)

	// Delete our record of the pod
	delete(e.state.podMap, podName)
	delete(pod.node.pods, podName)
	pod.node.mq.removeIfPresent(pod)

	var migrating string
	if currentlyMigrating {
		migrating = " migrating"
	}

	fmtString := "[autoscale-enforcer] Deleted%s VM pod %v from node %s:\n" +
		"\tvCPU verdict: %s\n" +
		"\t mem verdict: %s"
	klog.Infof(fmtString, migrating, pod.name, pod.node.name, vCPUVerdict, memVerdict)
}

func (e *AutoscaleEnforcer) handlePodDeletion(podName api.PodName) {
	klog.Infof("[autoscale-enforcer] Handling deletion of non-VM pod %v", podName)

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.otherPods[podName]
	if !ok {
		klog.Warningf("[autoscale-enforcer] delete non-VM pod: Cannot find pod %v in otherPods", podName)
		return
	}

	// Mark the resources as no longer reserved
	cpuVerdict, memVerdict := handleDeletedPod(pod.node, pod.resources, &e.state.conf.MemSlotSize)

	delete(e.state.otherPods, podName)
	delete(pod.node.otherPods, podName)

	fmtString := "[autoscale-enforcer] Deleted non-VM pod %v from node %s:\n" +
		"\tvCPU verdict: %s\n" +
		"\t mem verdict: %s"
	klog.Infof(fmtString, podName, pod.node.name, cpuVerdict, memVerdict)
}

func (s *podState) isBetterMigrationTarget(other *podState) bool {
	// TODO: this deprioritizes VMs whose metrics we can't collect. Maybe we don't want that?
	if s.metrics == nil || other.metrics == nil {
		return s.metrics != nil && other.metrics == nil
	}

	// TODO - this is just a first-pass approximation. Maybe it's ok for now? Maybe it's not. Idk.
	return s.metrics.LoadAverage1Min < other.metrics.LoadAverage1Min
}

// this method can only be called while holding a lock. It will be released temporarily while we
// send requests to the API server
//
// A lock will ALWAYS be held on return from this function.
func (s *pluginState) startMigration(ctx context.Context, pod *podState, vmClient *vmclient.Clientset) error {
	if pod.currentlyMigrating() {
		return fmt.Errorf("Pod is already migrating: state = %+v", pod.migrationState)
	}

	// Remove the pod from the migration queue.
	pod.node.mq.removeIfPresent(pod)
	// Mark the pod as migrating
	pod.migrationState = &podMigrationState{}
	// Update resource trackers
	oldNodeVCPUPressure := pod.node.vCPU.CapacityPressure
	oldNodeVCPUPressureAccountedFor := pod.node.vCPU.PressureAccountedFor
	pod.node.vCPU.PressureAccountedFor += pod.vCPU.Reserved + pod.vCPU.CapacityPressure

	klog.Infof(
		"[autoscale-enforcer] Migrate pod %v; node.vCPU.capacityPressure %d -> %d (%d -> %d spoken for)",
		pod.name, oldNodeVCPUPressure, pod.node.vCPU.CapacityPressure, oldNodeVCPUPressureAccountedFor, pod.node.vCPU.PressureAccountedFor,
	)

	// note: unimplemented for now, pending NeonVM implementation.
	return errors.New("VM migration is currently unimplemented")
}

// readClusterState sets the initial node and pod maps for the plugin's state, getting its
// information from the K8s cluster
//
// This method expects that all pluginState fields except pluginState.conf are set to their zero
// value, and will panic otherwise. Accordingly, this is only called during plugin initialization.
func (p *AutoscaleEnforcer) readClusterState(ctx context.Context) error {
	p.state.lock.Lock()
	defer p.state.lock.Unlock()

	// Check that all fields are equal to their zero value, per the function documentation.
	hasNonNilField := p.state.nodeMap != nil || p.state.podMap != nil || p.state.otherPods != nil ||
		p.state.maxTotalReservableCPU != 0 || p.state.maxTotalReservableMemSlots != 0

	if hasNonNilField {
		panic(errors.New("readClusterState called with non-nil pluginState field"))
	}
	if p.state.conf == nil {
		panic(errors.New("readClusterState called with nil config"))
	}

	// There's a couple challenges we have to deal with here, particularly around discrepancies in
	// the list of pods vs VMs.
	//
	// Our solution to this problem is _mostly_ to prefer going from VMs -> pods (i.e. "get pod info
	// related to VM") rather than the other way around, and going through the non-VM pods
	// separately. If a VM says it belongs to a pod that doesn't exist, we ignore the error and move
	// on. Because of this, it's better to fetch the VMs first and then pods, so that any VM that
	// was present at the start and still running has its pod visible to us. VMs that are started
	// between the VM listing and pod listing won't be handled, but that's ok because we are
	// probably about to schedule them anyways.

	klog.Infof("[autoscale-enforcer] load state: Listing VMs")
	vms, err := p.vmClient.NeonvmV1().VirtualMachines(corev1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("Error listing VirtualMachines: %w", err)
	}

	klog.Infof("[autoscale-enforcer] load state: Listing Pods")
	pods, err := p.handle.ClientSet().CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("Error listing Pods: %w", err)
	}

	klog.Infof("[autoscale-enforcer] load state: Listing Nodes")
	nodes, err := p.handle.ClientSet().CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("Error listing nodes: %w", err)
	}

	p.state.nodeMap = make(map[string]*nodeState)
	p.state.podMap = make(map[api.PodName]*podState)
	p.state.otherPods = make(map[api.PodName]*otherPodState)

	// Build the node map
	klog.Infof("[autoscale-enforcer] load state: Building node map")
	for i := range nodes.Items {
		n := &nodes.Items[i]
		node, err := buildInitialNodeState(n, p.state.conf)
		if err != nil {
			return fmt.Errorf("Error building state for node %s: %w", n.Name, err)
		}

		trCpu := node.totalReservableCPU()
		trMem := node.totalReservableMemSlots()
		if trCpu > p.state.maxTotalReservableCPU {
			p.state.maxTotalReservableCPU = trCpu
		}
		if trMem > p.state.maxTotalReservableMemSlots {
			p.state.maxTotalReservableMemSlots = trMem
		}

		p.state.nodeMap[n.Name] = node
	}

	// Store the PodSpecs by name, so we can access them as we're going through VMs
	klog.Infof("[autoscale-enforcer] load state: Building initial PodSpecs map")
	podSpecs := make(map[api.PodName]*corev1.Pod)
	for i := range pods.Items {
		p := &pods.Items[i]
		name := api.PodName{Name: p.Name, Namespace: p.Namespace}
		podSpecs[name] = p
	}

	// Add all VM pods to the map, by going through the VM list.
	klog.Infof("[autoscale-enforcer] load state: Adding VM pods to podMap")
	skippedVms := 0
	for i := range vms.Items {
		vm := &vms.Items[i]
		vmName := api.PodName{Name: vm.Name, Namespace: vm.Namespace}
		if vm.Spec.SchedulerName != p.state.conf.SchedulerName {
			klog.Infof(
				"[autoscale-enforcer] load state: Skipping VM %v, Spec.SchedulerName %q != our config.SchedulerName %q",
				vmName, vm.Spec.SchedulerName, p.state.conf.SchedulerName,
			)
			skippedVms += 1
			continue
		} else if vm.Status.PodName == "" {
			klog.Warningf(
				"[autoscale-enforcer] load state: Skipping VM %v due to Status.PodName = \"\" (maybe it hasn't been assigned yet?)",
				vmName,
			)
			skippedVms += 1
			continue
		}

		podName := api.PodName{Name: vm.Status.PodName, Namespace: vm.Namespace}
		pod, ok := podSpecs[podName]
		if !ok {
			klog.Warningf(
				"[autoscale-enforcer] load state: Skipping VM %v because pod %v is missing (maybe it was removed?)",
				vmName, podName,
			)
			skippedVms += 1
			continue
		} else if podsVm, exist := pod.Labels[LabelVM]; !exist || podsVm != vm.Name {
			if ok {
				return fmt.Errorf("Pod %v label %q doesn't match VM name %v", podName, LabelVM, vmName)
			} else {
				return fmt.Errorf("Pod %v missing label %q for VM %v", podName, LabelVM, vmName)
			}
		} else if pod.Spec.NodeName == "" {
			klog.Warningf(
				"[autoscale-enforcer] load state: Skipping VM %v because pod %v Spec.NodeName = \"\" (maybe it hasn't been scheduled yet?)",
				vmName, podName,
			)
			skippedVms += 1
			continue
		}

		vmInfo, err := api.ExtractVmInfo(vm)
		if err != nil {
			return fmt.Errorf("Error extracting VM info for %v: %w", vmName, err)
		}

		// Check that the memory slot size matches.
		if !vmInfo.Mem.SlotSize.Equal(p.state.conf.MemSlotSize) {
			return fmt.Errorf(
				"VM %v memory slot size (%v) doesn't match conf slot size (%v)",
				vmName, &vmInfo.Mem.SlotSize, &p.state.conf.MemSlotSize,
			)
		}

		ns, ok := p.state.nodeMap[pod.Spec.NodeName]
		if !ok {
			klog.Warningf(
				"[autoscale-enforcer] load state: Skipping VM %v because pod %v is on missing node %s, which may have been removed between listing Pods and Nodes",
				vmName, podName, pod.Spec.NodeName,
			)
			skippedVms += 1
			continue
		}

		// Build the pod state, update the node
		ps := &podState{
			name:   podName,
			vmName: vm.Name,
			node:   ns,
			vCPU: podResourceState[uint16]{
				Reserved:         vmInfo.Cpu.Max,
				Buffer:           vmInfo.Cpu.Max - vmInfo.Cpu.Use,
				CapacityPressure: 0,
			},
			memSlots: podResourceState[uint16]{
				Reserved:         vmInfo.Mem.Max,
				Buffer:           vmInfo.Mem.Max - vmInfo.Mem.Use,
				CapacityPressure: 0,
			},

			mqIndex:               -1,
			metrics:               nil,
			mostRecentComputeUnit: nil,
			migrationState:        nil,

			testingOnlyAlwaysMigrate: vmInfo.AlwaysMigrate,
		}
		oldNodeVCPUReserved := ns.vCPU.Reserved
		oldNodeMemReserved := ns.memSlots.Reserved
		oldNodeVCPUBuffer := ns.vCPU.Buffer
		oldNodeMemBuffer := ns.memSlots.Buffer

		ns.vCPU.Reserved += ps.vCPU.Reserved
		ns.memSlots.Reserved += ps.memSlots.Reserved
		fmtString := "[autoscale-enforcer] load state: Adding VM pod %v to node %s:\n" +
			"\tpod CPU = %d/%d (node %d -> %d / %d, %d -> %d buffer)\n" +
			"\tmem slots = %d/%d (node %d -> %d / %d, %d -> %d buffer)"
		klog.Infof(
			fmtString,
			// Adding VM pod %v to node %s
			podName, ns.name,
			// pod CPU = %d/%d (node %d -> %d / %d, %d -> %d buffer)
			ps.vCPU.Reserved, vmInfo.Cpu.Max, oldNodeVCPUReserved, ns.vCPU.Reserved, ns.totalReservableCPU(), oldNodeVCPUBuffer, ns.vCPU.Buffer,
			// mem slots = %d/%d (node %d -> %d / %d, %d -> %d buffer)
			ps.memSlots.Reserved, vmInfo.Mem.Max, oldNodeMemReserved, ns.memSlots.Reserved, ns.totalReservableMemSlots(), oldNodeMemBuffer, ns.memSlots.Buffer,
		)
		ns.pods[podName] = ps
		p.state.podMap[podName] = ps
	}

	// Add the non-VM pods to the map
	klog.Info("[autoscale-enforcer] load state: Adding non-VM pods to otherPods map")
	skippedOtherPods := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		podName := api.PodName{Name: pod.Name, Namespace: pod.Namespace}
		if pod.Spec.SchedulerName != p.state.conf.SchedulerName {
			klog.Infof(
				"[autoscale-enforcer] load state: Skipping non-VM pod %v, Spec.SchedulerName %q != our config.SchedulerName %q",
				podName, pod.Spec.SchedulerName, p.state.conf.SchedulerName,
			)
			skippedOtherPods += 1
			continue
		} else if _, ok := p.state.podMap[podName]; ok {
			continue
		} else if _, ok := pod.Labels[LabelVM]; ok {
			klog.Warningf(
				"[autoscale-enforcer] load state: Pod %v has label %q but isn't already processed (maybe the VM was created between listing VMs and Pods?)",
				podName, LabelVM,
			)
			skippedOtherPods += 1
			continue
		}

		if pod.Spec.NodeName == "" {
			klog.Warningf(
				"[autoscale-enforcer] load state: Skipping non-VM pod %v, Spec.NodeName = \"\" (maybe it hasn't been scheduled yet?)",
				podName,
			)
			skippedOtherPods += 1
			continue
		}

		ns, ok := p.state.nodeMap[pod.Spec.NodeName]
		if !ok {
			klog.Warningf(
				"[autoscale-enforcer] load state: Skipping non-VM pod %v on missing node %s, which may have been removed between listing Pods and Nodes",
				podName, pod.Spec.NodeName,
			)
			skippedOtherPods += 1
			continue
		}

		// TODO: this is largely duplicated from Reserve, so we should deduplicate it (probably into
		// trans.go or something).
		podRes, err := extractPodOtherPodResourceState(pod)
		if err != nil {
			return fmt.Errorf("Error extracting resource state from non-VM pod %v: %w", podName, err)
		}

		oldNodeRes := ns.otherResources
		newNodeRes := ns.otherResources.addPod(&p.state.conf.MemSlotSize, podRes)

		addCpu := newNodeRes.ReservedCPU - oldNodeRes.ReservedCPU
		addMem := newNodeRes.ReservedMemSlots - oldNodeRes.ReservedMemSlots

		oldNodeCpuReserved := ns.vCPU.Reserved
		oldNodeMemReserved := ns.memSlots.Reserved

		ns.otherResources = newNodeRes
		ns.vCPU.Reserved += addCpu
		ns.memSlots.Reserved += addMem

		ps := &otherPodState{
			name:      podName,
			node:      ns,
			resources: podRes,
		}
		fmtString := "[autoscale-enforcer] load state: Adding non-VM pod %v to node %s; " +
			"pod CPU = %v (node %d [%v raw] -> %d [%v raw]), " +
			"mem = %v (node %d slots [%v raw] -> %d [%v raw]"
		klog.Infof(
			fmtString,
			// Adding non-VM pod %v to node %s
			podName, pod.Spec.NodeName,
			// pod CPU = %v (node %d [%v raw] -> %d [%v raw])
			&podRes.RawCPU, oldNodeCpuReserved, &oldNodeRes.RawCPU, ns.vCPU.Reserved, &newNodeRes.RawCPU,
			// mem = %v (node %d slots [%v raw] -> %d [%v raw])
			&podRes.RawMemory, oldNodeMemReserved, &oldNodeRes.RawMemory, ns.memSlots.Reserved, &newNodeRes.RawMemory,
		)
		ns.otherPods[podName] = ps
		p.state.otherPods[podName] = ps
	}

	// Human-visible sanity checks on item counts:
	klog.Infof(
		"[autoscale-enforcer] Done loading state, found: %d nodes, %d VMs (%d skipped), %d non-VM pods (%d skipped)",
		len(p.state.nodeMap), len(p.state.podMap), skippedVms, len(p.state.otherPods), skippedOtherPods,
	)

	// At this point, everything's been added to the state. We just need to make sure that we're not
	// over-budget on anything:
	badNodes := make(map[string]string)
	for nodeName := range p.state.nodeMap {
		ns := p.state.nodeMap[nodeName]
		overBudget := []string{}

		if ns.vCPU.Reserved-ns.vCPU.Buffer > ns.vCPU.Total {
			overBudget = append(overBudget, fmt.Sprintf(
				"expected CPU usage (reserved %d - buffer %d) > total %d",
				ns.vCPU.Reserved, ns.vCPU.Buffer, ns.vCPU.Total,
			))
		}
		if ns.memSlots.Reserved > ns.memSlots.Total {
			overBudget = append(overBudget, fmt.Sprintf(
				"expected memSlots usage (reserved %d - buffer %d) > total %d",
				ns.memSlots.Reserved, ns.memSlots.Buffer, ns.memSlots.Total,
			))
		}

		if len(overBudget) == 1 {
			badNodes[nodeName] = overBudget[0]
		} else if len(overBudget) == 2 {
			badNodes[nodeName] = fmt.Sprintf("%s and %s", overBudget[0], overBudget[1])
		}
	}

	if len(badNodes) != 0 {
		errDesc := "[autoscale-enforcer] load state: The following Nodes' reserved resources were over budget:"
		for nodeName := range badNodes {
			errDesc += fmt.Sprintf("\n\t%s: %s", nodeName, badNodes[nodeName])
		}
		klog.Error(errDesc)
		return errors.New("Some nodes were over-budget (see logs for a list and reasons)")
	}

	return nil
}

func (s *pluginState) handleUpdatedConf() {
	// It's possible for this method to be called before readClusterState, in which case there's
	// nothing for us to do. We don't want to access nil data if that happens though, so we
	// special-case it:
	if s.podMap == nil {
		return
	}

	klog.Warningf("Ignoring updated configuration, runtime config updates are unimplemented")
}
