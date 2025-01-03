package state_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/exp/slices"

	"github.com/neondatabase/autoscaling/pkg/plugin/state"
)

func value[V any](value V, _ bool) V {
	return value
}

func ok[V any](_ V, ok bool) bool {
	return ok
}

func TestBasicMapUsage(t *testing.T) {
	m := state.NewXactMap[string, string]()

	assert.Equal(t, false, ok(m.Get("foo")))

	m.Set("foo", "bar")
	assert.Equal(t, true, ok(m.Get("foo")))
	assert.Equal(t, "bar", value(m.Get("foo")))
	assert.Equal(t, false, ok(m.Get("baz")))

	m.Set("baz", "qux")
	assert.Equal(t, true, ok(m.Get("foo")))
	assert.Equal(t, "bar", value(m.Get("foo")))
	assert.Equal(t, true, ok(m.Get("baz")))
	assert.Equal(t, "qux", value(m.Get("baz")))

	m.Set("baz", "bazqux")
	assert.Equal(t, true, ok(m.Get("baz")))
	assert.Equal(t, "bazqux", value(m.Get("baz")))

	m.Delete("foo")
	assert.Equal(t, false, ok(m.Get("foo")))
	assert.Equal(t, true, ok(m.Get("baz")))
}

func TestMapTransaction(t *testing.T) {
	m1 := state.NewXactMap[string, string]()
	m1.Set("abc", "xyz")
	m1.Set("def", "ghi")

	m2 := m1.NewTransaction()
	m2.Set("foo", "bar")
	// "foo" exists in m2 but not m1
	assert.Equal(t, "bar", value(m2.Get("foo")))
	assert.Equal(t, false, ok(m1.Get("foo")))

	m2.Set("abc", "zyx")
	// m2 has abc = zyx, but m1 has abc = xyz
	assert.Equal(t, "zyx", value(m2.Get("abc")))
	assert.Equal(t, "xyz", value(m1.Get("abc")))

	m2.Delete("def")
	// m2 doesn't have def, but m1 does
	assert.Equal(t, false, ok(m2.Get("def")))
	assert.Equal(t, true, ok(m1.Get("def")))

	m2.Delete("foo")
	// neither has foo now
	assert.Equal(t, false, ok(m2.Get("foo")))
	assert.Equal(t, false, ok(m1.Get("foo")))

	// one last change:
	m2.Set("baz", "qux")

	m2.Commit()
	// After committing the transaction, we should see all of the changes:
	// * abc was changed
	// * def was deleted
	// * baz was added
	assert.Equal(t, "zyx", value(m1.Get("abc")))
	assert.Equal(t, false, ok(m1.Get("def")))
	assert.Equal(t, "qux", value(m1.Get("baz")))
}

func TestMapIter(t *testing.T) {
	m1 := state.NewXactMap[string, string]()
	m1.Set("ab", "c")
	m1.Set("de", "f")
	m1.Set("gh", "i")

	items := list(m1, 3)
	assert.Equal(t, 3, len(items))
	assert.Equal(t, true, isSubset(items, []pair[string, string]{
		{"ab", "c"},
		{"de", "f"},
		{"gh", "i"},
	}))

	items = list(m1, 2)
	assert.Equal(t, 2, len(items))
	assert.Equal(t, true, isSubset(items, []pair[string, string]{
		{"ab", "c"},
		{"de", "f"},
		{"gh", "i"},
	}))

	m2 := m1.NewTransaction()
	m2.Set("de", "ff")
	m2.Delete("gh")
	m2.Set("jk", "l")

	items = list(m2, 3)
	assert.Equal(t, 3, len(items))
	assert.Equal(t, true, isSubset(items, []pair[string, string]{
		{"ab", "c"},
		{"de", "ff"},
		{"jk", "l"},
	}))

	items = list(m2, 2)
	assert.Equal(t, 2, len(items))
	assert.Equal(t, true, isSubset(items, []pair[string, string]{
		{"ab", "c"},
		{"de", "ff"},
		{"jk", "l"},
	}))

	items = list(m1, 3)
	assert.Equal(t, 3, len(items))
	assert.Equal(t, true, isSubset(items, []pair[string, string]{
		{"ab", "c"},
		{"de", "f"},
		{"gh", "i"},
	}))

	m2.Commit()

	items = list(m1, 3)
	assert.Equal(t, 3, len(items))
	assert.Equal(t, true, isSubset(items, []pair[string, string]{
		{"ab", "c"},
		{"de", "ff"},
		{"jk", "l"},
	}))
}

type pair[K any, V any] struct {
	key   K
	value V
}

// helper function to use (*XactMap).Entries() to simply build a list of the key/value pairs returned.
func list[K comparable, V any](m *state.XactMap[K, V], length int) []pair[K, V] {
	var items []pair[K, V]

	for key, value := range m.Entries() {
		if len(items) >= length {
			break
		}

		items = append(items, pair[K, V]{key, value})
	}

	return items
}

// helper to determine if the contents of one list is a subset of the other
func isSubset[T comparable](subset, superset []T) bool {
	for i, v := range subset {
		// Check that subset has no duplicates:
		if slices.Contains(subset[i+1:], v) {
			return false
		}

		// Check that this member of the subset is within the superset:
		if !slices.Contains(superset, v) {
			return false
		}
	}
	return true
}
