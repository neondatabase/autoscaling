package plugin

// Definitions and helper functions for managing plugin state

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	virtclient "github.com/neondatabase/virtink/pkg/generated/clientset/versioned"
	virtapi "github.com/neondatabase/virtink/pkg/apis/virt/v1alpha1"

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

	// vmName is the name of the VM, as given by the 'virtink.io/vm.name' label.
	vmName string

	// testingOnlyAlwaysMigrate is a test-only debugging flag that, if present in the pod's labels,
	// will always prompt it to mgirate, regardless of whether the VM actually *needs* to.
	testingOnlyAlwaysMigrate bool

	// node provides information about the node that this pod is bound to or reserved onto.
	node *nodeState
	// vCPU is the current state of vCPU utilization and pressure
	vCPU podResourceState[uint16]

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

// remainingReservableCPU returns the remaining CPU that can be allocated to VM pods
func (s *nodeState) remainingReservableCPU() uint16 {
	return s.totalReservableCPU() - s.vCPU.reserved
}

// tooMuchPressure is used to signal whether the node should start migrating pods out in order to
// relieve some of the pressure
func (s *nodeState) tooMuchPressure() bool {
	if s.vCPU.reserved <= s.vCPU.watermark {
		klog.V(1).Infof(
			"[autoscale-enforcer] tooMuchPressure(%s) = false (reserved %d < watermark %d)",
			s.name, s.vCPU.reserved, s.vCPU.watermark,
		)
		return false
	}

	logicalPressure := s.vCPU.reserved - s.vCPU.watermark
	result := logicalPressure + s.vCPU.capacityPressure > s.vCPU.pressureAccountedFor

	klog.V(1).Infof(
		"[autoscale-enforcer] tooMuchPressure(%s) = %v. logical = %d, capacity = %d, accountedFor = %d",
		s.name, result, logicalPressure, s.vCPU.capacityPressure, s.vCPU.pressureAccountedFor,
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

func getPodInfo(
	pod *corev1.Pod,
) (
	*struct{ initVCPU uint16; vmName string; alwaysMigrate bool},
	error,
) {
	// If this isn't a VM, it shouldn't have been scheduled with us
	name, ok := pod.Labels[LabelVM]
	if !ok {
		return nil, fmt.Errorf("Pod is not a VM (missing %s label)", LabelVM)
	}

	initVCPUString, ok := pod.Labels[LabelInitVCPU]
	if !ok {
		return nil, fmt.Errorf("Missing init vCPU label %s", LabelInitVCPU)
	}

	initVCPU, err := strconv.ParseUint(initVCPUString, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("Error parsing label %s as uint16: %s", LabelInitVCPU, err)
	}

	_, alwaysMigrate := pod.Labels[LabelTestingOnlyAlwaysMigrate]

	return &struct{initVCPU uint16; vmName string; alwaysMigrate bool}{
		initVCPU: uint16(initVCPU),
		vmName: name,
		alwaysMigrate: alwaysMigrate,
	}, nil
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

	cpu := node.Status.Capacity.Cpu()
	if cpu == nil {
		allocatableCPU := node.Status.Allocatable.Cpu()
		if allocatableCPU != nil {
			if s.conf.FallbackToAllocatable {
				klog.Warningf(
					"[autoscale-enforcer] Node %s has no CPU capacity, using Allocatable limit", nodeName,
				)
				cpu = allocatableCPU
			} else {
				return nil, fmt.Errorf("Node has no Capacity CPU limit (it does have Allocatable, but config.fallbackToAllocated = false. set it to true for a temporary hotfix)")
			}
		} else {
			return nil, fmt.Errorf("Node has no Capacity or Allocatable CPU limits")
		}
	}

	maxCPU := uint16(cpu.MilliValue() / 1000) // cpu.Value rounds up. We don't want to do that.
	vCPU, err := s.conf.forNode(nodeName).vCpuLimits(maxCPU)
	if err != nil {
		return nil, fmt.Errorf("Error calculating vCPU limits for node %s: %w", nodeName, err)
	}

	n := &nodeState{
		name: nodeName,
		vCPU: vCPU,
		pods: make(map[api.PodName]*podState),
	}

	klog.Infof(
		"[autoscale-enforcer] Fetched node %s CPU total = %d (milli = %d), max reservable = %d, watermark = %d",
		nodeName, maxCPU, cpu.MilliValue(), n.totalReservableCPU(), n.vCPU.watermark,
	)

	// update maxTotalReservableCPU if there's a new maximum
	totalReservableCPU := n.totalReservableCPU()
	if totalReservableCPU > s.maxTotalReservableCPU {
		s.maxTotalReservableCPU = totalReservableCPU
	}

	s.nodeMap[nodeName] = n
	return n, nil
}

// This method is /basically/ the same as e.Unreserve, but the API is different and it has different
// logs, so IMO it's worthwhile to have this separate.
func (e *AutoscaleEnforcer) handleVMDeletion(pName api.PodName) {
	klog.Infof("[autoscale-enforcer] Handling deletion of pod %v", pName)

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	ps, ok := e.state.podMap[pName]
	if !ok {
		klog.Warningf("[autoscale-enforcer] delete: Cannot find pod %v in podMap", pName)
		return
	}

	// Mark the resources as no longer reserved and delete the pod
	delete(e.state.podMap, pName)
	delete(ps.node.pods, pName)
	ps.node.mq.removeIfPresent(ps)
	oldReserved := ps.node.vCPU.reserved
	oldPressure := ps.node.vCPU.capacityPressure
	oldPressureAccountedFor := ps.node.vCPU.pressureAccountedFor
	ps.node.vCPU.reserved -= ps.vCPU.reserved
	ps.node.vCPU.capacityPressure -= ps.vCPU.capacityPressure

	// Something we have to handle here but not in Unreserve: the possibility of a VM being deleted
	// mid-migration:
	var migrating string
	if ps.currentlyMigrating() {
		ps.node.vCPU.pressureAccountedFor -= ps.vCPU.reserved + ps.vCPU.capacityPressure
		migrating = " migrating"
	}

	klog.Infof(
		"[autoscale-enforcer] Deleted%s pod %v (%d vCPU) from node %s: node.vCPU.reserved %d -> %d, node.vCPU.capacityPressure %d -> %d (%d -> %d spoken for)",
		migrating, pName, ps.vCPU.reserved, ps.node.name, oldReserved, ps.node.vCPU.reserved,
		oldPressure, ps.node.vCPU.capacityPressure, oldPressureAccountedFor, ps.node.vCPU.pressureAccountedFor,
	)
}

func (e *AutoscaleEnforcer) handleVMMFinished(ctx context.Context, vmmName api.PodName) {
	// Currently, VM migration objects have unique names, so we don't need to worry about name
	// reuse. However, the objects don't currently get cleaned up by Virtink, so we need to delete
	// them ourselves.

	klog.Infof("[autoscale-enforcer] Deleting VM Migration %v", vmmName)

	err := e.virtClient.VirtV1alpha1().
		VirtualMachineMigrations(vmmName.Namespace).
		Delete(ctx, vmmName.Name, metav1.DeleteOptions{})

	if err != nil {
		klog.Errorf("[autoscale-enforcer] Error deleting VM Migration %v: %s", vmmName, err)
	}
}

func (s *podState) isBetterMigrationTarget(other *podState) bool {
	// TODO - this is just a first-pass approximation. Maybe it's ok for now? Maybe it's not. Idk.
	return s.metrics.LoadAverage1Min < other.metrics.LoadAverage1Min
}

type patchValue[T any] struct{
	Op string `json:"op"`
	Path string `json:"path"`
	Value T `json:"value"`
}

// this method can only be called while holding a lock. It will be released temporarily while we
// send requests to the API server
//
// A lock will ALWAYS be held on return from this function.
func (s *pluginState) startMigration(ctx context.Context, pod *podState, virtClient *virtclient.Clientset) error {
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
	s.lock.Unlock() // Unlock while we make the API server requests

	var locked bool // In order to prevent double-unlock panics, we always lock on return.
	defer func() {
		if !locked {
			s.lock.Lock()
		}
	}()
	
	// Patch init vCPU for the VM, so that the migration target pod has the correct vCPU count
	patchPayload := []patchValue[string]{{
		Op: "replace",
		// if '/' is in the label, it's replaced by '~1'. Source:
		//   https://stackoverflow.com/questions/36147137#comment98654379_36163917
		Path: "/metadata/labels/autoscaler~1init-vcpu",
		Value: fmt.Sprintf("%d", pod.vCPU.reserved), // labels are strings
	}}

	patchPayloadBytes, err := json.Marshal(patchPayload)
	if err != nil {
		return fmt.Errorf("Error marshalling JSON patch payload: %s", err)
	}
	klog.Infof("[autoscale-enforcer] Sending VM %s:%s patch request", pod.name.Namespace, pod.vmName)
	_, err = virtClient.VirtV1alpha1().
		VirtualMachines("default").
		Patch(ctx, pod.vmName, ktypes.JSONPatchType, patchPayloadBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("Error executing VM patch request: %s", err)
	}

	// ... And then actually start the migration:
	vmMigration := virtapi.VirtualMachineMigration{
		ObjectMeta: metav1.ObjectMeta{
			// migrations need to be named, but we might end up starting a new migration before the
			// old one has been removed, so we use GenerateName instead of directly setting Name
			GenerateName: fmt.Sprintf("%s-migration-", pod.vmName),
			Namespace: pod.name.Namespace,
		},
		Spec: virtapi.VirtualMachineMigrationSpec{
			VMName: pod.vmName,
		},
	}
	klog.Infof("[autoscale-enforcer] Sending VM Migration %s:%s create request", pod.name.Namespace, pod.vmName)
	_, err = virtClient.VirtV1alpha1().
		VirtualMachineMigrations(pod.name.Namespace).
		Create(ctx, &vmMigration, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("Error executing VM Migration create request: %s", err)
	}

	return nil
}

func (s *pluginState) handleUpdatedConf() {
	panic("todo")
}
