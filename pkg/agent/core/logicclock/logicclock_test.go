package logicclock_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/agent/core/logicclock"
)

type testClockMetric struct {
	*logicclock.ClockSource
	t      *testing.T
	now    v1.Time
	result *time.Duration
}

func (tcm *testClockMetric) advance(d time.Duration) {
	tcm.now = v1.NewTime(tcm.now.Add(d))
}

func (tcm *testClockMetric) assertResult(d time.Duration) {
	require.NotNil(tcm.t, tcm.result)
	assert.Equal(tcm.t, d, *tcm.result)
	tcm.result = nil
}

func newTestClockMetric(t *testing.T) *testClockMetric {
	tcm := &testClockMetric{
		ClockSource: nil,
		t:           t,
		now:         v1.NewTime(time.Now()),
		result:      nil,
	}

	cb := func(d time.Duration) {
		tcm.result = &d
	}
	tcm.ClockSource = logicclock.NewClockSource(cb)
	tcm.ClockSource.Now = func() v1.Time {
		return tcm.now
	}

	return tcm
}

func TestClockMetric(t *testing.T) {
	tcm := newTestClockMetric(t)

	// Generate new clock
	cl := tcm.Next()
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
	cl := tcm.Next()
	assert.Equal(t, int64(0), cl.Value)

	// Generate another one
	tcm.advance(5 * time.Second)
	cl = tcm.Next()
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
	cl := tcm.Next()
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
