package failurelag_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/neondatabase/autoscaling/pkg/neonvm/controllers/failurelag"
)

type nowMock struct {
	ts time.Time
}

func (n *nowMock) Now() time.Time {
	return n.ts
}

func (n *nowMock) Add(d time.Duration) {
	n.ts = n.ts.Add(d)
}

func newNowMock() *nowMock {
	ts, _ := time.Parse("2006-01-02", "2024-01-01")
	return &nowMock{ts: ts}
}

func TestTracker(t *testing.T) {
	now := newNowMock()
	tracker := failurelag.NewTracker[string](10 * time.Minute)
	tracker.Now = now.Now

	// Alert fires after 15 minutes
	tracker.RecordFailure("key1")
	assert.Equal(t, tracker.DegradedCount(), 0)
	now.Add(15 * time.Minute)
	assert.Equal(t, tracker.DegradedCount(), 1)

	// Alert no longer fires
	tracker.RecordSuccess("key1")
	assert.Equal(t, tracker.DegradedCount(), 0)
}

func TestFailureNotRetried(t *testing.T) {
	now := newNowMock()
	tracker := failurelag.NewTracker[string](10 * time.Minute)
	tracker.Now = now.Now

	// No failures yet
	tracker.RecordFailure("key1")
	assert.Equal(t, tracker.DegradedRetriedCount(), 0)
	assert.Equal(t, tracker.DegradedNotRetriedCount(), 0)

	// Not retried yet
	now.Add(15 * time.Minute)
	assert.Equal(t, tracker.DegradedRetriedCount(), 0)
	assert.Equal(t, tracker.DegradedNotRetriedCount(), 1)

	// Successful retry
	tracker.RecordSuccess("key1")
	assert.Equal(t, tracker.DegradedRetriedCount(), 0)
	assert.Equal(t, tracker.DegradedNotRetriedCount(), 0)
}

func TestFailureRetried(t *testing.T) {
	now := newNowMock()
	tracker := failurelag.NewTracker[string](10 * time.Minute)
	tracker.Now = now.Now

	tracker.RecordFailure("key1")
	assert.Equal(t, tracker.DegradedCount(), 0)

	now.Add(5 * time.Minute)
	tracker.RecordFailure("key1")

	now.Add(7 * time.Minute)
	assert.Equal(t, tracker.DegradedRetriedCount(), 1)
	assert.Equal(t, tracker.DegradedNotRetriedCount(), 0)

	tracker.RecordSuccess("key1")
	assert.Equal(t, tracker.DegradedRetriedCount(), 0)
	assert.Equal(t, tracker.DegradedNotRetriedCount(), 0)
}

func TestFailureRetriedAndNotRetried(t *testing.T) {
	now := newNowMock()
	tracker := failurelag.NewTracker[string](10 * time.Minute)
	tracker.Now = now.Now

	// No failures yet
	tracker.RecordFailure("key1")
	assert.Equal(t, tracker.DegradedRetriedCount(), 0)
	assert.Equal(t, tracker.DegradedNotRetriedCount(), 0)

	// Not retried yet
	now.Add(15 * time.Minute)
	assert.Equal(t, tracker.DegradedRetriedCount(), 0)
	assert.Equal(t, tracker.DegradedNotRetriedCount(), 1)

	// One failed retry
	tracker.RecordFailure("key1")
	assert.Equal(t, tracker.DegradedRetriedCount(), 1)
	assert.Equal(t, tracker.DegradedNotRetriedCount(), 0)

	// No retries again
	now.Add(15 * time.Minute)
	assert.Equal(t, tracker.DegradedRetriedCount(), 0)
	assert.Equal(t, tracker.DegradedNotRetriedCount(), 1)

	// Successful retry
	tracker.RecordSuccess("key1")
	assert.Equal(t, tracker.DegradedRetriedCount(), 0)
	assert.Equal(t, tracker.DegradedNotRetriedCount(), 0)
}

func TestFailureSuccess(t *testing.T) {
	now := newNowMock()
	tracker := failurelag.NewTracker[string](10 * time.Minute)
	tracker.Now = now.Now

	// Alert doesn't fire if there was a success in the interval
	tracker.RecordFailure("key1")

	now.Add(5 * time.Minute)
	tracker.RecordSuccess("key1")

	now.Add(10 * time.Minute)
	assert.Equal(t, tracker.DegradedCount(), 0)
}

func TestFailureSuccessFailure(t *testing.T) {
	now := newNowMock()
	tracker := failurelag.NewTracker[string](10 * time.Minute)
	tracker.Now = now.Now

	// Alert doesn't fire if there was success + failure in the interval
	tracker.RecordFailure("key1")

	now.Add(5 * time.Minute)
	tracker.RecordSuccess("key1")

	now.Add(1 * time.Minute)
	tracker.RecordFailure("key1")

	now.Add(5 * time.Minute)
	assert.Equal(t, tracker.DegradedCount(), 0)

	// But after 7 more minutes it does
	now.Add(7 * time.Minute)
	assert.Equal(t, tracker.DegradedCount(), 1)
}

func TestMultipleKeys(t *testing.T) {
	now := newNowMock()
	tracker := failurelag.NewTracker[string](10 * time.Minute)
	tracker.Now = now.Now

	// A combination of TestFailureSuccess and TestFailureSuccessFailure
	tracker.RecordFailure("key1")
	tracker.RecordFailure("key2")

	now.Add(5 * time.Minute)
	tracker.RecordSuccess("key1")
	tracker.RecordSuccess("key2")

	now.Add(1 * time.Minute)
	tracker.RecordFailure("key1")

	now.Add(5 * time.Minute)
	assert.Equal(t, tracker.DegradedCount(), 0)

	now.Add(7 * time.Minute)
	assert.Equal(t, tracker.DegradedCount(), 1)
	assert.Equal(t, tracker.Degraded(), []string{"key1"})

	tracker.RecordFailure("key2")
	now.Add(15 * time.Minute)
	assert.Equal(t, tracker.DegradedCount(), 2)
	assert.Contains(t, tracker.Degraded(), "key1")
	assert.Contains(t, tracker.Degraded(), "key2")
}
