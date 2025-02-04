package queue_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/neondatabase/autoscaling/pkg/util/queue"
)

func isOk[T any](_ T, ok bool) bool {
	return ok
}

func getV[T any](v T, _ bool) T {
	return v
}

func TestPriorityQueueOperations(t *testing.T) {
	type value struct {
		name     string
		priority int
	}

	isHigherPriority := func(x, y value) bool {
		return x.priority > y.priority
	}

	// queue ordered so that values with larger priority should be returned first
	q := queue.New(isHigherPriority)
	assert.Equal(t, 0, q.Len())
	assert.Equal(t, false, isOk(q.Peek()))

	// => [ foo=3 ]
	q.Push(value{name: "foo", priority: 3})

	assert.Equal(t, 1, q.Len())
	assert.Equal(t, true, isOk(q.Peek()))

	// => [ ]
	v, ok := q.Pop()
	assert.Equal(t, true, ok)
	assert.Equal(t, "foo", v.name)

	// push multiple items, then pop+reorder+pop
	// => [ bar=1 ]
	barHandle := q.Push(value{name: "bar", priority: 1})
	// => [ bar=1 , baz=3 ]
	bazHandle := q.Push(value{name: "baz", priority: 3})
	// => [ bar=1 , qux=2 , baz=3 ]
	q.Push(value{name: "qux", priority: 2})

	assert.Equal(t, 3, q.Len())
	assert.Equal(t, "baz", getV(q.Peek()).name)

	// => [ bar=1 , qux=2 , baz-new=3 ]
	bazHandle.Update(func(v *value) {
		v.name = "baz-new"
	})

	// => [ bar=1 , qux=2 ]
	assert.Equal(t, "baz-new", getV(q.Pop()).name)

	assert.Equal(t, "qux", getV(q.Peek()).name)

	// => [ qux=2 , bar=4 ]
	barHandle.Update(func(v *value) {
		v.priority = 4
	})
	assert.Equal(t, "bar", getV(q.Peek()).name)

	// => [ qux=2 ]
	assert.Equal(t, "bar", getV(q.Pop()).name)

	assert.Equal(t, "qux", getV(q.Pop()).name)
}
