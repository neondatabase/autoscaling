package logiclock_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/agent/core/logiclock"
)

type testClockMetric struct {
	*logiclock.Clock
	t          *testing.T
	now        v1.Time
	result     *time.Duration
	resultKind logiclock.Kind
}

func (tcm *testClockMetric) advance(d time.Duration) {
	tcm.now = v1.NewTime(tcm.now.Add(d))
}

func (tcm *testClockMetric) assertResult(d time.Duration) {
	require.NotNil(tcm.t, tcm.result)
	assert.Equal(tcm.t, d, *tcm.result)
	tcm.result = nil
}

func (tcm *testClockMetric) nextNow() *vmv1.LogicalTime {
	return tcm.Next(tcm.now.Time, logiclock.KindUpscale)
}

func newTestClockMetric(t *testing.T) *testClockMetric {
	tcm := &testClockMetric{
		Clock:  nil,
		t:      t,
		now:    v1.NewTime(time.Now()),
		result: nil,
	}

	cb := func(d time.Duration, kind logiclock.Kind) {
		tcm.result = &d
		tcm.resultKind = kind
	}
	tcm.Clock = logiclock.NewClock(cb)

	return tcm
}

func TestClockMetric(t *testing.T) {
	tcm := newTestClockMetric(t)

	// Generate new clock
	cl := tcm.nextNow()
	assert.Equal(t, int64(0), cl.Value)

	// Observe it coming back in 5 seconds
	tcm.advance(5 * time.Second)
	err := tcm.Observe(&vmv1.LogicalTime{
		Value:     0,
		UpdatedAt: tcm.now,
	})
	assert.NoError(t, err)
	tcm.assertResult(5 * time.Second)
}

func TestClockMetricSkip(t *testing.T) {
	tcm := newTestClockMetric(t)

	// Generate new clock
	cl := tcm.nextNow()
	assert.Equal(t, int64(0), cl.Value)

	// Generate another one
	tcm.advance(5 * time.Second)
	cl = tcm.nextNow()
	assert.Equal(t, int64(1), cl.Value)

	// Observe the first one
	tcm.advance(5 * time.Second)
	err := tcm.Observe(&vmv1.LogicalTime{
		Value:     0,
		UpdatedAt: tcm.now,
	})
	assert.NoError(t, err)
	tcm.assertResult(10 * time.Second)

	// Observe the second one
	tcm.advance(2 * time.Second)
	err = tcm.Observe(&vmv1.LogicalTime{
		Value:     1,
		UpdatedAt: tcm.now,
	})
	assert.NoError(t, err)
	tcm.assertResult(7 * time.Second)
}

func TestClockMetricStale(t *testing.T) {
	tcm := newTestClockMetric(t)

	// Generate new clock
	cl := tcm.nextNow()
	assert.Equal(t, int64(0), cl.Value)

	// Observe it coming back in 5 seconds
	tcm.advance(5 * time.Second)
	err := tcm.Observe(&vmv1.LogicalTime{
		Value:     0,
		UpdatedAt: tcm.now,
	})
	assert.NoError(t, err)
	tcm.assertResult(5 * time.Second)

	// Observe it coming back again
	tcm.advance(5 * time.Second)
	err = tcm.Observe(&vmv1.LogicalTime{
		Value:     0,
		UpdatedAt: tcm.now,
	})
	// No error, but no result either
	assert.NoError(t, err)
	assert.Nil(t, tcm.result)
}
