package alerttracker_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/neondatabase/autoscaling/neonvm/controllers/alerttracker"
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
	alerttracker.Now = now.Now
	tracker := alerttracker.NewTracker[string](10 * time.Minute)

	// Alert fires after 15 minutes
	tracker.RecordFailure("key1")
	assert.Equal(t, tracker.FiringCount(), 0)
	now.Add(15 * time.Minute)
	assert.Equal(t, tracker.FiringCount(), 1)

	// Alert no longer fires
	tracker.RecordSuccess("key1")
	assert.Equal(t, tracker.FiringCount(), 0)
}

func TestFailureSuccess(t *testing.T) {
	now := newNowMock()
	alerttracker.Now = now.Now
	tracker := alerttracker.NewTracker[string](10 * time.Minute)

	// Alert doesn't fire if there was a success in the interval
	tracker.RecordFailure("key1")

	now.Add(5 * time.Minute)
	tracker.RecordSuccess("key1")

	now.Add(10 * time.Minute)
	assert.Equal(t, tracker.FiringCount(), 0)
}

func TestFailureSuccessFailure(t *testing.T) {
	now := newNowMock()
	alerttracker.Now = now.Now
	tracker := alerttracker.NewTracker[string](10 * time.Minute)

	// Alert doesn't fire if there was success + failure in the interval
	tracker.RecordFailure("key1")

	now.Add(5 * time.Minute)
	tracker.RecordSuccess("key1")

	now.Add(1 * time.Minute)
	tracker.RecordFailure("key1")

	now.Add(5 * time.Minute)
	assert.Equal(t, tracker.FiringCount(), 0)

	// But after 7 more minutes it does
	now.Add(7 * time.Minute)
	assert.Equal(t, tracker.FiringCount(), 1)
}

func TestMultipleKeys(t *testing.T) {
	now := newNowMock()
	alerttracker.Now = now.Now
	tracker := alerttracker.NewTracker[string](10 * time.Minute)

	// A combination of TestFailureSuccess and TestFailureSuccessFailure
	tracker.RecordFailure("key1")
	tracker.RecordFailure("key2")

	now.Add(5 * time.Minute)
	tracker.RecordSuccess("key1")
	tracker.RecordSuccess("key2")

	now.Add(1 * time.Minute)
	tracker.RecordFailure("key1")

	now.Add(5 * time.Minute)
	assert.Equal(t, tracker.FiringCount(), 0)

	now.Add(7 * time.Minute)
	assert.Equal(t, tracker.FiringCount(), 1)
	assert.Equal(t, tracker.Firing(), []string{"key1"})

	tracker.RecordFailure("key2")
	now.Add(15 * time.Minute)
	assert.Equal(t, tracker.FiringCount(), 2)
	assert.Contains(t, tracker.Firing(), "key1")
	assert.Contains(t, tracker.Firing(), "key2")
}