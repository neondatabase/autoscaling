package revsource

import (
	"errors"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

const (
	Upscale vmv1.Flag = 1 << iota
	Downscale
)

// MaxRevisions is the maximum number of revisions that can be stored in the RevisionSource.
// This is to prevent memory leaks.
// Upon reaching it, the oldest revisions are discarded.
const MaxRevisions = 100

// RevisionSource can generate and observe revisions.
// Each Revision is a value and a set of flags (for meta-information).
// Once RevisionSource observes a previously generated Revision after some time,
// the time it took since that Revision was generated.
type RevisionSource struct {
	cb ObserveCallback

	// The in-flight revisions are stored in-order.
	// After the revision is observed, it is removed from the measurements, and the offset is increased.
	measurements []time.Time
	offset       int64
}

func NewRevisionSource(initialRevision int64, cb ObserveCallback) *RevisionSource {
	return &RevisionSource{
		cb:           cb,
		measurements: nil,
		offset:       initialRevision + 1, // Will start from the next one
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

	if len(c.measurements) > MaxRevisions {
		c.measurements = c.measurements[1:]
		c.offset++
	}

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

type ObserveCallback func(dur time.Duration, flags vmv1.Flag)

// Propagate sets the target revision to be current, optionally measuring the time it took
// for propagation.
func Propagate(
	now time.Time,
	target vmv1.RevisionWithTime,
	currentSlot *vmv1.Revision,
	cb ObserveCallback,
) {
	if currentSlot == nil {
		return
	}
	if currentSlot.Value >= target.Value {
		return
	}
	if cb != nil {
		diff := now.Sub(target.UpdatedAt.Time)
		cb(diff, target.Flags)
	}
	*currentSlot = target.Revision
}
