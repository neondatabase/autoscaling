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
