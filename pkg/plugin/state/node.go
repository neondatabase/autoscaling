package state

import (
	"errors"
	"fmt"
	"iter"

	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/constraints"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type Node struct {
	Name string

	Labels *XactMap[string, string]

	// pods stores the set of pods on the node.
	//
	// NOTE: It's important that this is a map of Pod and not *Pod, because it means that
	// speculative changes can't leak through to changes on the underlying Pods.
	pods *XactMap[types.UID, Pod]

	// migratablePods stores the UIDs of pods that are currently migratable. This is to allow more
	// efficiently fetching candidate pods to migrate, so that we don't perform a lot of unnecessary
	// work when nodes are over-full but don't have any migratable pods.
	migratablePods *XactMap[types.UID, struct{}]

	CPU NodeResources[vmv1.MilliCPU]
	Mem NodeResources[api.Bytes]
}

// MarshalLogObject implements zapcore.ObjectMarshaler so that Node can be used with zap.Object
// without emitting large lists of pods.
func (n Node) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("Name", n.Name)
	err := enc.AddObject("Labels", zapcore.ObjectMarshalerFunc(func(e zapcore.ObjectEncoder) error {
		for label, value := range n.Labels.Entries() {
			e.AddString(label, value)
		}
		return nil
	}))
	if err != nil {
		return err
	}
	if err := enc.AddReflected("CPU", n.CPU); err != nil {
		return err
	}
	if err := enc.AddReflected("Mem", n.Mem); err != nil {
		return err
	}
	return nil
}

type NodeResources[T constraints.Unsigned] struct {
	// Total is the total amount of T available on the node.
	//
	// This value does not change.
	Total T

	// Reserved is exactly equal to all Pods' <resource>.Reserved values.
	//
	// It SHOULD be less than or equal to Total, and - when live migration is enabled - we take
	// active measures to reduce it once it is above Watermark.
	//
	// Reserved can be greater than Total if:
	// * There is misbehavior between the autoscaler-agent and scheduler plugin;
	// * Eventual consistency causes us to operate on stale data; or
	// * Other pods are scheduled without going through our scheduler plugin
	Reserved T

	// Migrating is the amount of T that we expect will be removed by ongoing live migration.
	Migrating T

	// Watermark is the amount of T reserved to pods above which we attempt to reduce usage via
	// migration.
	//
	// This value does not change.
	Watermark T
}

type NodeResourceField[T any] struct {
	Name  string
	Value T
}

func (r NodeResources[T]) Fields() []NodeResourceField[T] {
	return []NodeResourceField[T]{
		{"Total", r.Total},
		{"Reserved", r.Reserved},
		{"Migrating", r.Migrating},
		{"Watermark", r.Watermark},
	}
}

func NodeStateFromK8sObj(
	node *corev1.Node,
	watermarkFraction float64,
	keepLabels []string,
) (*Node, error) {
	// Note that node.Status.Allocatable has the following docs:
	//
	//   "Allocatable represents the resources of a node that are available for scheduling. Defaults
	//   to Capacity."
	//
	// So we should be able to assume that the resources exist there.

	// cpuQ is the CPU amount as a k8s resource.Quantity
	cpuQ := node.Status.Allocatable.Cpu()
	if cpuQ == nil {
		return nil, errors.New("Node hsa no Allocatable CPU limit")
	}
	totalCPU := vmv1.MilliCPUFromResourceQuantity(*cpuQ)

	memQ := node.Status.Allocatable.Memory()
	if memQ == nil {
		return nil, errors.New("Node has no Allocatable Memory limit")
	}
	totalMem := api.BytesFromResourceQuantity(*memQ)

	labels := make(map[string]string)
	for _, lbl := range keepLabels {
		labels[lbl] = node.Labels[lbl]
	}

	return NodeStateFromParams(node.Name, totalCPU, totalMem, watermarkFraction, labels), nil
}

// NodeStateFromParams is a helper to construct a *Node, primarily for use in tests.
//
// For practical usage, see NodeStateFromK8sObj.
func NodeStateFromParams(
	name string,
	totalCPU vmv1.MilliCPU,
	totalMem api.Bytes,
	watermarkFraction float64,
	labels map[string]string,
) *Node {
	return &Node{
		Name: name,
		Labels: func() *XactMap[string, string] {
			m := NewXactMap[string, string]()
			for k, v := range labels {
				m.Set(k, v)
			}
			return m
		}(),
		pods:           NewXactMap[types.UID, Pod](),
		migratablePods: NewXactMap[types.UID, struct{}](),
		CPU: NodeResources[vmv1.MilliCPU]{
			Total:     totalCPU,
			Reserved:  0,
			Migrating: 0,
			Watermark: vmv1.MilliCPU(float64(totalCPU) * watermarkFraction),
		},
		Mem: NodeResources[api.Bytes]{
			Total:     totalMem,
			Reserved:  0,
			Migrating: 0,
			Watermark: api.Bytes(float64(totalMem) * watermarkFraction),
		},
	}
}

// OverBudget returns whether this node has more resources reserved than in total
func (n *Node) OverBudget() bool {
	return n.CPU.Reserved > n.CPU.Total || n.Mem.Reserved > n.Mem.Total
}

// Speculatively allows attempting a modification to the node before deciding whether to actually
// commit that change.
//
// Any of the fields of the node can be updated, including its pods.
func (n *Node) Speculatively(modify func(n *Node) (commit bool)) (committed bool) {
	tmp := &Node{
		Name:           n.Name,
		Labels:         n.Labels.NewTransaction(),
		pods:           n.pods.NewTransaction(),
		migratablePods: n.migratablePods.NewTransaction(),
		CPU:            n.CPU,
		Mem:            n.Mem,
	}
	commit := modify(tmp)
	if commit {
		tmp.Labels.Commit()
		tmp.pods.Commit()
		tmp.migratablePods.Commit()
		n.CPU = tmp.CPU
		n.Mem = tmp.Mem
	}
	return commit
}

// Update sets the resource state of the node, corresponding to the changes in the totals present as
// part of newState.
//
// GENERALLY there should be no change here, but if something happens, it's better to accept the
// change and continue rather than to operate on stale data.
func (n *Node) Update(newState *Node) (changed bool) {
	if n.Name != newState.Name {
		panic(fmt.Sprintf("Node name changed from %q to %q", n.Name, newState.Name))
	}

	changed = newState.CPU.Total != n.CPU.Total || newState.Mem.Total != n.Mem.Total ||
		newState.CPU.Watermark != n.CPU.Watermark || newState.Mem.Watermark != n.Mem.Watermark

	// Propagate changes to labels:
	for label, value := range n.Labels.Entries() {
		v, ok := n.Labels.Get(label)
		if !ok || v != value {
			n.Labels.Set(label, value)
			changed = true
		}
	}
	for label := range n.Labels.Entries() {
		// remove labels that no longer exist
		if _, ok := newState.Labels.Get(label); !ok {
			n.Labels.Delete(label)
			changed = true
		}
	}

	if !changed {
		return
	}

	*n = Node{
		Name:           n.Name,
		Labels:         n.Labels,
		pods:           n.pods,
		migratablePods: n.migratablePods,
		CPU: NodeResources[vmv1.MilliCPU]{
			Total:     newState.CPU.Total,
			Reserved:  n.CPU.Reserved,
			Migrating: n.CPU.Migrating,
			Watermark: newState.CPU.Watermark,
		},
		Mem: NodeResources[api.Bytes]{
			Total:     newState.Mem.Total,
			Reserved:  n.Mem.Reserved,
			Migrating: n.Mem.Migrating,
			Watermark: newState.Mem.Watermark,
		},
	}

	return
}

// GetPod returns a copy of the state for the Pod with the given UID, or false if no such Pod is
// present in the Node's state.
func (n *Node) GetPod(uid types.UID) (_ Pod, ok bool) {
	return n.pods.Get(uid)
}

// Pods returns an iterator over all pods on the node.
func (n *Node) Pods() iter.Seq2[types.UID, Pod] {
	return n.pods.Entries()
}

// MigratablePods returns an iterator through the migratable pods on the node.
//
// This method is provided as a specialized version of (*Node).Pods() in order to support more
// efficient look-ups when trying to balance nodes.
func (n *Node) MigratablePods() iter.Seq2[types.UID, Pod] {
	return func(yield func(types.UID, Pod) bool) {
		for uid := range n.migratablePods.Entries() {
			pod, ok := n.pods.Get(uid)
			if !ok {
				panic(fmt.Sprintf("pod with UID %s preset in migratablePods map but not pods map", uid))
			}

			if !yield(uid, pod) {
				break
			}
		}
	}
}

// AddPod adds a pod to the node, updating resources as required.
func (n *Node) AddPod(pod Pod) {
	if _, ok := n.pods.Get(pod.UID); ok {
		panic("cannot add Pod that already exists")
	}

	n.CPU.add(&pod.CPU, pod.Migrating)
	n.Mem.add(&pod.Mem, pod.Migrating)
	n.pods.Set(pod.UID, pod)
	if pod.Migratable {
		n.migratablePods.Set(pod.UID, struct{}{})
	}
}

// UpdatePod updates the node based on the change in the pod from old to new.
//
// NOTE: this DOES NOT make any follow-up changes -- e.g., updating the reserved resources to match
// what was requested. Those must be done by a call to Reconcile().
func (n *Node) UpdatePod(oldPod, newPod Pod) (changed bool) {
	// Some safety checks:
	if oldPod.UID != newPod.UID {
		panic(fmt.Sprintf("Pod UID changed from %q to %q", oldPod.UID, newPod.UID))
	} else if oldPod.NamespacedName != newPod.NamespacedName {
		panic(fmt.Sprintf("Pod name changed from %v to %v", oldPod.NamespacedName, newPod.NamespacedName))
	} else if newPod.Migrating && !newPod.Migratable {
		panic("new pod state is migrating but not migratable")
	} else if _, ok := n.pods.Get(oldPod.UID); !ok {
		panic("cannot update Pod that doesn't exist in the node state")
	}

	// remove the old pod; replace it with the new one! Simple as.
	n.RemovePod(oldPod.UID)
	n.AddPod(newPod)

	return oldPod != newPod
}

// ReconcilePodReserved will make all possible progress on updating the Pod's reserved resources
// based on the what's requested for the pod and what's available.
//
// Currently, that's just updating the reserved resources to match what's requested (or, as much as
// is possible).
//
// The new values of the Pod's resources will be left in the provided Pod.
//
// This method will misbehave if the values of the Pod do not match the value of what's stored for
// that Pod in the node.
func (n *Node) ReconcilePodReserved(pod *Pod) (done bool) {
	if _, ok := n.pods.Get(pod.UID); !ok {
		panic("cannot reconcile reserved resources for Pod that doesn't exist in the node state")
	}

	cpuDone := n.CPU.reconcilePod(&pod.CPU, pod.Migrating)
	memDone := n.Mem.reconcilePod(&pod.Mem, pod.Migrating)
	n.pods.Set(pod.UID, *pod)

	return cpuDone && memDone
}

// RemovePod removes the pod from the node, given its UID, returning true iff the pod existed on the
// node.
//
// Resources are updated as required.
func (n *Node) RemovePod(uid types.UID) (exists bool) {
	pod, ok := n.pods.Get(uid)
	if !ok {
		return false
	}

	n.pods.Delete(uid)
	n.migratablePods.Delete(uid)
	n.CPU.remove(pod.CPU, pod.Migrating)
	n.Mem.remove(pod.Mem, pod.Migrating)
	return true
}

func (r *NodeResources[T]) add(p *PodResources[T], migrating bool) {
	r.Reserved += p.Reserved
	if migrating {
		r.Migrating += p.Reserved
	}
}

func (r *NodeResources[T]) remove(p PodResources[T], migrating bool) {
	r.Reserved -= p.Reserved
	if migrating {
		r.Migrating -= p.Reserved
	}
}

func (r *NodeResources[T]) reconcilePod(p *PodResources[T], migrating bool) (done bool) {
	if p.Requested == p.Reserved {
		return true // nothing to do!
	}

	if p.Requested < p.Reserved {
		// Easy enough - we can just make the reduction.
		r.remove(*p, migrating)
		p.Reserved = p.Requested
		r.add(p, migrating)
		return true // nothing to do!
	}

	// Difficult case: Requested is greater than Reserved -- how much can we give?
	desiredIncrease := p.Requested - p.Reserved
	remaining := util.SaturatingSub(r.Total, r.Reserved)

	// (X / M) * M is equivalent to floor(X / M) -- any amount that we give must be a multiple of
	// the factor (roughly, the compute unit).
	maxIncrease := (remaining / p.Factor) * p.Factor

	actualIncrease := min(maxIncrease, desiredIncrease)
	if actualIncrease != 0 {
		r.remove(*p, migrating)
		p.Reserved += actualIncrease
		r.add(p, migrating)
	}
	// We're done iff everything that was asked for has been granted
	return p.Reserved == p.Requested
}

// UnmigratedAboveWatermark returns the amount of T above Watermark that isn't already being
// migrated.
//
// This method will panic if r.Migrating is greater than r.Reserved.
func (r NodeResources[T]) UnmigratedAboveWatermark() T {
	if r.Migrating > r.Reserved {
		panic(fmt.Sprintf(
			"unexpectedly migrating more resources than are reserved: %v > %v",
			r.Migrating, r.Reserved,
		))
	}

	unmigrating := r.Reserved - r.Migrating
	return util.SaturatingSub(unmigrating, r.Watermark)
}
