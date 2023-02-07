package api

// Generic version handling

import (
	"fmt"

	"golang.org/x/exp/constraints"

	"github.com/neondatabase/autoscaling/pkg/util"
)

// VersionRange is a helper type to represent a range of versions.
//
// The bounds are inclusive, representing all versions v with Min <= v <= Max.
type VersionRange[V constraints.Ordered] struct {
	Min V
	Max V
}

func (r VersionRange[V]) String() string {
	if r.Min == r.Max {
		return fmt.Sprintf("%v", r.Min)
	} else {
		return fmt.Sprintf("%v to %v", r.Min, r.Max)
	}
}

// LatestSharedVersion returns the latest version covered by both VersionRanges, if there is one.
//
// If either range is invalid, or no such version exists (i.e. the ranges are disjoint), then the
// returned values will be (0, false).
func (r VersionRange[V]) LatestSharedVersion(cmp VersionRange[V]) (_ V, ok bool) {
	max := util.Min(r.Max, cmp.Max)
	min := util.Max(r.Min, cmp.Min)
	if max >= min {
		return max, true
	} else {
		var v V
		return v, false
	}
}
