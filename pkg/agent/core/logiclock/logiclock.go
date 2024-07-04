package logiclock

import (
	"errors"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type Kind string

const (
	KindUpscale   Kind = "upscale"
	KindDownscale Kind = "downscale"
)

type measurement struct {
	createdAt time.Time
	kind      Kind
}

type Clock struct {
	cb           func(time.Duration, Kind)
	measurements []measurement
	offset       int64
}

func NewClock(cb func(time.Duration, Kind)) *Clock {
	return &Clock{
		cb:           cb,
		measurements: nil,
		offset:       0,
	}
}

func (c *Clock) NextValue() int64 {
	return c.offset + int64(len(c.measurements))
}

func (c *Clock) Next(now time.Time, kind Kind) *vmv1.LogicalTime {
	ret := vmv1.LogicalTime{
		Value:     c.NextValue(),
		UpdatedAt: v1.NewTime(now),
	}
	c.measurements = append(c.measurements, measurement{
		createdAt: ret.UpdatedAt.Time,
		kind:      kind,
	})
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
	if idx > int64(len(c.measurements)) {
		return errors.New("logicalTime value is in the future")
	}

	diff := logicalTime.UpdatedAt.Time.Sub(c.measurements[idx].createdAt)

	if c.cb != nil {
		c.cb(diff, c.measurements[idx].kind)
	}

	c.offset = logicalTime.Value + 1
	c.measurements = c.measurements[idx+1:]

	return nil
}

type NilClock struct{}

func (c *NilClock) Next(now time.Time, _ Kind) *vmv1.LogicalTime { return nil }
func (c *NilClock) Observe(_ *vmv1.LogicalTime) error            { return nil }
