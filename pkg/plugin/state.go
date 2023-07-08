package plugin

// Definitions and helper functions for managing plugin state

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

// pluginState stores the private state for the plugin, used both within and outside of the
// predefined scheduler plugin points
//
// Accessing the individual fields MUST be done while holding a lock.
type pluginState struct {
	lock util.ChanMutex

	podMap  map[util.NamespacedName]*podState
	nodeMap map[string]*nodeState

	// otherPods stores information about non-VM pods
	otherPods map[util.NamespacedName]*otherPodState

	// maxTotalReservableCPU stores the maximum value of any node's totalReservableCPU(), so that we
	// can appropriately scale our scoring
	maxTotalReservableCPU vmapi.MilliCPU
	// maxTotalReservableMemSlots is the same as maxTotalReservableCPU, but for memory slots instead
	// of CPU
	maxTotalReservableMemSlots uint16
	// conf stores the current configuration, and is nil if the configuration has not yet been set
	//
	// Proper initialization of the plugin guarantees conf is not nil.
	conf *Config
}

func (s *pluginState) memSlotSizeBytes() uint64 {
	return uint64(s.conf.MemSlotSize.Value())
}

// nodeState is the information that we track for a particular
type nodeState struct {
	// name is the name of the node, guaranteed by kubernetes to be unique
	name string

	// nodeGroup, if present, gives the node group that this node belongs to.
	nodeGroup string

	// vCPU tracks the state of vCPU resources -- what's available and how
	vCPU nodeResourceState[vmapi.MilliCPU]
	// memSlots tracks the state of memory slots -- what's available and how
	memSlots nodeResourceState[uint16]

	computeUnit *api.Resources

	// pods tracks all the VM pods assigned to this node
	//
	// This includes both bound pods (i.e., pods fully committed to the node) and reserved pods
	// (still may be unreserved)
	pods map[util.NamespacedName]*podState

	// otherPods are the non-VM pods that we're also tracking in this node
	otherPods map[util.NamespacedName]*otherPodState
	// otherResources is the sum resource usage associated with the non-VM pods
	otherResources nodeOtherResourceState

	// mq is the priority queue tracking which pods should be chosen first for migration
	mq migrationQueue
}

type nodeResourceStateField[T any] struct {
	valueName string
	value     T
}

func (s *nodeResourceState[T]) fields() []nodeResourceStateField[T] {
	return []nodeResourceStateField[T]{
		{"Total", s.Total},
		{"System", s.System},
		{"Watermark", s.Watermark},
		{"Reserved", s.Reserved},
		{"Buffer", s.Buffer},
		{"CapacityPressure", s.CapacityPressure},
		{"PressureAccountedFor", s.PressureAccountedFor},
	}
}

func (s *nodeState) updateMetrics(metrics PromMetrics, memSlotSizeBytes uint64) {
	s.vCPU.updateMetrics(metrics.nodeCPUResources, s.name, s.nodeGroup, vmapi.MilliCPU.AsFloat64)
	s.memSlots.updateMetrics(metrics.nodeMemResources, s.name, s.nodeGroup, func(memSlots uint16) float64 {
		return float64(uint64(memSlots) * memSlotSizeBytes) // convert memSlots -> bytes
	})
}

func (s *nodeResourceState[T]) updateMetrics(metric *prometheus.GaugeVec, nodeName, nodeGroup string, convert func(T) float64) {
	for _, f := range s.fields() {
		metric.WithLabelValues(nodeName, nodeGroup, f.valueName).Set(convert(f.value))
	}
}

func (s *nodeState) removeMetrics(metrics PromMetrics) {
	gauges := []*prometheus.GaugeVec{metrics.nodeCPUResources, metrics.nodeMemResources}
	fields := s.vCPU.fields() // No particular reason to be CPU, we just want the valueNames, and CPU vs memory valueNames are the same

	for _, g := range gauges {
		for _, f := range fields {
			g.DeleteLabelValues(s.name, s.nodeGroup, f.valueName)
		}
	}
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

	ReservedCPU      vmapi.MilliCPU `json:"reservedCPU"`
	ReservedMemSlots uint16         `json:"reservedMemSlots"`

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
	name util.NamespacedName

	// vmName is the name of the VM, as given by the 'vm.neon.tech/name' label (and name.Namespace)
	vmName util.NamespacedName

	// testingOnlyAlwaysMigrate is a test-only debugging flag that, if present in the pod's labels,
	// will always prompt it to mgirate, regardless of whether the VM actually *needs* to.
	testingOnlyAlwaysMigrate bool

	// node provides information about the node that this pod is bound to or reserved onto.
	node *nodeState
	// vCPU is the current state of this pod's vCPU utilization and pressure
	vCPU podResourceState[vmapi.MilliCPU]
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
type podMigrationState struct {
	// name gives the name of the VirtualMachineMigration that this pod is involved in
	name util.NamespacedName
}

type podResourceState[T any] struct {
	// Reserved is the amount of T that this pod has reserved. It is guaranteed that the pod is
	// using AT MOST Reserved T.
	Reserved T `json:"reserved"`
	// Buffer is the amount of T that we've included in Reserved to account for the possibility of
	// unilateral increases by the autoscaler-agent
	//
	// This value is only nonzero during startup (between initial state load and first communication
	// from the autoscaler-agent), and MUST be less than or equal to reserved.
	//
	// After the first communication from the autoscaler-agent, we update Reserved to match its
	// value, and set Buffer to zero.
	Buffer T `json:"buffer"`
	// CapacityPressure is this pod's contribution to this pod's node's CapacityPressure for this
	// resource
	CapacityPressure T `json:"capacityPressure"`

	// Min and Max give the minimum and maximum values of this resource that the VM may use.
	Min T `json:"min"`
	Max T `json:"max"`
}

// otherPodState tracks a little bit of information for the non-VM pods we're handling
type otherPodState struct {
	name      util.NamespacedName
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
		r.ReservedCPU = vmapi.MilliCPUFromResourceQuantity(cpuCopy)
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
func (s *nodeState) totalReservableCPU() vmapi.MilliCPU {
	return s.vCPU.Total - s.vCPU.System
}

// totalReservableMemSlots returns the number of memory slots that may be allocated to VM pods --
// i.e., excluding the memory pre-reserved for system tasks.
func (s *nodeState) totalReservableMemSlots() uint16 {
	return s.memSlots.Total - s.memSlots.System
}

// remainingReservableCPU returns the remaining CPU that can be allocated to VM pods
func (s *nodeState) remainingReservableCPU() vmapi.MilliCPU {
	return s.totalReservableCPU() - s.vCPU.Reserved
}

// remainingReservableMemSlots returns the remaining number of memory slots that can be allocated to
// VM pods
func (s *nodeState) remainingReservableMemSlots() uint16 {
	return s.totalReservableMemSlots() - s.memSlots.Reserved
}

// tooMuchPressure is used to signal whether the node should start migrating pods out in order to
// relieve some of the pressure
func (s *nodeState) tooMuchPressure(logger *zap.Logger) bool {
	if s.vCPU.Reserved <= s.vCPU.Watermark && s.memSlots.Reserved < s.memSlots.Watermark {
		type okPair[T any] struct {
			Reserved  T
			Watermark T
		}

		logger.Debug(
			"tooMuchPressure = false (clearly)",
			zap.Any("vCPU", okPair[vmapi.MilliCPU]{Reserved: s.vCPU.Reserved, Watermark: s.vCPU.Watermark}),
			zap.Any("memSlots", okPair[uint16]{Reserved: s.memSlots.Reserved, Watermark: s.memSlots.Watermark}),
		)
		return false
	}

	type info[T any] struct {
		LogicalPressure T
		LogicalSlack    T
		Capacity        T
		AccountedFor    T
		TooMuch         bool
	}

	var cpu info[vmapi.MilliCPU]
	var mem info[uint16]

	cpu.LogicalPressure = util.SaturatingSub(s.vCPU.Reserved, s.vCPU.Watermark)
	mem.LogicalPressure = util.SaturatingSub(s.memSlots.Reserved, s.memSlots.Watermark)

	// Account for existing slack in the system, to counteract capacityPressure that hasn't been
	// updated yet
	cpu.LogicalSlack = s.vCPU.Buffer + util.SaturatingSub(s.vCPU.Watermark, s.vCPU.Reserved)
	mem.LogicalSlack = s.memSlots.Buffer + util.SaturatingSub(s.memSlots.Watermark, s.memSlots.Reserved)

	cpu.TooMuch = cpu.LogicalPressure+s.vCPU.CapacityPressure > s.vCPU.PressureAccountedFor+cpu.LogicalSlack
	mem.TooMuch = mem.LogicalPressure+s.memSlots.CapacityPressure > s.memSlots.PressureAccountedFor+mem.LogicalSlack

	result := cpu.TooMuch || mem.TooMuch

	logger.Debug(
		fmt.Sprintf("tooMuchPressure = %v", result),
		zap.Any("vCPU", cpu),
		zap.Any("memSlots", mem),
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
	logger *zap.Logger,
	metrics PromMetrics,
	store IndexedNodeStore,
	nodeName string,
) (*nodeState, error) {
	logger = logger.With(zap.String("node", nodeName))

	if n, ok := s.nodeMap[nodeName]; ok {
		logger.Debug("Using stored information for node")
		return n, nil
	}

	logger.Info("Node has not yet been processed, fetching from store")

	accessor := func(index *watch.FlatNameIndex[corev1.Node]) (*corev1.Node, bool) {
		return index.Get(nodeName)
	}

	// Before unlocking, try to get the node from the store.
	node, ok := store.GetIndexed(accessor)
	if !ok {
		logger.Warn("Node is missing from local store. Relisting to try getting it from API server")

		s.lock.Unlock() // Unlock to let other goroutines progress while we get the data we need

		var locked bool // In order to prevent double-unlock panics, we always lock on return.
		defer func() {
			if !locked {
				s.lock.Lock()
			}
		}()

		// Use a reasonable timeout on the relist request, so that if the store is broken, we won't
		// block forever.
		//
		// FIXME: make this configurable
		timeout := 5 * time.Second
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		select {
		case <-store.Relist():
		case <-timer.C:
			message := "Timed out waiting on Node store relist"
			logger.Error(message, zap.Duration("timeout", timeout))
			return nil, errors.New(message)
		case <-ctx.Done():
			err := ctx.Err()
			message := "Context expired while waiting on Node store relist"
			logger.Error(message, zap.Error(err))
			return nil, errors.New(message)
		}

		node, ok = store.GetIndexed(accessor)
		if !ok {
			// Either the node is already gone, or there's a deeper problem.
			message := "Could not find Node, even after relist"
			logger.Error(message)
			return nil, errors.New(message)
		}

		logger.Info("Found node after relisting")

		// Re-lock and process API result
		locked = true
		s.lock.Lock()

		// It's possible that the node was already added. Don't double-process nodes if we don't have
		// to.
		if n, ok := s.nodeMap[nodeName]; ok {
			logger.Warn("Local information for node became available while waiting on relist, using it instead")
			return n, nil
		}
	}

	n, err := buildInitialNodeState(logger, node, s.conf)
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

	n.updateMetrics(metrics, s.memSlotSizeBytes())

	s.nodeMap[nodeName] = n
	return n, nil
}

// this method must only be called while holding s.lock. It will not be released during this
// function.
//
// Note: buildInitialNodeState does not take any of the pods or VMs on the node into account; it
// only examines the total resources available to the node.
func buildInitialNodeState(logger *zap.Logger, node *corev1.Node, conf *Config) (*nodeState, error) {
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

	var nodeGroup string
	if conf.K8sNodeGroupLabel != "" {
		var ok bool
		nodeGroup, ok = node.Labels[conf.K8sNodeGroupLabel]
		if !ok {
			logger.Warn("Node does not have node group label", zap.String("label", conf.K8sNodeGroupLabel))
		}
	}

	n := &nodeState{
		name:      node.Name,
		nodeGroup: nodeGroup,
		vCPU:      vCPU,
		memSlots:  memSlots,
		pods:      make(map[util.NamespacedName]*podState),
		otherPods: make(map[util.NamespacedName]*otherPodState),
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

	type resourceInfo[T any] struct {
		Total           T
		Raw             *resource.Quantity
		Margin          *resource.Quantity
		TotalReservable T
		Watermark       T
	}

	logger.Info(
		"Built initial node state",
		zap.Any("cpu", resourceInfo[vmapi.MilliCPU]{
			Total:           n.vCPU.Total,
			Raw:             cpuQ,
			Margin:          n.otherResources.MarginCPU,
			TotalReservable: n.totalReservableCPU(),
			Watermark:       n.vCPU.Watermark,
		}),
		zap.Any("memSlots", resourceInfo[uint16]{
			Total:           n.memSlots.Total,
			Raw:             memQ,
			Margin:          n.otherResources.MarginMemory,
			TotalReservable: n.totalReservableMemSlots(),
			Watermark:       n.memSlots.Watermark,
		}),
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

func (e *AutoscaleEnforcer) handleNodeDeletion(logger *zap.Logger, nodeName string) {
	logger = logger.With(
		zap.String("action", "Node deletion"),
		zap.String("node", nodeName),
	)

	logger.Info("Handling deletion of Node")

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	node, ok := e.state.nodeMap[nodeName]
	if !ok {
		logger.Warn("Cannot find node in nodeMap")
	}

	if logger.Core().Enabled(zapcore.DebugLevel) {
		logger.Debug("Dump final node state", zap.Any("state", node.dump()))
	}

	// For any pods still on the node, remove them from the global state:
	for name, pod := range node.pods {
		logger.Warn(
			"Found VM pod still on node at time of deletion",
			zap.Object("pod", name),
			zap.Object("virtualmachine", pod.vmName),
		)
		delete(e.state.podMap, name)
	}
	for name := range node.otherPods {
		logger.Warn("Found non-VM pod still on node at time of deletion", zap.Object("pod", name))
		delete(e.state.otherPods, name)
	}

	node.removeMetrics(e.metrics)

	delete(e.state.nodeMap, nodeName)
	logger.Info("Deleted node")
}

// This method is /basically/ the same as e.Unreserve, but the API is different and it has different
// logs, so IMO it's worthwhile to have this separate.
func (e *AutoscaleEnforcer) handleVMDeletion(logger *zap.Logger, podName util.NamespacedName) {
	logger = logger.With(
		zap.String("action", "VM deletion"),
		zap.Object("pod", podName),
	)

	logger.Info("Handling deletion of VM pod")

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.podMap[podName]
	if !ok {
		logger.Warn("Cannot find pod in podMap")
		return
	}
	logger = logger.With(zap.String("node", pod.node.name), zap.Object("virtualmachine", pod.vmName))

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

	pod.node.updateMetrics(e.metrics, e.state.memSlotSizeBytes())

	logger.Info(
		"Deleted VM pod",
		zap.Bool("migrating", currentlyMigrating),
		zap.Object("verdict", verdictSet{
			cpu: vCPUVerdict,
			mem: memVerdict,
		}),
	)
}

func (e *AutoscaleEnforcer) handleVMDisabledScaling(logger *zap.Logger, podName util.NamespacedName) {
	logger = logger.With(
		zap.String("action", "VM scaling disabled"),
		zap.Object("pod", podName),
	)

	logger.Info("Handling disabled autoscaling for VM pod")

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.podMap[podName]
	if !ok {
		logger.Error("Cannot find pod in podMap")
		return
	}
	logger = logger.With(zap.String("node", pod.node.name), zap.Object("virtualmachine", pod.vmName))

	// Reset buffer to zero:
	vCPUVerdict := collectResourceTransition(&pod.node.vCPU, &pod.vCPU).
		handleAutoscalingDisabled()
	memVerdict := collectResourceTransition(&pod.node.memSlots, &pod.memSlots).
		handleAutoscalingDisabled()

	pod.node.updateMetrics(e.metrics, e.state.memSlotSizeBytes())

	logger.Info(
		"Disabled autoscaling for VM pod",
		zap.Object("verdict", verdictSet{
			cpu: vCPUVerdict,
			mem: memVerdict,
		}),
	)
}

func (e *AutoscaleEnforcer) handlePodStartMigration(logger *zap.Logger, podName, migrationName util.NamespacedName, source bool) {
	logger = logger.With(
		zap.String("action", "VM pod start migration"),
		zap.Object("pod", podName),
		zap.Object("virtualmachinemigration", migrationName),
	)

	logger.Info("Handling VM pod migration start")

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.podMap[podName]
	if !ok {
		logger.Warn("Cannot find pod in podMap")
		return
	}
	logger = logger.With(zap.String("node", pod.node.name), zap.Object("virtualmachine", pod.vmName))

	// Reset buffer to zero, remove from migration queue (if in it), and set pod's migrationState
	cpuVerdict := collectResourceTransition(&pod.node.vCPU, &pod.vCPU).
		handleStartMigration(source)
	memVerdict := collectResourceTransition(&pod.node.memSlots, &pod.memSlots).
		handleStartMigration(source)

	pod.node.mq.removeIfPresent(pod)
	pod.migrationState = &podMigrationState{name: migrationName}

	pod.node.updateMetrics(e.metrics, e.state.memSlotSizeBytes())

	logger.Info(
		"Handled start of migration involving pod",
		zap.Object("verdict", verdictSet{
			cpu: cpuVerdict,
			mem: memVerdict,
		}),
	)
}

func (e *AutoscaleEnforcer) handlePodEndMigration(logger *zap.Logger, podName, migrationName util.NamespacedName) {
	logger = logger.With(
		zap.String("action", "VM pod end migration"),
		zap.Object("virtualmachinemigration", migrationName),
	)

	logger.Info("Handling VM pod migration end")

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.podMap[podName]
	if !ok {
		logger.Warn("Cannot find pod in podMap")
		return
	}
	logger = logger.With(zap.String("node", pod.node.name), zap.Object("virtualmachine", pod.vmName))

	pod.migrationState = nil

	//nolint:gocritic // NOTE: not *currently* needed, but this should be kept here as a reminder, in case that changes.
	// pod.node.updateMetrics(e.metrics, e.state.memSlotSizeBytes())

	logger.Info("Recorded end of migration for VM pod")
}

func (e *AutoscaleEnforcer) handlePodDeletion(logger *zap.Logger, podName util.NamespacedName) {
	logger = logger.With(
		zap.String("action", "non-VM Pod deletion"),
		zap.Object("pod", podName),
	)

	logger.Info("Handling non-VM Pod deletion")

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.otherPods[podName]
	if !ok {
		logger.Warn("Cannot find pod in otherPods")
		return
	}
	logger = logger.With(zap.String("node", pod.node.name))

	// Mark the resources as no longer reserved
	cpuVerdict, memVerdict := handleDeletedPod(pod.node, pod.resources, &e.state.conf.MemSlotSize)

	delete(e.state.otherPods, podName)
	delete(pod.node.otherPods, podName)

	pod.node.updateMetrics(e.metrics, e.state.memSlotSizeBytes())

	logger.Info(
		"Deleted non-VM pod",
		zap.Object("verdict", verdictSet{
			cpu: cpuVerdict,
			mem: memVerdict,
		}),
	)
}

func (e *AutoscaleEnforcer) handleUpdatedScalingBounds(logger *zap.Logger, vm *api.VmInfo, unqualifiedPodName string) {
	podName := util.NamespacedName{Namespace: vm.Namespace, Name: unqualifiedPodName}

	logger = logger.With(
		zap.String("action", "VM updated scaling bounds"),
		zap.Object("pod", podName),
		zap.Object("virtualmachine", vm.NamespacedName()),
	)

	logger.Info("Handling updated scaling bounds for VM")

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.podMap[podName]
	if !ok {
		logger.Error("Cannot find Pod in podMap")
		return
	}
	logger = logger.With(zap.String("node", pod.node.name))

	// FIXME: this definition of receivedContact may be inaccurate if there was an error with the
	// autoscaler-agent's request.
	receivedContact := pod.mostRecentComputeUnit != nil
	var n *nodeResourceState[vmapi.MilliCPU] = &pod.node.vCPU
	cpuVerdict := handleUpdatedLimits(n, &pod.vCPU, receivedContact, vm.Cpu.Min, vm.Cpu.Max)
	memVerdict := handleUpdatedLimits(&pod.node.memSlots, &pod.memSlots, receivedContact, vm.Mem.Min, vm.Mem.Max)

	pod.node.updateMetrics(e.metrics, e.state.memSlotSizeBytes())

	logger.Info(
		"Updated scaling bounds for VM pod",
		zap.Object("verdict", verdictSet{
			cpu: cpuVerdict,
			mem: memVerdict,
		}),
	)
}

func (e *AutoscaleEnforcer) handleNonAutoscalingUsageChange(logger *zap.Logger, vm *api.VmInfo, unqualifiedPodName string) {
	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	podName := util.NamespacedName{Namespace: vm.Namespace, Name: unqualifiedPodName}

	logger = logger.With(
		zap.String("action", "non-autoscaling VM usage change"),
		zap.Object("pod", podName),
		zap.Object("virtualmachine", vm.NamespacedName()),
	)

	pod, ok := e.state.podMap[podName]
	if !ok {
		logger.Error("Cannot find Pod in podMap")
		return
	}

	cpuVerdict := collectResourceTransition(&pod.node.vCPU, &pod.vCPU).
		handleNonAutoscalingUsageChange(vm.Using().VCPU)
	memVerdict := collectResourceTransition(&pod.node.memSlots, &pod.memSlots).
		handleNonAutoscalingUsageChange(vm.Using().Mem)

	pod.node.updateMetrics(e.metrics, e.state.memSlotSizeBytes())

	logger.Info(
		"Updated non-autoscaling VM usage",
		zap.Object("verdict", verdictSet{
			cpu: cpuVerdict,
			mem: memVerdict,
		}),
	)
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
func (s *pluginState) startMigration(ctx context.Context, logger *zap.Logger, pod *podState, vmClient *vmclient.Clientset) error {
	if pod.currentlyMigrating() {
		return fmt.Errorf("Pod is already migrating")
	}

	// Unlock to make the API request(s), then make sure we're locked on return.
	s.lock.Unlock()
	defer s.lock.Lock()

	vmmName := util.NamespacedName{
		Name:      fmt.Sprintf("schedplugin-%s", pod.vmName.Name),
		Namespace: pod.name.Namespace,
	}

	logger = logger.With(zap.Object("virtualmachinemigration", vmmName))

	logger.Info("Starting VirtualMachineMigration for VM")

	// Check that the migration doesn't already exist. If it does, then there's no need to recreate
	// it.
	//
	// We technically don't *need* this additional request here (because we can check the return
	// from the Create request with apierrors.IsAlreadyExists). However: the benefit we get from
	// this is that the logs are significantly clearer.
	_, err := vmClient.NeonvmV1().
		VirtualMachineMigrations(pod.name.Namespace).
		Get(ctx, vmmName.Name, metav1.GetOptions{})
	if err == nil {
		logger.Warn("VirtualMachineMigration already exists, nothing to do")
		return nil
	} else if !apierrors.IsNotFound(err) {
		// We're *expecting* to get IsNotFound = true; if err != nil and isn't NotFound, then
		// there's some unexpected error.
		logger.Error("Unexpected error doing Get request to check if migration already exists", zap.Error(err))
		return fmt.Errorf("Error checking if migration exists: %w", err)
	}

	vmm := &vmapi.VirtualMachineMigration{
		ObjectMeta: metav1.ObjectMeta{
			// TODO: it's maybe possible for this to run into name length limits? Unclear what we
			// should do if that happens.
			Name:      vmmName.Name,
			Namespace: pod.name.Namespace,
		},
		Spec: vmapi.VirtualMachineMigrationSpec{
			VmName: pod.vmName.Name,

			// FIXME: NeonVM's VirtualMachineMigrationSpec has a bunch of boolean fields that aren't
			// pointers, which means we need to explicitly set them when using the Go API.
			PreventMigrationToSameHost: true,
			CompletionTimeout:          3600,
			Incremental:                true,
			AutoConverge:               true,
			MaxBandwidth:               resource.MustParse("1Gi"),
			AllowPostCopy:              false,
		},
	}

	logger.Info("Migration doesn't already exist, creating one for VM", zap.Any("spec", vmm.Spec))
	_, err = vmClient.NeonvmV1().VirtualMachineMigrations(pod.name.Namespace).Create(ctx, vmm, metav1.CreateOptions{})
	if err != nil {
		// log here, while the logger's fields are in scope
		logger.Error("Unexpected error doing Create request for new migration", zap.Error(err))
		return fmt.Errorf("Error creating migration: %w", err)
	}
	logger.Info("VM migration request successful")

	return nil
}

// readClusterState sets the initial node and pod maps for the plugin's state, getting its
// information from the K8s cluster
//
// This method expects that all pluginState fields except pluginState.conf are set to their zero
// value, and will panic otherwise. Accordingly, this is only called during plugin initialization.
func (p *AutoscaleEnforcer) readClusterState(ctx context.Context, logger *zap.Logger) error {
	logger = logger.With(zap.String("action", "read cluster state"))

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
	//
	// As a final note: the VM store is already present, and its startup guarantees that the listing
	// has already been made.

	logger.Info("Fetching VMs from existing store")
	vms := p.vmStore.Items()

	logger.Info("Listing Pods")
	pods, err := p.handle.ClientSet().CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("Error listing Pods: %w", err)
	}

	p.state.nodeMap = make(map[string]*nodeState)
	p.state.podMap = make(map[util.NamespacedName]*podState)
	p.state.otherPods = make(map[util.NamespacedName]*otherPodState)

	// Store the VMs by name, so that we can access them as we're going through pods
	logger.Info("Building initial vmSpecs map")
	vmSpecs := make(map[util.NamespacedName]*vmapi.VirtualMachine)
	for _, vm := range vms {
		vmSpecs[util.GetNamespacedName(vm)] = vm
	}

	// Add all VM pods to the map, by filtering out from the pod list. We'll take care of the non-VM
	// pods in a separate pass after this.
	logger.Info("Adding VM pods to podMap")
	skippedVms := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		podName := util.GetNamespacedName(pod)

		if _, isVM := pod.Labels[LabelVM]; !isVM {
			continue
		}

		// new logger just for this loop iteration, with info about the Pod
		logger := logger.With(util.PodNameFields(pod))
		if pod.Spec.NodeName != "" {
			logger = logger.With(zap.String("node", pod.Spec.NodeName))
		}

		migrationName := tryMigrationOwnerReference(pod)
		if migrationName != nil {
			logger = logger.With(zap.Object("virtualmachinemigration", *migrationName))
		}

		logSkip := func(format string, args ...any) {
			logger.Warn("Skipping VM", zap.Error(fmt.Errorf(format, args...)))
			skippedVms += 1
		}

		if pod.Spec.SchedulerName != p.state.conf.SchedulerName {
			logSkip("Spec.SchedulerName %q != our config.SchedulerName %q", pod.Spec.SchedulerName, p.state.conf.SchedulerName)
			continue
		} else if pod.Spec.NodeName == "" {
			logSkip("VM pod's Spec.NodeName = \"\" (maybe it hasn't been scheduled yet?)")
			continue
		}

		// Check if the VM exists
		vmName := util.NamespacedName{Namespace: pod.Namespace, Name: pod.Labels[LabelVM]}
		vm, ok := vmSpecs[vmName]
		if !ok {
			logSkip("VM Pod's corresponding VM object is missing from our map (maybe it was removed between listing VMs and Pods?)")
			continue
		} else if util.PodCompleted(pod) {
			logSkip("Pod is in its final, complete state (phase = %q), so will not use any resources", pod.Status.Phase)
			continue
		}

		vmInfo, err := api.ExtractVmInfo(logger, vm)
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

		ns, err := p.state.getOrFetchNodeState(ctx, logger, p.metrics, p.nodeStore, pod.Spec.NodeName)
		if err != nil {
			logSkip("Couldn't find Node that VM pod is on (maybe it has since been removed?): %w", err)
			continue
		}

		// Build the pod state, update the node
		ps := &podState{
			name:   podName,
			vmName: util.GetNamespacedName(vm),
			node:   ns,
			vCPU: podResourceState[vmapi.MilliCPU]{
				Reserved:         vmInfo.Cpu.Max,
				Buffer:           vmInfo.Cpu.Max - vmInfo.Cpu.Use,
				CapacityPressure: 0,
				Min:              vmInfo.Cpu.Min,
				Max:              vmInfo.Cpu.Max,
			},
			memSlots: podResourceState[uint16]{
				Reserved:         vmInfo.Mem.Max,
				Buffer:           vmInfo.Mem.Max - vmInfo.Mem.Use,
				CapacityPressure: 0,
				Min:              vmInfo.Mem.Min,
				Max:              vmInfo.Mem.Max,
			},

			mqIndex:               -1,
			metrics:               nil,
			mostRecentComputeUnit: nil,
			migrationState:        nil,

			testingOnlyAlwaysMigrate: vmInfo.AlwaysMigrate,
		}

		// If scaling isn't enabled *or* the pod is invovled in an ongoing migration, then we can be
		// more precise about usage (because scaling is forbidden while migrating).
		if !vmInfo.ScalingEnabled || migrationName != nil {
			ps.vCPU.Buffer = 0
			ps.vCPU.Reserved = vmInfo.Cpu.Use

			ps.memSlots.Buffer = 0
			ps.memSlots.Reserved = vmInfo.Mem.Use
		}

		oldNodeVCPUReserved := ns.vCPU.Reserved
		oldNodeMemReserved := ns.memSlots.Reserved
		oldNodeVCPUBuffer := ns.vCPU.Buffer
		oldNodeMemBuffer := ns.memSlots.Buffer

		ns.vCPU.Reserved += ps.vCPU.Reserved
		ns.memSlots.Reserved += ps.memSlots.Reserved

		cpuVerdict := fmt.Sprintf(
			"pod = %v/%v (node %v -> %v / %v, %v -> %v buffer)",
			ps.vCPU.Reserved, vmInfo.Cpu.Max, oldNodeVCPUReserved, ns.vCPU.Reserved, ns.totalReservableCPU(), oldNodeVCPUBuffer, ns.vCPU.Buffer,
		)
		memVerdict := fmt.Sprintf(
			"pod = %v/%v (node %v -> %v / %v, %v -> %v buffer",
			ps.memSlots.Reserved, vmInfo.Mem.Max, oldNodeMemReserved, ns.memSlots.Reserved, ns.totalReservableMemSlots(), oldNodeMemBuffer, ns.memSlots.Buffer,
		)

		logger.Info(
			"Adding VM pod to node",
			zap.Object("verdict", verdictSet{
				cpu: cpuVerdict,
				mem: memVerdict,
			}),
		)

		ns.pods[podName] = ps
		p.state.podMap[podName] = ps
	}

	// Add the non-VM pods to the map
	logger.Info("Adding non-VM pods to otherPods map")
	skippedOtherPods := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		podName := util.GetNamespacedName(pod)

		if _, isVM := pod.Labels[LabelVM]; isVM {
			continue
		}

		// new logger just for this loop iteration, with info about the Pod
		logger := logger.With(util.PodNameFields(pod))
		if pod.Spec.NodeName != "" {
			logger = logger.With(zap.String("node", pod.Spec.NodeName))
		}

		logSkip := func(format string, args ...any) {
			logger.Warn("Skipping non-VM pod", zap.Error(fmt.Errorf(format, args...)))
			skippedOtherPods += 1
		}

		if util.PodCompleted(pod) {
			logSkip("Pod is in its final, complete state (phase = %q), so will not use any resources", pod.Status.Phase)
			continue
		}

		if _, ok := p.state.podMap[podName]; ok {
			continue
		} else if pod.Spec.SchedulerName != p.state.conf.SchedulerName {
			logSkip("Spec.SchedulerName %q != our config.SchedulerName %q", pod.Spec.SchedulerName, p.state.conf.SchedulerName)
			continue
		}

		if pod.Spec.NodeName == "" {
			logSkip("Spec.NodeName = \"\" (maybe it hasn't been scheduled yet?)")
			continue
		}

		ns, err := p.state.getOrFetchNodeState(ctx, logger, p.metrics, p.nodeStore, pod.Spec.NodeName)
		if err != nil {
			logSkip("Couldn't find Node that non-VM Pod is on (maybe it has since been removed?): %w", err)
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

		cpuVerdict := fmt.Sprintf(
			"pod %v (node %v [%v raw] -> %v [%v raw])",
			&podRes.RawCPU, oldNodeCpuReserved, &oldNodeRes.RawCPU, ns.vCPU.Reserved, &newNodeRes.RawCPU,
		)
		memVerdict := fmt.Sprintf(
			"pod %v (node %v [%v raw] -> %v [%v raw])",
			&podRes.RawMemory, oldNodeMemReserved, &oldNodeRes.RawMemory, ns.memSlots.Reserved, &newNodeRes.RawMemory,
		)

		logger.Info(
			"Adding non-VM pod to node",
			zap.Object("verdict", verdictSet{
				cpu: cpuVerdict,
				mem: memVerdict,
			}),
		)

		ns.otherPods[podName] = ps
		p.state.otherPods[podName] = ps
	}

	// Human-visible sanity checks on item counts:
	logger.Info(fmt.Sprintf(
		"Done loading state, found: %d nodes, %d VMs (%d skipped), %d non-VM pods (%d skipped)",
		len(p.state.nodeMap), len(p.state.podMap), skippedVms, len(p.state.otherPods), skippedOtherPods,
	))

	// At this point, everything's been added to the state. We just need to make sure that we're not
	// over-budget on anything:
	logger.Info("Checking for any over-budget nodes")
	overBudgetCount := 0
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

		if len(overBudget) == 0 {
			continue
		}

		overBudgetCount += 1
		message := overBudget[0]
		if len(overBudget) == 2 {
			message = fmt.Sprintf("%s and %s", overBudget[0], overBudget[1])
		}
		logger.Error("Node is over budget", zap.String("node", nodeName), zap.String("error", message))
	}

	if overBudgetCount != 0 {
		logger.Error(fmt.Sprintf("Found %d nodes over budget", overBudgetCount))
	}

	return nil
}
