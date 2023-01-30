package util

// Helper arithmetic methods

import (
	"golang.org/x/exp/constraints"
)

// SaturatingSub returns x - y if x >= y, otherwise zero
func SaturatingSub[T constraints.Unsigned](x, y T) T {
	if x >= y {
		return x - y
	} else {
		var zero T
		return zero
	}
}

// Max returns the maximum of the two values
func Max[T constraints.Ordered](x, y T) T {
	if x > y {
		return x
	} else {
		return y
	}
}

// Max returns the minimum of the two values
func Min[T constraints.Ordered](x, y T) T {
	if x < y {
		return x
	} else {
		return y
	}
}

// AtomicInt represents the shared interface provided by various atomic.<NAME> integers
//
// This interface type is primarily used by AtomicMax.
type AtomicInt[I any] interface {
	Add(delta I) (new I)                      //nolint:predeclared // same var names as methods
	CompareAndSwap(old, new I) (swapped bool) //nolint:predeclared // same var names as methods
	Load() I
	Store(val I)
	Swap(new I) (old I) //nolint:predeclared // same var names as methods
}

// AtomicMax atomically sets a to the maximum of *a and i, returning the old value at a.
//
// Eventually, one would *hope* that go could add atomic maximum (and minimum), so we don't need
// this function (there are some ISAs that support such instructions). In the meantime though, this
// tends to be pretty useful - event though it's not wait-free.
func AtomicMax[A AtomicInt[I], I constraints.Integer](a A, i I) I {
	for {
		current := a.Load()
		if current >= i {
			return current
		}
		if a.CompareAndSwap(current, i) {
			return current
		}
	}
}
