package failurelag

import (
	"sync"
	"time"

	"github.com/samber/lo"
)

// Tracker accumulates failure events for a given key and determines if
// the key is degraded. The key becomes degraded if it receives only failures
// over a configurable pending period. Once the success event is received, the key
// is no longer considered degraded, and the pending period is reset.
type Tracker[T comparable] struct {
	period time.Duration

	firstFailure map[T]time.Time
	lastFailure  map[T]time.Time
	refreshQueue []refreshAt[T]

	degradedRetried    map[T]struct{}
	degradedNotRetried map[T]struct{}

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
		lastFailure:  make(map[T]time.Time),
		refreshQueue: make([]refreshAt[T], 0),

		degradedRetried:    make(map[T]struct{}),
		degradedNotRetried: make(map[T]struct{}),

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
			// There was a success event in between event.ts and now
			continue
		}

		if event.ts.Sub(firstFailure) < t.period {
			// There was a success, and another failure in between event.ts and now
			// We will have another fireAt event for this key in the future
			continue
		}

		if event.ts.Sub(t.lastFailure[event.key]) < t.period {
			// There was a failure in between event.ts and now
			t.degradedRetried[event.key] = struct{}{}
		} else {
			// There were no more events for this key in between event.ts and now
			delete(t.degradedRetried, event.key)
			t.degradedNotRetried[event.key] = struct{}{}
		}
	}
	t.refreshQueue = t.refreshQueue[i:]
}

func (t *Tracker[T]) RecordSuccess(key T) {
	t.lock.Lock()
	defer t.lock.Unlock()

	delete(t.degradedRetried, key)
	delete(t.degradedNotRetried, key)

	delete(t.firstFailure, key)
	delete(t.lastFailure, key)

	t.processQueue(t.Now())
}

func (t *Tracker[T]) RecordFailure(key T) {
	t.lock.Lock()
	defer t.lock.Unlock()

	now := t.Now()

	if _, ok := t.firstFailure[key]; !ok {
		t.firstFailure[key] = now
	}

	t.lastFailure[key] = now
	if _, ok := t.degradedNotRetried[key]; ok {
		// We need to move the key from degradedNotRetried to degradedRetried
		t.degradedRetried[key] = struct{}{}
		delete(t.degradedNotRetried, key)
	}

	// We always add refresh event, even if the key is already degraded.
	// If there were no more retries since, we will also mark the key as degradedNonRetried.
	t.refreshQueue = append(t.refreshQueue, refreshAt[T]{
		ts:  now.Add(t.period),
		key: key,
	})

	t.processQueue(now)
}

func (t *Tracker[T]) DegradedCount() int {
	return t.DegradedRetriedCount() + t.DegradedNotRetriedCount()
}

func (t *Tracker[T]) DegradedRetriedCount() int {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.processQueue(t.Now())
	return len(t.degradedRetried)
}

func (t *Tracker[T]) DegradedNotRetriedCount() int {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.processQueue(t.Now())
	return len(t.degradedNotRetried)
}

func (t *Tracker[T]) Degraded() []T {
	return append(t.DegradedRetried(), t.DegradedNotRetried()...)
}

func (t *Tracker[T]) DegradedRetried() []T {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.processQueue(t.Now())
	return lo.Keys(t.degradedRetried)
}

func (t *Tracker[T]) DegradedNotRetried() []T {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.processQueue(t.Now())
	return lo.Keys(t.degradedNotRetried)
}
