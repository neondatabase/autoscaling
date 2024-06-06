package failurelag

import (
	"sync"
	"time"
)

var Now = time.Now

// Tracker accumulates failure events for a given key and determines if
// the key is degraded. The key becomes degraded if it receives only failures
// over a configurable pending period. Once the success event is received, the key
// is no longer considered degraded, and the pending period is reset.
type Tracker[T comparable] struct {
	period time.Duration

	pendingSince map[T]time.Time
	degraded     map[T]struct{}
	degradeAt    []degradeAt[T]

	lock sync.Mutex
}

type degradeAt[T comparable] struct {
	ts  time.Time
	key T
}

func NewTracker[T comparable](period time.Duration) *Tracker[T] {
	return &Tracker[T]{
		period:       period,
		pendingSince: make(map[T]time.Time),
		degraded:     make(map[T]struct{}),
		degradeAt:    []degradeAt[T]{},
		lock:         sync.Mutex{},
	}
}

// forward processes all the fireAt events that are now in the past.
func (t *Tracker[T]) forward(now time.Time) {
	i := 0
	for ; i < len(t.degradeAt); i++ {
		event := t.degradeAt[i]
		if event.ts.After(now) {
			break
		}
		pendingSince, ok := t.pendingSince[event.key]
		if !ok {
			// There was a success event in between
			continue
		}

		if event.ts.Sub(pendingSince) < t.period {
			// There was a success, and another failure in between
			// We will have another fireAt event for this key in the future
			continue
		}
		t.degraded[event.key] = struct{}{}
	}
	t.degradeAt = t.degradeAt[i:]
}

func (t *Tracker[T]) RecordSuccess(key T) {
	t.lock.Lock()
	defer t.lock.Unlock()

	delete(t.degraded, key)
	delete(t.pendingSince, key)
	t.forward(Now())
}

func (t *Tracker[T]) RecordFailure(key T) {
	t.lock.Lock()
	defer t.lock.Unlock()

	now := Now()

	if _, ok := t.pendingSince[key]; !ok {
		t.pendingSince[key] = now
	}

	t.degradeAt = append(t.degradeAt, degradeAt[T]{
		ts:  now.Add(t.period),
		key: key,
	})

	t.forward(now)
}

func (t *Tracker[T]) DegradedCount() int {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.forward(Now())
	return len(t.degraded)
}

func (t *Tracker[T]) Degraded() []T {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.forward(Now())
	keys := make([]T, 0, len(t.degraded))
	for k := range t.degraded {
		keys = append(keys, k)
	}
	return keys
}
