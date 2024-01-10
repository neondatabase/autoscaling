package billing

// Types and implementation relating to VMNodeIndex, which provides indexing for watch.Watch for
// efficient lookup of VMs on a particular node.

import (
	"k8s.io/apimachinery/pkg/types"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

type VMStoreForNode = watch.IndexedStore[vmapi.VirtualMachine, *VMNodeIndex]

// VMNodeIndex is a watch.Index that stores all of the VMs for a particular node
//
// We have to implement this ourselves because K8s does not (as of 2023-04-04) support field
// selectors on CRDs, so we can't have the API server filter out VMs for us.
//
// For more info, see: https://github.com/kubernetes/kubernetes/issues/53459
// This comment in particular was particularly instructive:
// https://github.com/kubernetes/kubernetes/issues/53459#issuecomment-1146200268
type VMNodeIndex struct {
	forNode map[types.UID]*vmapi.VirtualMachine
	node    string
}

func NewVMNodeIndex(node string) *VMNodeIndex {
	return &VMNodeIndex{
		forNode: make(map[types.UID]*vmapi.VirtualMachine),
		node:    node,
	}
}

func (i *VMNodeIndex) Add(vm *vmapi.VirtualMachine) {
	if vm.Status.Node == i.node {
		i.forNode[vm.UID] = vm
	}
}
func (i *VMNodeIndex) Update(oldVM, newVM *vmapi.VirtualMachine) {
	i.Delete(oldVM)
	i.Add(newVM)
}
func (i *VMNodeIndex) Delete(vm *vmapi.VirtualMachine) {
	// note: delete is a no-op if the key isn't present.
	delete(i.forNode, vm.UID)
}

func (i *VMNodeIndex) List() []*vmapi.VirtualMachine {
	items := make([]*vmapi.VirtualMachine, 0, len(i.forNode))
	for _, vm := range i.forNode {
		items = append(items, vm)
	}
	return items
}
