package testhelpers

import (
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type ExpectedRevision struct {
	vmv1.Revision
	Now func() time.Time
}

func NewExpectedRevision(now func() time.Time) *ExpectedRevision {
	return &ExpectedRevision{
		Now:      now,
		Revision: vmv1.ZeroRevision,
	}
}

func (e *ExpectedRevision) WithTime() vmv1.RevisionWithTime {
	return e.Revision.WithTime(e.Now())
}

type NilRevisionSource struct{}

func (c *NilRevisionSource) Next(_ time.Time, _ vmv1.Flag) vmv1.Revision {
	return vmv1.Revision{
		Value: 0,
		Flags: 0,
	}
}
func (c *NilRevisionSource) Observe(_ time.Time, _ vmv1.Revision) error { return nil }
