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

// AbsDiff returns the absolute value of the difference between x and y
func AbsDiff[T constraints.Unsigned](x, y T) T {
	if x > y {
		return x - y
	} else {
		return y - x
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
// On ISAs without atomic maximum/minimum instructions, a fallback is typically implemented as the
// Load + CompareAndSwap loop that this function uses. At time of writing (Go 1.20), the Go standard
// library does not include atomic maximum/minimum functions.
//
// This function is lock-free but not wait-free.
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
