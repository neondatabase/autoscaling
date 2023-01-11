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
