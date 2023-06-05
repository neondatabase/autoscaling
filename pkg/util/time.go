package util

import (
	"errors"
	"math/rand"
	"time"
)

type TimeRange struct {
	min   int
	max   int
	units time.Duration
}

func NewTimeRange(units time.Duration, min, max int) *TimeRange {
	if min < 0 {
		panic(errors.New("bad time range: min < 0"))
	} else if min == 0 && max == 0 {
		panic(errors.New("bad time range: min and max = 0"))
	} else if max < min {
		panic(errors.New("bad time range: max < min"))
	}

	return &TimeRange{min: min, max: max, units: units}
}

// Random returns a random time.Duration within the range
func (r TimeRange) Random() time.Duration {
	if r.max == r.min {
		return time.Duration(r.min) * r.units
	}

	count := rand.Intn(r.max-r.min) + r.min
	return time.Duration(count) * r.units
}
