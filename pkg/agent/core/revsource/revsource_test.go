package revsource_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/agent/core/revsource"
)

type testRevisionSource struct {
	*revsource.RevisionSource
	t           *testing.T
	now         v1.Time
	result      *time.Duration
	resultFlags *vmv1.Flag
}

func (trs *testRevisionSource) advance(d time.Duration) {
	trs.now = v1.NewTime(trs.now.Add(d))
}

func (trs *testRevisionSource) assertResult(d time.Duration) {
	require.NotNil(trs.t, trs.result)
	assert.Equal(trs.t, d, *trs.result)
	trs.result = nil
}

func newTestRevisionSource(t *testing.T) *testRevisionSource {
	tcm := &testRevisionSource{
		RevisionSource: nil,
		t:              t,
		now:            v1.NewTime(time.Now()),
		result:         nil,
		resultFlags:    nil,
	}

	cb := func(d time.Duration, flags vmv1.Flag) {
		tcm.result = &d
		tcm.resultFlags = &flags
	}
	tcm.RevisionSource = revsource.NewRevisionSource(cb)

	return tcm
}

func TestRevSource(t *testing.T) {
	trs := newTestRevisionSource(t)

	// Generate new revision
	rev := trs.Next(trs.now.Time)
	assert.Equal(t, int64(0), rev.Value)

	// Observe it coming back in 5 seconds
	trs.advance(5 * time.Second)
	err := trs.Observe(trs.now.Time, rev)
	assert.NoError(t, err)
	trs.assertResult(5 * time.Second)
}

func TestRevSourceSkip(t *testing.T) {
	trs := newTestRevisionSource(t)

	// Generate new clock
	rev1 := trs.Next(trs.now.Time)
	assert.Equal(t, int64(0), rev1.Value)

	// Generate another one
	trs.advance(5 * time.Second)
	rev2 := trs.Next(trs.now.Time)
	assert.Equal(t, int64(1), rev2.Value)

	// Observe the first one
	trs.advance(5 * time.Second)
	err := trs.Observe(trs.now.Time, rev1)
	assert.NoError(t, err)
	trs.assertResult(10 * time.Second)

	// Observe the second one
	trs.advance(2 * time.Second)
	err = trs.Observe(trs.now.Time, rev2)
	assert.NoError(t, err)
	trs.assertResult(7 * time.Second)
}

func TestStale(t *testing.T) {
	trs := newTestRevisionSource(t)

	// Generate new clock
	cl := trs.Next(trs.now.Time)
	assert.Equal(t, int64(0), cl.Value)

	// Observe it coming back in 5 seconds
	trs.advance(5 * time.Second)
	err := trs.Observe(trs.now.Time, cl)
	assert.NoError(t, err)
	trs.assertResult(5 * time.Second)

	// Observe it coming back again
	trs.advance(5 * time.Second)
	err = trs.Observe(trs.now.Time, cl)
	// No error, but no result either
	assert.NoError(t, err)
	assert.Nil(t, trs.result)
}
