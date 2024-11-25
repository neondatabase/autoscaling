package billing

// Types and implementation relating to VMNodeIndex, which provides indexing for watch.Watch for
// efficient lookup of VMs on a particular node.

import (
	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

type VMStoreForNode = watch.IndexedStore[vmv1.VirtualMachine, *VMNodeIndex]

// VMNodeIndex is a watch.Index that stores all of the VMs for a particular node
//
// We have to implement this ourselves because K8s does not (as of 2023-04-04) support field
// selectors on CRDs, so we can't have the API server filter out VMs for us.
//
// For more info, see: https://github.com/kubernetes/kubernetes/issues/53459
// This comment in particular was particularly instructive:
// https://github.com/kubernetes/kubernetes/issues/53459#issuecomment-1146200268
type VMNodeIndex struct {
	forNode map[types.UID]*vmv1.VirtualMachine
	node    string
}

func NewVMNodeIndex(node string) *VMNodeIndex {
	return &VMNodeIndex{
		forNode: make(map[types.UID]*vmv1.VirtualMachine),
		node:    node,
	}
}

func (i *VMNodeIndex) Add(vm *vmv1.VirtualMachine) {
	if vm.Status.Node == i.node {
		i.forNode[vm.UID] = vm
	}
}

func (i *VMNodeIndex) Update(oldVM, newVM *vmv1.VirtualMachine) {
	i.Delete(oldVM)
	i.Add(newVM)
}

func (i *VMNodeIndex) Delete(vm *vmv1.VirtualMachine) {
	// note: delete is a no-op if the key isn't present.
	delete(i.forNode, vm.UID)
}

func (i *VMNodeIndex) List() []*vmv1.VirtualMachine {
	items := make([]*vmv1.VirtualMachine, 0, len(i.forNode))
	for _, vm := range i.forNode {
		items = append(items, vm)
	}
	return items
}
