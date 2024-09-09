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
//
// This type is sent directly to the monitor during the creation of a new
// Dispatcher as part of figuring out which protocol to use.
type VersionRange[V constraints.Ordered] struct {
	Min V `json:"min"`
	Max V `json:"max"`
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
	maxVersion := util.Min(r.Max, cmp.Max)
	minVersion := util.Max(r.Min, cmp.Min)
	if maxVersion >= minVersion {
		return maxVersion, true
	} else {
		var v V
		return v, false
	}
}
