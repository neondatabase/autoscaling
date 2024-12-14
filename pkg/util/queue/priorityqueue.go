package queue

// Wrapper around Go's container/heap so that it works with generic types

import (
	"container/heap"
)

// PriorityQueue is a generic priority queue of type T, given a function to determine when a value
// should be given sooner.
type PriorityQueue[T any] struct {
	inner *innerQueue[T]
}

type innerQueue[T any] struct {
	values []*item[T]
	less   func(T, T) bool
}

// ItemHandle is a stable reference to a particular value in the queue
type ItemHandle[T any] struct {
	queue *innerQueue[T]
	item  *item[T]
}

type item[T any] struct {
	v     T
	index int
}

// New creates a new queue, given a function that returns if a value should be returned sooner than
// another.
func New[T any](sooner func(T, T) bool) PriorityQueue[T] {
	return PriorityQueue[T]{
		inner: &innerQueue[T]{
			values: []*item[T]{},
			less:   sooner,
		},
	}
}

// Len returns the length of the queue
func (q PriorityQueue[T]) Len() int {
	return q.inner.Len()
}

// Push adds the value to the queue and returns a stable handle to it
func (q PriorityQueue[T]) Push(value T) ItemHandle[T] {
	item := &item[T]{
		v:     value,
		index: -1,
	}
	heap.Push(q.inner, item)
	return ItemHandle[T]{
		queue: q.inner,
		item:  item,
	}
}

// Peek returns the value at the front of the queue without removing it, returning false iff the
// queue is empty.
func (p PriorityQueue[T]) Peek() (_ T, ok bool) {
	var empty T
	if p.Len() == 0 {
		return empty, false
	}
	// container/heap guarantees that the value at index 0 is the one that will be returned by Pop.
	// See the docs for Pop -- "Pop is equivalent to Remove(h, 0)."
	return p.inner.values[0].v, true
}

// Pop returns a value from the queue, returning false iff the queue is empty
func (q PriorityQueue[T]) Pop() (_ T, ok bool) {
	if q.inner.Len() == 0 {
		var empty T
		return empty, false
	}
	item := heap.Pop(q.inner).(*item[T])
	return item.v, true
}

// Value returns the value associated with the item
//
// NOTE: Any updates to the value here will not be reflected by changing its position in the queue.
// For that, you must use (ItemHandle[T]).Update().
func (it ItemHandle[T]) Value() T {
	return it.item.v
}

// Update sets the value of this item, updating its position in the queue accordingly
func (it ItemHandle[T]) Update(update func(value *T)) {
	if it.item.index == -1 {
		panic("item has since been removed from the queue")
	}

	update(&it.item.v)
	heap.Fix(it.queue, it.item.index)
}

///////////////////////////////////////////////////////////
//      INTERNAL METHODS, FOR container/heap TO USE      //
///////////////////////////////////////////////////////////

// Len implements heap.Interface
func (q *innerQueue[T]) Len() int {
	return len(q.values)
}

// Less implements heap.Interface
func (q *innerQueue[T]) Less(i, j int) bool {
	return q.less(q.values[i].v, q.values[j].v)
}

// Swap implements heap.Interface
func (q *innerQueue[T]) Swap(i, j int) {
	// copied from the example in container/heap
	q.values[i], q.values[j] = q.values[j], q.values[i]
	q.values[i].index = i
	q.values[j].index = j
}

// Push implements heap.Interface
func (q *innerQueue[T]) Push(x any) {
	// copied from the example in container/heap
	n := len(q.values)
	item := x.(*item[T])
	item.index = n
	q.values = append(q.values, item)
}

// Pop implements heap.Interface
func (q *innerQueue[T]) Pop() any {
	// copied from the example in container/heap
	old := q.values
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // don't stop the GC from reclaiming the item eventually
	item.index = -1 // for safety
	q.values = old[0 : n-1]
	return item
}
