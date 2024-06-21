package logicclock

import (
	"errors"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type ClockSource struct {
	cb     func(time.Duration)
	ts     []time.Time
	offset int64
	Now    func() v1.Time
}

func NewClockSource(cb func(time.Duration)) *ClockSource {
	return &ClockSource{
		cb:     cb,
		ts:     nil,
		offset: 0,
		Now:    v1.Now,
	}
}

func (c *ClockSource) Next() *vmv1.LogicalTime {
	ret := vmv1.LogicalTime{
		Value:     c.offset + int64(len(c.ts)),
		UpdatedAt: c.Now(),
	}
	c.ts = append(c.ts, ret.UpdatedAt.Time)
	return &ret
}

func (c *ClockSource) Observe(clock *vmv1.LogicalTime) error {
	if clock == nil {
		return nil
	}
	if clock.Value < c.offset {
		return nil
	}

	idx := clock.Value - c.offset
	if idx > int64(len(c.ts)) {
		return errors.New("clock value is in the future")
	}

	diff := clock.UpdatedAt.Time.Sub(c.ts[idx])

	c.cb(diff)

	c.offset = clock.Value + 1
	c.ts = c.ts[idx+1:]

	return nil
}
