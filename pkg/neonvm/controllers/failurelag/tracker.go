package failurelag

import (
	"sync"
	"time"
)

// Tracker accumulates failure events for a given key and determines if
// the key is degraded. The key becomes degraded if it receives only failures
// over a configurable pending period. Once the success event is received, the key
// is no longer considered degraded, and the pending period is reset.
type Tracker[T comparable] struct {
	period time.Duration

	firstFailure map[T]time.Time
	refreshQueue []refreshAt[T]

	degraded map[T]struct{}

	lock sync.Mutex
	Now  func() time.Time
}

type refreshAt[T comparable] struct {
	ts  time.Time
	key T
}

func NewTracker[T comparable](period time.Duration) *Tracker[T] {
	return &Tracker[T]{
		period: period,

		firstFailure: make(map[T]time.Time),
		refreshQueue: make([]refreshAt[T], 0),

		degraded: make(map[T]struct{}),

		lock: sync.Mutex{},
		Now:  time.Now,
	}
}

// processQueue processes all refresh requests that are now in the past.
func (t *Tracker[T]) processQueue(now time.Time) {
	i := 0
	for ; i < len(t.refreshQueue); i++ {
		event := t.refreshQueue[i]
		if event.ts.After(now) {
			break
		}
		firstFailure, ok := t.firstFailure[event.key]
		if !ok {
			// There was a success event in between
			continue
		}

		if event.ts.Sub(firstFailure) < t.period {
			// There was a success, and another failure in between
			// We will have another fireAt event for this key in the future
			continue
		}
		t.degraded[event.key] = struct{}{}
	}
	t.refreshQueue = t.refreshQueue[i:]
}

func (t *Tracker[T]) RecordSuccess(key T) {
	t.lock.Lock()
	defer t.lock.Unlock()

	delete(t.degraded, key)
	delete(t.firstFailure, key)
	t.processQueue(t.Now())
}

func (t *Tracker[T]) RecordFailure(key T) {
	t.lock.Lock()
	defer t.lock.Unlock()

	now := t.Now()

	if _, ok := t.firstFailure[key]; !ok {
		t.firstFailure[key] = now
	}

	t.refreshQueue = append(t.refreshQueue, refreshAt[T]{
		ts:  now.Add(t.period),
		key: key,
	})

	t.processQueue(now)
}

func (t *Tracker[T]) DegradedCount() int {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.processQueue(t.Now())
	return len(t.degraded)
}

func (t *Tracker[T]) Degraded() []T {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.processQueue(t.Now())
	keys := make([]T, 0, len(t.degraded))
	for k := range t.degraded {
		keys = append(keys, k)
	}
	return keys
}
