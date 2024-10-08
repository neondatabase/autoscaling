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

func NewTimeRange(units time.Duration, minTime, maxTime int) *TimeRange {
	if minTime < 0 {
		panic(errors.New("bad time range: min < 0"))
	} else if minTime == 0 && maxTime == 0 {
		panic(errors.New("bad time range: min and max = 0"))
	} else if maxTime < minTime {
		panic(errors.New("bad time range: max < min"))
	}

	return &TimeRange{min: minTime, max: maxTime, units: units}
}

// Random returns a random time.Duration within the range
func (r TimeRange) Random() time.Duration {
	if r.max == r.min {
		return time.Duration(r.min) * r.units
	}

	count := rand.Intn(r.max-r.min) + r.min
	return time.Duration(count) * r.units
}
