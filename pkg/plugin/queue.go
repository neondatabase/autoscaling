package plugin

// Implementation of a metrics-based migration priority queue over vmPodStates

import (
	"container/heap"
)

type migrationQueue []*vmPodState

///////////////////////
// package-local API //
///////////////////////

func (mq *migrationQueue) addOrUpdate(vm *vmPodState) {
	if vm.MqIndex == -1 {
		heap.Push(mq, vm)
	} else {
		heap.Fix(mq, vm.MqIndex)
	}
}

func (mq migrationQueue) isNextInQueue(vm *vmPodState) bool {
	// the documentation for heap.Pop says that it's equivalent to heap.Remove(h, 0). Therefore,
	// checking whether something's the next pop target can just be done by checking if its index is
	// zero.
	return vm.MqIndex == 0
}

func (mq *migrationQueue) removeIfPresent(vm *vmPodState) {
	if vm.MqIndex != -1 {
		_ = heap.Remove(mq, vm.MqIndex)
	}
}

//////////////////////////////////////
// container/heap.Interface methods //
//////////////////////////////////////

func (mq migrationQueue) Len() int { return len(mq) }

func (mq migrationQueue) Less(i, j int) bool {
	return mq[i].isBetterMigrationTarget(mq[j])
}

func (mq migrationQueue) Swap(i, j int) {
	mq[i], mq[j] = mq[j], mq[i]
	mq[i].MqIndex = i
	mq[j].MqIndex = j
}

func (mq *migrationQueue) Push(v any) {
	n := len(*mq)
	vm := v.(*vmPodState)
	vm.MqIndex = n
	*mq = append(*mq, vm)
}

func (mq *migrationQueue) Pop() any {
	// Function body + comments taken from the example at https://pkg.go.dev/container/heap
	old := *mq
	n := len(old)
	vm := old[n-1]
	old[n-1] = nil  // avoid memory leak
	vm.MqIndex = -1 // for safety
	*mq = old[0 : n-1]
	return vm
}
