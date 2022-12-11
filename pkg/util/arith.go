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
