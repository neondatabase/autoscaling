package plugin

// Definitions and helper functions for managing plugin state

import (
	"context"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
)

// pluginState stores the private state for the plugin, used both within and outside of the
// predefined scheduler plugin points
//
// Accessing the individual fields MUST be done while holding a lock.
type pluginState struct {
	lock sync.Mutex

	podMap  map[api.PodName]*podState
	nodeMap map[string]*nodeState

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

	// mq is the priority queue tracking which pods should be chosen first for migration
	mq migrationQueue
}

// nodeResourceState describes the state of a resource allocated to a node
type nodeResourceState[T any] struct {
	// total is the total amount of T available on the node. This value does not change.
	total T
	// system is the amount of T pre-reserved for system functions, and cannot be handed out to pods
	// on the node. This amount CAN change on config updates, which may result in more of T than
	// we'd like being already provided to the pods.
	system T
	// watermark is the amount of T reserved to pods above which we attempt to reduce usage via
	// migration.
	watermark T
	// reserved is the current amount of T reserved to pods. It MUST be less than or equal to total,
	// and SHOULD be less than or equal to (total - system), although the latter may be temporarily
	// false after config updates.
	//
	// We try to keep reserved less than or equal to watermark, but exceeding it is a deliberate
	// part of normal operation.
	//
	// reserved is always exactly equal to the sum of all of this node's pods' reserved T.
	reserved T
	// capacityPressure is -- roughly speaking -- the amount of T that we're currently denying to
	// pods in this node when they request it, due to not having space in remainingReservableCPU().
	// This value is exactly equal to the sum of each pod's capacityPressure.
	//
	// This value is used alongside the "logical pressure" (equal to reserved - watermark, if
	// nonzero) in tooMuchPressure() to determine if more pods should be migrated off the node to
	// free up pressure.
	capacityPressure T
	// pressureAccountedFor gives the total pressure expected to be relieved by ongoing migrations.
	// This is equal to the sum of reserved + capacityPressure for all pods currently migrating.
	//
	// The value may be larger than capacityPressure.
	pressureAccountedFor T
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
	// reserved is the amount of T that this pod has reserved. It is guaranteed that the pod is
	// using AT MOST reserved T.
	reserved T
	// capacityPressure is this pod's contribution to this pod's node's capacityPressure for this
	// resource
	capacityPressure T
}

// totalReservableCPU returns the amount of node CPU that may be allocated to VM pods -- i.e.,
// excluding the CPU pre-reserved for system tasks.
func (s *nodeState) totalReservableCPU() uint16 {
	return s.vCPU.total - s.vCPU.system
}

// totalReservableMemSlots returns the number of memory slots that may be allocated to VM pods --
// i.e., excluding the memory pre-reserved for system tasks.
func (s *nodeState) totalReservableMemSlots() uint16 {
	return s.memSlots.total - s.memSlots.system
}

// remainingReservableCPU returns the remaining CPU that can be allocated to VM pods
func (s *nodeState) remainingReservableCPU() uint16 {
	return s.totalReservableCPU() - s.vCPU.reserved
}

// remainingReservableMemSlots returns the remaining number of memory slots that can be allocated to
// VM pods
func (s *nodeState) remainingReservableMemSlots() uint16 {
	return s.totalReservableMemSlots() - s.memSlots.reserved
}

// tooMuchPressure is used to signal whether the node should start migrating pods out in order to
// relieve some of the pressure
func (s *nodeState) tooMuchPressure() bool {
	if s.vCPU.reserved <= s.vCPU.watermark && s.memSlots.reserved < s.memSlots.watermark {
		klog.V(1).Infof(
			"[autoscale-enforcer] tooMuchPressure(%s) = false (vCPU: reserved %d < watermark %d, mem: reserved %d < watermark %d)",
			s.name, s.vCPU.reserved, s.vCPU.watermark, s.memSlots.reserved, s.memSlots.watermark,
		)
		return false
	}

	logicalCpuPressure := s.vCPU.reserved - s.vCPU.watermark
	logicalMemPressure := s.memSlots.reserved - s.memSlots.watermark

	tooMuchCpu := logicalCpuPressure+s.vCPU.capacityPressure > s.vCPU.pressureAccountedFor
	tooMuchMem := logicalMemPressure+s.memSlots.capacityPressure > s.memSlots.pressureAccountedFor

	result := tooMuchCpu || tooMuchMem

	fmtString := "[autoscale-enforcer] tooMuchPressure(%s) = %v. " +
		"vCPU: {logical: %d, capacity: %d, accountedFor: %d}, " +
		"mem: {logical: %d, capacity: %d, accountedFor: %d}"

	klog.V(1).Infof(
		fmtString,
		// tooMuchPressure(%s) = %v
		s.name, result,
		// vCPU: {logical: %d, capacity: %d, accountedFor: %d}
		logicalCpuPressure, s.vCPU.capacityPressure, s.vCPU.pressureAccountedFor,
		// mem: {logical: %d, capacity: %d, accountedFor: %d}
		logicalMemPressure, s.memSlots.capacityPressure, s.memSlots.pressureAccountedFor,
	)

	return result
}

// checkOkToMigrate allows us to check that it's still ok to start migrating a pod, after it was
// previously selected for migration
//
// A returned error indicates that the pod's resource usage has changed enough that we should try to
// migrate something else first. The error provides justification for this.
func (s *podState) checkOkToMigrate(oldMetrics api.Metrics) error {
	// TODO
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
		return nil, fmt.Errorf("Error querying node information: %s", err)
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

	// Fetch this upfront, because we'll need it a couple times later.
	nodeConf := s.conf.forNode(nodeName)

	// helper string for error messages
	hasAllocatableMsg := "it does have Allocatable, but config.fallbackToAllocatable = false. set it to true for a temporary hotfix"

	// cpuQ = "cpu, as a K8s resource.Quantity"
	cpuQ := node.Status.Capacity.Cpu()
	if cpuQ == nil {
		allocatableCPU := node.Status.Allocatable.Cpu()
		if allocatableCPU != nil {
			if s.conf.FallbackToAllocatable {
				klog.Warningf(
					"[autoscale-enforcer] Node %s has no CPU capacity limit, using Allocatable limit",
					nodeName,
				)
				cpuQ = allocatableCPU
			} else {
				return nil, fmt.Errorf("Node has no Capacity CPU limit (%s)", hasAllocatableMsg)
			}
		} else {
			return nil, fmt.Errorf("Node has no Capacity or Allocatable CPU limits")
		}
	}

	maxCPU := uint16(cpuQ.MilliValue() / 1000) // cpu.Value rounds up. We don't want to do that.
	vCPU, err := nodeConf.vCpuLimits(maxCPU)
	if err != nil {
		return nil, fmt.Errorf("Error calculating vCPU limits for node %s: %w", nodeName, err)
	}

	// memQ = "mem, as a K8s resource.Quantity"
	memQ := node.Status.Capacity.Memory()
	if memQ == nil {
		allocatableMem := node.Status.Allocatable.Memory()
		if allocatableMem != nil {
			if s.conf.FallbackToAllocatable {
				klog.Warningf(
					"[autoscale-enforcer] Node %s has no Memory capacity limit, using Allocatable limit",
					nodeName,
				)
				memQ = allocatableMem
			} else {
				return nil, fmt.Errorf("Node has not Capacity Memory limit (%s)", hasAllocatableMsg)
			}
		} else {
			return nil, fmt.Errorf("Node has not Capacity or Allocatable Memory limits")
		}
	}
	// note: Value() rounds up. That's ok (probably), because the computation for totalSlots will
	// round down.
	totalSlots := memQ.Value() / s.conf.MemSlotSize.Value()
	// Check that totalSlots fits within a uint16
	if totalSlots > (1<<16 - 1) {
		return nil, fmt.Errorf(
			"Node memory too big for current slot size, calculated at %d memory slots",
			totalSlots,
		)
	}
	memSlots, err := nodeConf.memoryLimits(uint16(totalSlots))
	if err != nil {
		return nil, fmt.Errorf("Error calculating memory slot limits for node %s: %w", nodeName, err)
	}

	n := &nodeState{
		name:        nodeName,
		vCPU:        vCPU,
		memSlots:    memSlots,
		pods:        make(map[api.PodName]*podState),
		computeUnit: &nodeConf.ComputeUnit,
	}

	fmtString := "[autoscale-enforcer] Fetched node %s:\n" +
		"\tCPU:    total = %d (milli = %d), max reservable = %d, watermark = %d\n" +
		"\tMemory: total = %d slots (value = %d), max reservable = %d, watermark = %d"

	klog.Infof(
		fmtString,
		// fetched node %s
		nodeName,
		// cpu: total = %d (milli = %d), max reservable = %d, watermark = %d
		maxCPU, cpuQ.MilliValue(), n.totalReservableCPU(), n.vCPU.watermark,
		// mem: total = %d (value = %d), max reservable = %d, watermark = %d
		totalSlots, memQ.Value(), n.totalReservableMemSlots(), n.memSlots.watermark,
	)

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

// This method is /basically/ the same as e.Unreserve, but the API is different and it has different
// logs, so IMO it's worthwhile to have this separate.
func (e *AutoscaleEnforcer) handleVMDeletion(podName api.PodName) {
	klog.Infof("[autoscale-enforcer] Handling deletion of pod %v", podName)

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.podMap[podName]
	if !ok {
		klog.Warningf("[autoscale-enforcer] delete: Cannot find pod %v in podMap", podName)
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

	fmtString := "[autoscale-enforcer] Deleted%s pod %v from node %s:\n" +
		"\tvCPU verdict: %s\n" +
		"\t mem verdict: %s"
	klog.Infof(fmtString, migrating, pod.name, pod.node.name, vCPUVerdict, memVerdict)
}

func (s *podState) isBetterMigrationTarget(other *podState) bool {
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
	oldNodeVCPUPressure := pod.node.vCPU.capacityPressure
	oldNodeVCPUPressureAccountedFor := pod.node.vCPU.pressureAccountedFor
	pod.node.vCPU.pressureAccountedFor += pod.vCPU.reserved + pod.vCPU.capacityPressure

	klog.Infof(
		"[autoscaler-enforcer] Migrate pod %v; node.vCPU.capacityPressure %d -> %d (%d -> %d spoken for)",
		pod.name, oldNodeVCPUPressure, pod.node.vCPU.capacityPressure, oldNodeVCPUPressureAccountedFor, pod.node.vCPU.pressureAccountedFor,
	)

	// note: unimplemented for now, pending NeonVM implementation.
	return fmt.Errorf("VM migration is currently unimplemented")
}

func (s *pluginState) handleUpdatedConf() {
	panic("todo")
}
