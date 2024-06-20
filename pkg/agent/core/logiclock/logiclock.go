package logiclock

import (
	"errors"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type Clock struct {
	cb     func(time.Duration)
	times  []time.Time
	offset int64
}

func NewClock(cb func(time.Duration)) *Clock {
	return &Clock{
		cb:     cb,
		times:  nil,
		offset: 0,
	}
}

func (c *Clock) NextValue() int64 {
	return c.offset + int64(len(c.times))
}

func (c *Clock) Next(now time.Time) *vmv1.LogicalTime {
	ret := vmv1.LogicalTime{
		Value:     c.NextValue(),
		UpdatedAt: v1.NewTime(now),
	}
	c.times = append(c.times, ret.UpdatedAt.Time)
	return &ret
}

func (c *Clock) Observe(logicalTime *vmv1.LogicalTime) error {
	if logicalTime == nil {
		return nil
	}
	if logicalTime.Value < c.offset {
		return nil
	}

	idx := logicalTime.Value - c.offset
	if idx > int64(len(c.times)) {
		return errors.New("logicalTime value is in the future")
	}

	diff := logicalTime.UpdatedAt.Time.Sub(c.times[idx])

	if c.cb != nil {
		c.cb(diff)
	}

	c.offset = logicalTime.Value + 1
	c.times = c.times[idx+1:]

	return nil
}

type NilClock struct{}

func (c *NilClock) Next(now time.Time) *vmv1.LogicalTime { return nil }
func (c *NilClock) Observe(_ *vmv1.LogicalTime) error    { return nil }
