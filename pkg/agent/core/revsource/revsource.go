package revsource

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

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

func NewRevisionSource(cb ObserveCallback) *RevisionSource {
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

func WrapHistogramVec(hist *prometheus.HistogramVec) ObserveCallback {
	return func(dur time.Duration, flags vmv1.Flag) {
		labels := FlagsToLabels(flags)
		hist.WithLabelValues(labels...).Observe(dur.Seconds())
	}
}

// Propagate sets the target revision to be current, optionally measuring the time it took
// for propagation.
func Propagate(
	now time.Time,
	target vmv1.RevisionWithTime,
	currentSlot *vmv1.Revision,
	metricCB ObserveCallback,
) {
	if metricCB != nil {
		diff := now.Sub(target.UpdatedAt.Time)
		metricCB(diff, target.Flags)
	}
	if currentSlot == nil {
		return
	}
	if currentSlot.Value > target.Value {
		return
	}
	*currentSlot = target.Revision
}
