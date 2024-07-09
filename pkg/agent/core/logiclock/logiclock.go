package logiclock

import (
	"errors"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

// Flag is a set of flags that can be associated with a logical timestamp.
type Flag uint64

const (
	Upscale Flag = 1 << iota
	Downscale
	Immediate
)

func (f *Flag) Set(flag Flag) {
	*f |= flag
}

func (f *Flag) Clear(flag Flag) {
	*f &= ^flag
}

func (f Flag) Has(flag Flag) bool {
	return f&flag != 0
}

// Clock can generate and observe logical time.
// Each logical timestamp is associated with a physical timestamp and a set of flags upon creation.
// Once Clock observes a previously generated timestamp after some time, it will call the callback with
// the time difference and the flags associated with the timestamp.
type Clock struct {
	cb func(time.Duration, Flag)

	// The in-flight timestamps are stored in-order.
	// After the timestamp is observed, it is removed from the measurements, and the offset is increased.
	measurements []measurement
	offset       int64
}

type measurement struct {
	createdAt time.Time
	flags     Flag
}

func NewClock(cb func(time.Duration, Flag)) *Clock {
	return &Clock{
		cb:           cb,
		measurements: nil,
		offset:       0,
	}
}

func (c *Clock) nextValue() int64 {
	return c.offset + int64(len(c.measurements))
}

func (c *Clock) Next(now time.Time, flags Flag) *vmv1.LogicalTime {
	ret := vmv1.LogicalTime{
		Value:     c.nextValue(),
		UpdatedAt: v1.NewTime(now),
	}
	c.measurements = append(c.measurements, measurement{
		createdAt: ret.UpdatedAt.Time,
		flags:     flags,
	})
	return &ret
}

func (c *Clock) Observe(logicalTime *vmv1.LogicalTime) error {
	if logicalTime == nil {
		return nil
	}
	if logicalTime.Value < c.offset {
		// Already observed
		return nil
	}

	idx := logicalTime.Value - c.offset
	if idx > int64(len(c.measurements)) {
		return errors.New("logicalTime value is in the future")
	}

	diff := logicalTime.UpdatedAt.Time.Sub(c.measurements[idx].createdAt)

	if c.cb != nil {
		c.cb(diff, c.measurements[idx].flags)
	}

	// Forget the measurement, and all the measurements before it.
	c.offset = logicalTime.Value + 1
	c.measurements = c.measurements[idx+1:]

	return nil
}

type NilClock struct{}

func (c *NilClock) Next(_ time.Time, _ Flag) *vmv1.LogicalTime { return nil }
func (c *NilClock) Observe(_ *vmv1.LogicalTime) error          { return nil }
