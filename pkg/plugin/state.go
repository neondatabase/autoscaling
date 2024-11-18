package plugin

// Definitions and helper functions for managing plugin state

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
	"github.com/neondatabase/autoscaling/pkg/util/xact"
)

// pluginState stores the private state for the plugin, used both within and outside of the
// predefined scheduler plugin points
//
// Accessing the individual fields MUST be done while holding the lock, with some exceptions.
type pluginState struct {
	lock util.ChanMutex

	ongoingMigrationDeletions map[util.NamespacedName]int

	pods  map[util.NamespacedName]*podState
	nodes map[string]*nodeState

	// maxTotalReservableCPU stores the maximum value of any node's totalReservableCPU(), so that we
	// can appropriately scale our scoring
	maxTotalReservableCPU vmapi.MilliCPU
	// maxTotalReservableMem is the same as maxTotalReservableCPU, but for bytes of memory instead
	// of CPU
	maxTotalReservableMem api.Bytes
	// conf stores the current configuration, and is nil if the configuration has not yet been set
	//
	// Proper initialization of the plugin guarantees conf is not nil.
	//
	// conf MAY be accessed without holding the lock; it MUST not be modified.
	conf *Config
}

// nodeState is the information that we track for a particular
type nodeState struct {
	// name is the name of the node, guaranteed by kubernetes to be unique
	name string

	// nodeGroup, if present, gives the node group that this node belongs to.
	nodeGroup string

	// availabilityZone, if present, gives the availability zone that this node is in.
	availabilityZone string

	// cpu tracks the state of vCPU resources -- what's available and how
	cpu nodeResourceState[vmapi.MilliCPU]
	// mem tracks the state of bytes of memory -- what's available and how
	mem nodeResourceState[api.Bytes]

	// pods tracks all the VM pods assigned to this node
	//
	// This includes both bound pods (i.e., pods fully committed to the node) and reserved pods
	// (still may be unreserved)
	pods map[util.NamespacedName]*podState

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
		{"Watermark", s.Watermark},
		{"Reserved", s.Reserved},
		{"Buffer", s.Buffer},
		{"CapacityPressure", s.CapacityPressure},
		{"PressureAccountedFor", s.PressureAccountedFor},
	}
}

func (s *nodeState) updateMetrics(metrics PromMetrics) {
	s.cpu.updateMetrics(metrics.nodeCPUResources, s.name, s.nodeGroup, s.availabilityZone, vmapi.MilliCPU.AsFloat64)
	s.mem.updateMetrics(metrics.nodeMemResources, s.name, s.nodeGroup, s.availabilityZone, api.Bytes.AsFloat64)
}

func (s *nodeResourceState[T]) updateMetrics(
	metric *prometheus.GaugeVec,
	nodeName string,
	nodeGroup string,
	availabilityZone string,
	convert func(T) float64,
) {
	for _, f := range s.fields() {
		metric.WithLabelValues(nodeName, nodeGroup, availabilityZone, f.valueName).Set(convert(f.value))
	}
}

func (s *nodeState) removeMetrics(metrics PromMetrics) {
	gauges := []*prometheus.GaugeVec{metrics.nodeCPUResources, metrics.nodeMemResources}
	fields := s.cpu.fields() // No particular reason to be CPU, we just want the valueNames, and CPU vs memory valueNames are the same

	for _, g := range gauges {
		for _, f := range fields {
			g.DeleteLabelValues(s.name, s.nodeGroup, s.availabilityZone, f.valueName)
		}
	}
}

// nodeResourceState describes the state of a resource allocated to a node
type nodeResourceState[T any] struct {
	// Total is the Total amount of T available on the node. This value does not change.
	Total T `json:"total"`
	// Watermark is the amount of T reserved to pods above which we attempt to reduce usage via
	// migration.
	Watermark T `json:"watermark"`
	// Reserved is the current amount of T reserved to pods. It SHOULD be less than or equal to
	// Total), and we take active measures reduce it once it is above Watermark.
	//
	// Reserved MAY be greater than Total on scheduler restart (because of buffering with VM scaling
	// maximums), but (Reserved - Buffer) MUST be less than Total. In general, (Reserved - Buffer)
	// SHOULD be less than or equal to Total, but this can be temporarily violated after restart.
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

// podState is the information we track for an individual pod, which may or may not be associated
// with a VM
type podState struct {
	// name is the namespace'd name of the pod
	//
	// name will not change after initialization, so it can be accessed without holding a lock.
	name util.NamespacedName

	// node provides information about the node that this pod is bound to or reserved onto.
	node *nodeState

	// cpu is the current state of this pod's vCPU utilization and pressure
	cpu podResourceState[vmapi.MilliCPU]
	// memBytes is the current state of this pod's memory utilization and pressure
	mem podResourceState[api.Bytes]

	// vm stores the extra information associated with VMs
	vm *vmPodState
}

type vmPodState struct {
	// Name is the name of the VM, as given by the owner reference for the VM or VM migration that
	// owns this pod
	Name util.NamespacedName

	// PodCreatedAt is the time at which the pod was created.
	//
	// This is used for prioritization in live migration, where we prioritize pods that have been
	// running for longer, as a way of naturally making sure that individual VMs are not migrated too
	// frequently.
	PodCreatedAt time.Time

	// MemSlotSize stores the value of the VM's .Spec.Guest.MemorySlotSize, for compatibility with
	// earlier versions of the agent<->plugin protocol.
	MemSlotSize api.Bytes

	// Config stores the values of per-VM settings for this VM
	Config api.VmConfig

	// MqIndex stores this pod's index in the migrationQueue. This value is -1 iff metrics is nil or
	// it is currently migrating.
	MqIndex int

	// MigrationState gives current information about an ongoing migration, if this pod is currently
	// migrating.
	MigrationState *podMigrationState
}

// podMigrationState tracks the information about an ongoing VM pod's migration
type podMigrationState struct {
	// Name gives the name of the VirtualMachineMigration that this pod is involved in
	Name util.NamespacedName
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

func (p *podState) kind() string {
	if p.vm != nil {
		return "VM"
	} else {
		return "non-VM"
	}
}

func (p *podState) logFields() []zap.Field {
	podName := zap.Object("pod", p.name)
	if p.vm != nil {
		vmName := zap.Object("virtualmachine", p.vm.Name)
		return []zap.Field{podName, vmName}
	} else {
		return []zap.Field{podName}
	}
}

// remainingReservableCPU returns the remaining CPU that can be allocated to VM pods
func (s *nodeState) remainingReservableCPU() vmapi.MilliCPU {
	return util.SaturatingSub(s.cpu.Total, s.cpu.Reserved)
}

// remainingReservableMem returns the remaining number of bytes of memory that can be allocated to
// VM pods
func (s *nodeState) remainingReservableMem() api.Bytes {
	return util.SaturatingSub(s.mem.Total, s.mem.Reserved)
}

// tooMuchPressure is used to signal whether the node should start migrating pods out in order to
// relieve some of the pressure
func (s *nodeState) tooMuchPressure(logger *zap.Logger) bool {
	if s.cpu.Reserved <= s.cpu.Watermark && s.mem.Reserved < s.mem.Watermark {
		type okPair[T any] struct {
			Reserved  T
			Watermark T
		}

		logger.Debug(
			"tooMuchPressure = false (clearly)",
			zap.Any("cpu", okPair[vmapi.MilliCPU]{Reserved: s.cpu.Reserved, Watermark: s.cpu.Watermark}),
			zap.Any("mem", okPair[api.Bytes]{Reserved: s.mem.Reserved, Watermark: s.mem.Watermark}),
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
	var mem info[api.Bytes]

	cpu.LogicalPressure = util.SaturatingSub(s.cpu.Reserved, s.cpu.Watermark)
	mem.LogicalPressure = util.SaturatingSub(s.mem.Reserved, s.mem.Watermark)

	// Account for existing slack in the system, to counteract capacityPressure that hasn't been
	// updated yet
	cpu.LogicalSlack = s.cpu.Buffer + util.SaturatingSub(s.cpu.Watermark, s.cpu.Reserved)
	mem.LogicalSlack = s.mem.Buffer + util.SaturatingSub(s.mem.Watermark, s.mem.Reserved)

	cpu.TooMuch = cpu.LogicalPressure+s.cpu.CapacityPressure > s.cpu.PressureAccountedFor+cpu.LogicalSlack
	mem.TooMuch = mem.LogicalPressure+s.mem.CapacityPressure > s.mem.PressureAccountedFor+mem.LogicalSlack

	result := cpu.TooMuch || mem.TooMuch

	logger.Debug(
		fmt.Sprintf("tooMuchPressure = %v", result),
		zap.Any("cpu", cpu),
		zap.Any("mem", mem),
	)

	return result
}

func (s *vmPodState) currentlyMigrating() bool {
	return s.MigrationState != nil
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

	if n, ok := s.nodes[nodeName]; ok {
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
		if n, ok := s.nodes[nodeName]; ok {
			logger.Warn("Local information for node became available while waiting on relist, using it instead")
			return n, nil
		}
	}

	n, err := buildInitialNodeState(logger, node, s.conf)
	if err != nil {
		return nil, err
	}

	// update maxTotalReservableCPU and maxTotalReservableMem if there's new maxima
	if n.cpu.Total > s.maxTotalReservableCPU {
		s.maxTotalReservableCPU = n.cpu.Total
	}
	if n.mem.Total > s.maxTotalReservableMem {
		s.maxTotalReservableMem = n.mem.Total
	}

	n.updateMetrics(metrics)

	s.nodes[nodeName] = n
	return n, nil
}

// this method must only be called while holding s.lock. It will not be released during this
// function.
//
// Note: buildInitialNodeState does not take any of the pods or VMs on the node into account; it
// only examines the total resources available to the node.
func buildInitialNodeState(logger *zap.Logger, node *corev1.Node, conf *Config) (*nodeState, error) {
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

	cpu := conf.NodeConfig.vCpuLimits(cpuQ)

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

	mem := conf.NodeConfig.memoryLimits(memQ)

	var nodeGroup string
	if conf.K8sNodeGroupLabel != "" {
		var ok bool
		nodeGroup, ok = node.Labels[conf.K8sNodeGroupLabel]
		if !ok {
			logger.Warn("Node does not have node group label", zap.String("label", conf.K8sNodeGroupLabel))
		}
	}

	var availabilityZone string
	if conf.K8sAvailabilityZoneLabel != "" {
		var ok bool
		availabilityZone, ok = node.Labels[conf.K8sAvailabilityZoneLabel]
		if !ok {
			logger.Warn("Node does not have availability zone label", zap.String("label", conf.K8sAvailabilityZoneLabel))
		}
	}

	n := &nodeState{
		name:             node.Name,
		nodeGroup:        nodeGroup,
		availabilityZone: availabilityZone,
		cpu:              cpu,
		mem:              mem,
		pods:             make(map[util.NamespacedName]*podState),
		mq:               migrationQueue{},
	}

	type resourceInfo[T any] struct {
		Total     T
		Watermark T
	}

	logger.Info(
		"Built initial node state",
		zap.Any("cpu", resourceInfo[vmapi.MilliCPU]{
			Total:     n.cpu.Total,
			Watermark: n.cpu.Watermark,
		}),
		zap.Any("memSlots", resourceInfo[api.Bytes]{
			Total:     n.mem.Total,
			Watermark: n.mem.Watermark,
		}),
	)

	return n, nil
}

func extractPodResources(pod *corev1.Pod) api.Resources {
	var cpu vmapi.MilliCPU
	var mem api.Bytes

	for _, container := range pod.Spec.Containers {
		// For each resource, add the requests, if they're provided. We use this because it matches
		// what cluster-autoscaler uses.
		//
		// NB: .Cpu() returns a pointer to a value equal to zero if the resource is not present. So
		// we can just add it either way.
		cpu += vmapi.MilliCPUFromResourceQuantity(*container.Resources.Requests.Cpu())
		mem += api.BytesFromResourceQuantity(*container.Resources.Requests.Memory())
	}

	return api.Resources{VCPU: cpu, Mem: mem}
}

func (e *AutoscaleEnforcer) handleNodeDeletion(logger *zap.Logger, nodeName string) {
	logger = logger.With(
		zap.String("action", "Node deletion"),
		zap.String("node", nodeName),
	)

	logger.Info("Handling deletion of Node")

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	node, ok := e.state.nodes[nodeName]
	if !ok {
		logger.Warn("Cannot find node in nodeMap")
	}

	if logger.Core().Enabled(zapcore.DebugLevel) {
		logger.Debug("Dump final node state", zap.Any("state", node.dump()))
	}

	// For any pods still on the node, remove them from the global state:
	for name, pod := range node.pods {
		logger.Warn(
			fmt.Sprintf("Found %s pod still on node at time of deletion", pod.kind()),
			pod.logFields()...,
		)
		delete(e.state.pods, name)
	}

	node.removeMetrics(e.metrics)

	delete(e.state.nodes, nodeName)
	logger.Info("Deleted node")
}

// handleStarted updates the state according to a pod that's already started, but may or may not
// have been scheduled via the plugin.
//
// We need to handle this so that we maintain an accurate view of the resource usage in the cluster;
// otherwise, we might (a) ignore resources from pods that weren't scheduled here, or (b) fail to
// include pods that *were* scheduled here, but had spurious Unreserves.
// (for more, see: https://github.com/neondatabase/autoscaling/pull/435)
func (e *AutoscaleEnforcer) handleStarted(logger *zap.Logger, pod *corev1.Pod, preexisting bool) {
	nodeName := pod.Spec.NodeName

	logger = logger.With(
		zap.String("action", "Pod started"),
		zap.String("node", nodeName),
		util.PodNameFields(pod),
	)
	if migrationName := util.TryPodOwnerVirtualMachineMigration(pod); migrationName != nil {
		logger = logger.With(zap.Object("virtualmachinemigration", *migrationName))
	}

	logger.Info("Handling Pod start event")

	_, _, _ = e.reserveResources(context.TODO(), logger, pod, "Pod started", reserveOptions{
		// pod already started, out of our control - we don't have a mechanism to deny it
		allowDeny: false,
		// this may be a preexisting VM. If so, we should include it in "buffer" as long it's
		// supposed to be handled by us (otherwise, the "buffer" will never be resolved)
		includeBuffer: pod.Spec.SchedulerName == e.state.conf.SchedulerName,
		preexisting:   preexisting,
	})
}

type reserveOptions struct {
	allowDeny     bool
	includeBuffer bool
	preexisting   bool
}

// reserveResources attempts to set aside resources on the node for the pod.
//
// If allowDeny is false, reserveResources is not "allowed" to reject the pod if there isn't enough
// room - it must instead set aside resources that don't exist.
//
// If an unexpected error occurs, the first two return values are unspecified, and the error will be
// non-nil. Otherwise, 'ok' will indicate whether the pod was accepted and the verdictSet will
// provide messages describing the result, suitable for being logged.
func (e *AutoscaleEnforcer) reserveResources(
	ctx context.Context,
	logger *zap.Logger,
	pod *corev1.Pod,
	action string,
	opts reserveOptions,
) (ok bool, _ *verdictSet, _ error) {
	nodeName := pod.Spec.NodeName

	if e.state.conf.ignoredNamespace(pod.Namespace) {
		panic(fmt.Errorf("reserveResources called with ignored pod %v", util.GetNamespacedName(pod)))
	}

	vmInfo, err := e.getVmInfo(logger, pod, action)
	if err != nil {
		msg := "Error getting VM info for Pod"
		logger.Error(msg, zap.Error(err))
		return false, nil, fmt.Errorf("%s: %w", msg, err)
	}

	e.state.lock.Lock()
	defer e.state.lock.Unlock()
	podName := util.GetNamespacedName(pod)

	// If the pod already exists, nothing to do
	_, isPodInState := e.state.pods[podName]
	if isPodInState {
		logger.Info("Pod already exists in global state")
		return true, &verdictSet{cpu: "", mem: ""}, nil
	}

	// If the following conditions are met, the pod has bypassed neon scheduler which might be a sign
	// of a bug or misbehavior:
	// - pod is assigned to autoscaler scheduler
	// - pod not in the state
	// - pod is not preexisting pod
	// - pod has the node name
	if !isPodInState && !opts.preexisting && pod.Spec.SchedulerName == e.state.conf.SchedulerName && pod.Spec.NodeName != "" {
		logger.Warn("Pod has bypassed neon scheduler")
	}

	// Get information about the node
	node, err := e.state.getOrFetchNodeState(ctx, logger, e.metrics, e.nodeStore, nodeName)
	if err != nil {
		msg := "Failed to get state for node"
		logger.Error(msg, zap.Error(err))
		return false, nil, fmt.Errorf("%s: %w", msg, err)
	}

	accept := func(verdict verdictSet, overBudget bool) bool {
		shouldDeny := overBudget
		if shouldDeny {
			e.metrics.IncReserveShouldDeny(pod, node)
		}

		if shouldDeny && opts.allowDeny {
			logger.Error(
				"Can't reserve resources for Pod (not enough available)",
				zap.Object("verdict", verdict),
			)
			return false
		}

		if opts.allowDeny {
			logger.Info("Allowing reserve resources for Pod", zap.Object("verdict", verdict))
		} else if shouldDeny /* want to deny, but can't */ {
			logger.Warn("Reserved resources for Pod above totals", zap.Object("verdict", verdict))
		} else /* don't want to deny, but also couldn't if we wanted to */ {
			logger.Info("Reserved resources for Pod", zap.Object("verdict", verdict))
		}
		return true
	}

	ok, verdict := e.speculativeReserve(node, vmInfo, pod, opts.includeBuffer, accept)
	return ok, &verdict, nil
}

// speculativeReserve reserves the pod, and then calls accept() to see whether the pod should
// actually be added.
//
// If accept() returns false, no changes to the state will be made.
func (e *AutoscaleEnforcer) speculativeReserve(
	node *nodeState,
	vmInfo *api.VmInfo,
	pod *corev1.Pod,
	includeBuffer bool,
	accept func(verdict verdictSet, overBudget bool) bool,
) (ok bool, _ verdictSet) {
	// Construct the speculative state of the pod
	//
	// We'll pass this into (resourceTransitioner).handleReserve(), but only commit the changes if
	// the caller allows us to.

	var cpuState podResourceState[vmapi.MilliCPU]
	var memState podResourceState[api.Bytes]
	var vmState *vmPodState

	if vmInfo != nil {
		vmState = &vmPodState{
			Name:           vmInfo.NamespacedName(),
			PodCreatedAt:   pod.CreationTimestamp.Time,
			MemSlotSize:    vmInfo.Mem.SlotSize,
			Config:         vmInfo.Config,
			MqIndex:        -1,
			MigrationState: nil,
		}
		// initially build the resource states assuming that we're including buffer, and then update
		// later to remove it if that turns out not to be right.
		cpuState = podResourceState[vmapi.MilliCPU]{
			Reserved:         vmInfo.Max().VCPU,
			Buffer:           util.SaturatingSub(vmInfo.Max().VCPU, vmInfo.Using().VCPU),
			CapacityPressure: 0,
			Min:              vmInfo.Min().VCPU,
			Max:              vmInfo.Max().VCPU,
		}
		memState = podResourceState[api.Bytes]{
			Reserved:         vmInfo.Max().Mem,
			Buffer:           util.SaturatingSub(vmInfo.Max().Mem, vmInfo.Using().Mem),
			CapacityPressure: 0,
			Min:              vmInfo.Min().Mem,
			Max:              vmInfo.Max().Mem,
		}

		// If scaling isn't enabled *or* the pod is involved in an ongoing migration *or* the caller
		// has opted out of setting Buffer, then we can be more precise about usage.
		//
		// Buffer exists to handle scaling that may happen due to a prior scheduler's approval.
		// If scaling is disabled, we don't have to worry about this, and if there's an ongoing
		// migration, scaling is forbidden.
		migrating := util.TryPodOwnerVirtualMachineMigration(pod) != nil
		if !vmInfo.Config.ScalingEnabled || migrating || !includeBuffer {
			cpuState.Buffer = 0
			cpuState.Reserved = vmInfo.Using().VCPU
			memState.Buffer = 0
			memState.Reserved = vmInfo.Using().Mem
		}
	} else {
		res := extractPodResources(pod)

		cpuState = podResourceState[vmapi.MilliCPU]{
			Reserved:         res.VCPU,
			Buffer:           0,
			CapacityPressure: 0,
			Min:              res.VCPU,
			Max:              res.VCPU,
		}
		memState = podResourceState[api.Bytes]{
			Reserved:         res.Mem,
			Buffer:           0,
			CapacityPressure: 0,
			Min:              res.Mem,
			Max:              res.Mem,
		}
	}
	podName := util.GetNamespacedName(pod)
	ps := &podState{
		name: podName,
		node: node,
		cpu:  cpuState,
		mem:  memState,
		vm:   vmState,
	}

	// Speculatively try reserving the pod.
	nodeXactCPU := xact.New(&node.cpu)
	nodeXactMem := xact.New(&node.mem)

	cpuOverBudget, cpuVerdict := makeResourceTransitioner(nodeXactCPU.Value(), &ps.cpu).handleReserve()
	memOverBudget, memVerdict := makeResourceTransitioner(nodeXactMem.Value(), &ps.mem).handleReserve()

	overBudget := cpuOverBudget || memOverBudget

	verdict := verdictSet{
		cpu: cpuVerdict,
		mem: memVerdict,
	}

	const verdictNotEnough = "NOT ENOUGH"
	const verdictOk = "OK"

	if overBudget {
		cpuShortVerdict := verdictNotEnough
		if !cpuOverBudget {
			cpuShortVerdict = verdictOk
		}
		verdict.cpu = fmt.Sprintf("%s: %s", cpuShortVerdict, verdict.cpu)
		memShortVerdict := verdictNotEnough
		if !memOverBudget {
			memShortVerdict = verdictOk
		}
		verdict.mem = fmt.Sprintf("%s: %s", memShortVerdict, verdict.mem)
	}

	if !accept(verdict, overBudget) {
		return false, verdict
	}

	nodeXactCPU.Commit()
	nodeXactMem.Commit()

	node.pods[podName] = ps
	e.state.pods[podName] = ps

	node.updateMetrics(e.metrics)

	return true, verdict
}

// This method is /basically/ the same as e.Unreserve, but the API is different and it has different
// logs, so IMO it's worthwhile to have this separate.
func (e *AutoscaleEnforcer) handleDeletion(logger *zap.Logger, podName util.NamespacedName) {
	logger = logger.With(
		zap.String("action", "VM deletion"),
		zap.Object("pod", podName),
	)

	logger.Info("Handling deletion of VM pod")

	logFields, kind, migrating, verdict := e.unreserveResources(logger, podName)

	logger.With(logFields...).Info(
		fmt.Sprintf("Deleted %s Pod", kind),
		zap.Bool("migrating", migrating),
		zap.Object("verdict", verdict),
	)
}

// unreserveResources is *essentially* the inverse of reserveResources, but with two main
// differences:
//
//  1. unreserveResources cannot "deny" unreserving, whereas reserveResources may choose whether to
//     accept the additional reservation.
//  2. unreserveResources returns additional information for logging.
//
// Also note that because unreserveResources is expected to be called by the plugin's Unreserve()
// method, it may be called for pods that no longer exist.
func (e *AutoscaleEnforcer) unreserveResources(
	logger *zap.Logger,
	podName util.NamespacedName,
) (_ []zap.Field, kind string, migrating bool, _ verdictSet) {
	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	ps, ok := e.state.pods[podName]
	if !ok {
		logger.Warn("Cannot find Pod in global pods map")
		return
	}
	logFields := []zap.Field{zap.String("node", ps.node.name)}
	if ps.vm != nil {
		logFields = append(logFields, zap.Object("virtualmachine", ps.vm.Name))
	}

	// Mark the resources as no longer reserved
	currentlyMigrating := ps.vm != nil && ps.vm.currentlyMigrating()

	cpuVerdict := makeResourceTransitioner(&ps.node.cpu, &ps.cpu).
		handleDeleted(currentlyMigrating)
	memVerdict := makeResourceTransitioner(&ps.node.mem, &ps.mem).
		handleDeleted(currentlyMigrating)

	// Delete our record of the pod
	delete(e.state.pods, podName)
	delete(ps.node.pods, podName)
	if ps.vm != nil {
		ps.node.mq.removeIfPresent(ps.vm)
	}

	ps.node.updateMetrics(e.metrics)

	return logFields, ps.kind(), currentlyMigrating, verdictSet{cpu: cpuVerdict, mem: memVerdict}
}

func (e *AutoscaleEnforcer) handleVMConfigUpdated(logger *zap.Logger, podName util.NamespacedName, newCfg api.VmConfig) {
	logger = logger.With(
		zap.String("action", "VM config updated"),
		zap.Object("pod", podName),
	)

	logger.Info("Handling updated config for VM pod")

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	ps, ok := e.state.pods[podName]
	if !ok {
		logger.Error("Cannot find Pod in global pods map")
		return
	}
	logger = logger.With(zap.String("node", ps.node.name))
	if ps.vm == nil {
		logger.Error("handleVMConfigUpdated called for non-VM Pod")
		return
	}
	logger = logger.With(zap.Object("virtualmachine", ps.vm.Name))

	// Broadly, we want to update the value of the vmPodState.Config field.
	// But *also*, if autoscaling is newly disabled, we should update update the pod/node state.
	// And if auto-migration is disabled, we should remove the VM from the migration queue.

	oldCfg := ps.vm.Config
	ps.vm.Config = newCfg

	// worth logging all of this in case we hit issues.
	logger.Info("Config updated for VM", zap.Any("oldCfg", newCfg), zap.Any("newCfg", newCfg))

	if oldCfg.AutoMigrationEnabled && !newCfg.AutoMigrationEnabled {
		ps.node.mq.removeIfPresent(ps.vm)
	}

	if oldCfg.ScalingEnabled && !newCfg.ScalingEnabled {
		cpuVerdict := makeResourceTransitioner(&ps.node.cpu, &ps.cpu).
			handleAutoscalingDisabled()
		memVerdict := makeResourceTransitioner(&ps.node.mem, &ps.mem).
			handleAutoscalingDisabled()

		ps.node.updateMetrics(e.metrics)

		logger.Info(
			"Disabled autoscaling for VM pod",
			zap.Object("verdict", verdictSet{
				cpu: cpuVerdict,
				mem: memVerdict,
			}),
		)
	}
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

	ps, ok := e.state.pods[podName]
	if !ok {
		logger.Error("Cannot find Pod in global pods map")
		return
	}
	logger = logger.With(zap.String("node", ps.node.name))
	if ps.vm == nil {
		logger.Error("handlePodStartMigration called for non-VM Pod")
		return
	}
	logger = logger.With(zap.Object("virtualmachine", ps.vm.Name))

	// Reset buffer to zero, remove from migration queue (if in it), and set pod's migrationState
	cpuVerdict := makeResourceTransitioner(&ps.node.cpu, &ps.cpu).
		handleStartMigration(source)
	memVerdict := makeResourceTransitioner(&ps.node.mem, &ps.mem).
		handleStartMigration(source)

	ps.node.mq.removeIfPresent(ps.vm)
	ps.vm.MigrationState = &podMigrationState{Name: migrationName}

	ps.node.updateMetrics(e.metrics)

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
		zap.Object("pod", podName),
		zap.Object("virtualmachinemigration", migrationName),
	)

	logger.Info("Handling VM pod migration end")

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	ps, ok := e.state.pods[podName]
	if !ok {
		logger.Error("Cannot find Pod in global pods map")
		return
	}
	logger = logger.With(zap.String("node", ps.node.name))
	if ps.vm == nil {
		logger.Error("handlePodEndMigration called for non-VM Pod")
		return
	}
	logger = logger.With(zap.Object("virtualmachine", ps.vm.Name))

	ps.vm.MigrationState = nil

	//nolint:gocritic // NOTE: not *currently* needed, but this should be kept here as a reminder, in case that changes.
	// ps.node.updateMetrics(e.metrics)

	logger.Info("Recorded end of migration for VM pod")
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

	ps, ok := e.state.pods[podName]
	if !ok {
		logger.Error("Cannot find Pod in global pods map")
		return
	}
	logger = logger.With(zap.String("node", ps.node.name))
	if ps.vm == nil {
		logger.Error("handleUpdatedScalingBounds called for non-VM Pod")
		return
	}

	cpuVerdict := handleUpdatedLimits(&ps.node.cpu, &ps.cpu, vm.Cpu.Min, vm.Cpu.Max)
	memVerdict := handleUpdatedLimits(&ps.node.mem, &ps.mem, vm.Min().Mem, vm.Max().Mem)

	ps.node.updateMetrics(e.metrics)

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

	ps, ok := e.state.pods[podName]
	if !ok {
		logger.Error("Cannot find Pod in global pods map")
		return
	}

	cpuVerdict := makeResourceTransitioner(&ps.node.cpu, &ps.cpu).
		handleNonAutoscalingUsageChange(vm.Using().VCPU)
	memVerdict := makeResourceTransitioner(&ps.node.mem, &ps.mem).
		handleNonAutoscalingUsageChange(vm.Using().Mem)

	ps.node.updateMetrics(e.metrics)

	logger.Info(
		"Updated non-autoscaling VM usage",
		zap.Object("verdict", verdictSet{
			cpu: cpuVerdict,
			mem: memVerdict,
		}),
	)
}

// NB: expected to be run in its own thread.
func (e *AutoscaleEnforcer) cleanupMigration(logger *zap.Logger, vmm *vmapi.VirtualMachineMigration) {
	vmmName := util.GetNamespacedName(vmm)

	logger = logger.With(
		// note: use the "virtualmachinemigration" key here for just the name, because it mirrors
		// what we log in startMigration.
		zap.Object("virtualmachinemigration", vmmName),
		// also include the VM, for better association.
		zap.Object("virtualmachine", util.NamespacedName{
			Name:      vmm.Spec.VmName,
			Namespace: vmm.Namespace,
		}),
	)
	// Failed migrations should be noisy. Everything to do with cleaning up a failed migration
	// should be logged at "Warn" or higher.
	var logInfo func(string, ...zap.Field)
	if vmm.Status.Phase == vmapi.VmmSucceeded {
		logInfo = logger.Info
	} else {
		logInfo = logger.Warn
	}
	logInfo(
		"Going to delete VirtualMachineMigration",
		// Explicitly include "phase" here because we have metrics for it.
		zap.String("phase", string(vmm.Status.Phase)),
		// ... and then log the rest of the information about the migration:
		zap.Any("spec", vmm.Spec),
		zap.Any("status", vmm.Status),
	)

	// mark the operation as ongoing
	func() {
		e.state.lock.Lock()
		defer e.state.lock.Unlock()

		newCount := e.state.ongoingMigrationDeletions[vmmName] + 1
		if newCount != 1 {
			// context included by logger
			logger.Error(
				"More than one ongoing deletion for VirtualMachineMigration",
				zap.Int("count", newCount),
			)
		}
		e.state.ongoingMigrationDeletions[vmmName] = newCount
	}()
	// ... and remember to clean up when we're done:
	defer func() {
		e.state.lock.Lock()
		defer e.state.lock.Unlock()

		newCount := e.state.ongoingMigrationDeletions[vmmName] - 1
		if newCount == 0 {
			delete(e.state.ongoingMigrationDeletions, vmmName)
		} else {
			// context included by logger
			logger.Error(
				"More than one ongoing deletion for VirtualMachineMigration",
				zap.Int("count", newCount),
			)
			e.state.ongoingMigrationDeletions[vmmName] = newCount
		}
	}()

	// Continually retry the operation, until we're successful (or the VM doesn't exist anymore)

	retryWait := time.Second * time.Duration(e.state.conf.MigrationDeletionRetrySeconds)

	for {
		logInfo("Attempting to delete VirtualMachineMigration")
		err := e.vmClient.NeonvmV1().
			VirtualMachineMigrations(vmmName.Namespace).
			Delete(context.TODO(), vmmName.Name, metav1.DeleteOptions{})
		if err == nil /* NB! This condition is inverted! */ {
			logInfo("Successfully deleted VirtualMachineMigration")
			e.metrics.migrationDeletions.WithLabelValues(string(vmm.Status.Phase)).Inc()
			return
		} else if apierrors.IsNotFound(err) {
			logger.Warn("Deletion was handled for us; VirtualMachineMigration no longer exists")
			return
		}

		logger.Error(
			"Failed to delete VirtualMachineMigration, will try again after delay",
			zap.Duration("delay", retryWait),
			zap.Error(err),
		)
		e.metrics.migrationDeleteFails.WithLabelValues(string(vmm.Status.Phase)).Inc()

		// retry after a delay
		time.Sleep(retryWait)
		continue
	}
}

func (s *vmPodState) isBetterMigrationTarget(other *vmPodState) bool {
	// TODO - this is just a first-pass approximation. Maybe it's ok for now? Maybe it's not. Idk.
	return s.PodCreatedAt.Before(other.PodCreatedAt)
}

// this method can only be called while holding a lock. It will be released temporarily while we
// send requests to the API server
//
// A lock will ALWAYS be held on return from this function.
func (e *AutoscaleEnforcer) startMigration(ctx context.Context, logger *zap.Logger, pod *podState) (created bool, _ error) {
	if pod.vm.currentlyMigrating() {
		return false, fmt.Errorf("Pod is already migrating")
	}

	// Unlock to make the API request(s), then make sure we're locked on return.
	e.state.lock.Unlock()
	defer e.state.lock.Lock()

	vmmName := util.NamespacedName{
		Name:      fmt.Sprintf("schedplugin-%s", pod.vm.Name.Name),
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
	_, err := e.vmClient.NeonvmV1().
		VirtualMachineMigrations(pod.name.Namespace).
		Get(ctx, vmmName.Name, metav1.GetOptions{})
	if err == nil {
		logger.Warn("VirtualMachineMigration already exists, nothing to do")
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		// We're *expecting* to get IsNotFound = true; if err != nil and isn't NotFound, then
		// there's some unexpected error.
		logger.Error("Unexpected error doing Get request to check if migration already exists", zap.Error(err))
		return false, fmt.Errorf("Error checking if migration exists: %w", err)
	}

	vmm := &vmapi.VirtualMachineMigration{
		ObjectMeta: metav1.ObjectMeta{
			// TODO: it's maybe possible for this to run into name length limits? Unclear what we
			// should do if that happens.
			Name:      vmmName.Name,
			Namespace: pod.name.Namespace,
			Labels: map[string]string{
				LabelPluginCreatedMigration: "true",
			},
		},
		Spec: vmapi.VirtualMachineMigrationSpec{
			VmName: pod.vm.Name.Name,

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
	_, err = e.vmClient.NeonvmV1().VirtualMachineMigrations(pod.name.Namespace).Create(ctx, vmm, metav1.CreateOptions{})
	if err != nil {
		e.metrics.migrationCreateFails.Inc()
		// log here, while the logger's fields are in scope
		logger.Error("Unexpected error doing Create request for new migration", zap.Error(err))
		return false, fmt.Errorf("Error creating migration: %w", err)
	}
	e.metrics.migrationCreations.Inc()
	logger.Info("VM migration request successful")

	return true, nil
}
