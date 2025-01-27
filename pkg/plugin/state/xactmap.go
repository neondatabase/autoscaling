package state

import (
	"iter"
)

// XactMap is a map with support for transactions.
type XactMap[K comparable, V any] struct {
	parent *XactMap[K, V]

	newObjs map[K]V
	deletes map[K]struct{}
}

func NewXactMap[K comparable, V any]() *XactMap[K, V] {
	return &XactMap[K, V]{
		parent:  nil,
		newObjs: make(map[K]V),
		deletes: make(map[K]struct{}),
	}
}

// NewTransaction creates a new XactMap that acts as a shallow copy of the parent -- any changes in
// the child will not affect the parent until a call to Commit(), if desired.
//
// NOTE: Once you have already made some changes to the child, it is unsound to make changes to the
// parent and then continue using the child.
func (m *XactMap[K, V]) NewTransaction() *XactMap[K, V] {
	return &XactMap[K, V]{
		parent:  m,
		newObjs: make(map[K]V),
		deletes: make(map[K]struct{}),
	}
}

// Commit propagates all changes from this local XactMap into its parent.
//
// Afterwards, this map can continue to be used as normal, if you want.
func (m *XactMap[K, V]) Commit() {
	if m.parent == nil {
		panic("(*XactMap).Commit() called with nil parent")
	}

	for k := range m.deletes {
		m.parent.Delete(k)
	}
	for k, v := range m.newObjs {
		m.parent.Set(k, v)
	}

	// clean up our maps to make this safe for potential reuse.
	clear(m.newObjs)
	clear(m.deletes)
}

// Get returns the value for the key if it's present in the map, else (zero, false).
func (m *XactMap[K, V]) Get(key K) (V, bool) {
	var emptyValue V

	// Value is overridden here:
	if v, ok := m.newObjs[key]; ok {
		return v, ok
	}
	// Value is deleted here:
	if _, ok := m.deletes[key]; ok {
		return emptyValue, false
	}
	// fall through to the parent:
	if m.parent != nil {
		return m.parent.Get(key)
	}
	// otherwise, nothing.
	return emptyValue, false
}

func (m *XactMap[K, V]) Set(key K, value V) {
	m.newObjs[key] = value

	// un-delete the key, if necessary:
	delete(m.deletes, key)
}

// Delete removes the key from the map, if it's present.
func (m *XactMap[K, V]) Delete(key K) {
	delete(m.newObjs, key)

	// To make sure we don't leak memory at the base, we need deleted objects FULLY deleted if
	// there's no parent -- so we should only add the key to deletes if there's a parent:
	if m.parent != nil {
		if _, ok := m.parent.Get(key); ok {
			// it exists in the parent -- delete it here.
			m.deletes[key] = struct{}{}
		}
	}
}

// Entries returns an iterator over key-value pairs in the map.
//
// Deleting elements from the map during iteration is always sound. They will not be visited later.
func (m *XactMap[K, V]) Entries() iter.Seq2[K, V] {
	return m.entriesImpl
}

// entriesImpl is the inner implementation of (*XactMap).Entries(), extracted for recursive use.
func (m *XactMap[K, V]) entriesImpl(yield func(K, V) bool) {
	// General plan:
	// 1. Iterate through all the elements added here
	// 2. Iterate through all the elements in the parent, as long as they weren't added or deleted
	// in this map instead.

	for k, v := range m.newObjs {
		if !yield(k, v) {
			return
		}
	}

	if m.parent != nil {
		m.parent.entriesImpl(func(k K, v V) bool {
			if _, ok := m.newObjs[k]; ok {
				return true
			}
			if _, ok := m.deletes[k]; ok {
				return true
			}

			return yield(k, v)
		})
	}
}
