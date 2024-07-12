package revsource

import (
	"errors"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

const (
	Upscale vmv1.Flag = 1 << iota
	Downscale
	Immediate
)

// AllFlags and AllFlagNames must have the same order, so the metrics work correctly.
var AllFlags = []vmv1.Flag{Upscale, Downscale, Immediate}
var AllFlagNames = []string{"upscale", "downscale", "immediate"}

// FlagsToLabels converts a set of flags to a list of strings which prometheus can take.
func FlagsToLabels(flags vmv1.Flag) []string {
	var ret []string
	for _, flag := range AllFlags {
		value := "false"
		if flags.Has(flag) {
			value = "true"
		}
		ret = append(ret, value)
	}
	return ret
}

// RevisionSource can generate and observe logical time.
// Each logical timestamp is associated with a physical timestamp and a set of flags upon creation.
// Once RevisionSource observes a previously generated timestamp after some time, it will call the callback with
// the time difference and the flags associated with the timestamp.
type RevisionSource struct {
	cb func(time.Duration, vmv1.Flag)

	// The in-flight timestamps are stored in-order.
	// After the timestamp is observed, it is removed from the measurements, and the offset is increased.
	measurements []time.Time
	offset       int64
}

func NewRevisionSource(cb func(time.Duration, vmv1.Flag)) *RevisionSource {
	return &RevisionSource{
		cb:           cb,
		measurements: nil,
		offset:       1, // Start with 1, 0 is reserved for default value.
	}
}

func (c *RevisionSource) nextValue() int64 {
	return c.offset + int64(len(c.measurements))
}

func (c *RevisionSource) Next(now time.Time, flags vmv1.Flag) vmv1.Revision {
	ret := vmv1.Revision{
		Value: c.nextValue(),
		Flags: flags,
	}
	c.measurements = append(c.measurements, now)
	return ret
}

func (c *RevisionSource) Observe(moment time.Time, rev vmv1.Revision) error {
	if rev.Value < c.offset {
		// Already observed
		return nil
	}

	idx := rev.Value - c.offset
	if idx > int64(len(c.measurements)) {
		return errors.New("revision is in the future")
	}

	diff := moment.Sub(c.measurements[idx])

	if c.cb != nil {
		c.cb(diff, rev.Flags)
	}

	// Forget the measurement, and all the measurements before it.
	c.offset = rev.Value + 1
	c.measurements = c.measurements[idx+1:]

	return nil
}

type NilRevisionSource struct{}

func (c *NilRevisionSource) Next(_ time.Time, _ vmv1.Flag) vmv1.Revision {
	return vmv1.Revision{
		Value: 0,
		Flags: 0,
	}
}
func (c *NilRevisionSource) Observe(_ time.Time, _ vmv1.Revision) error { return nil }
