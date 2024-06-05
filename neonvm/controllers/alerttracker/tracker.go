package alerttracker

import (
	"sync"
	"time"
)

var Now = time.Now

// Tracker is a struct that keeps track of failure/success events for a given key.
// Failure state over a given interval will trigger a firing alert.
type Tracker[T comparable] struct {
	interval time.Duration

	pendingSince map[T]time.Time
	firing       map[T]struct{}
	fireAt       []fireAt[T]

	lock sync.Mutex
}

type fireAt[T comparable] struct {
	ts  time.Time
	key T
}

func NewTracker[T comparable](interval time.Duration) *Tracker[T] {
	return &Tracker[T]{
		interval:     interval,
		pendingSince: make(map[T]time.Time),
		firing:       make(map[T]struct{}),
		fireAt:       []fireAt[T]{},
		lock:         sync.Mutex{},
	}
}

// forward processes all the fireAt events that are now in the past.
func (t *Tracker[T]) forward(now time.Time) {
	i := 0
	for ; i < len(t.fireAt); i++ {
		event := t.fireAt[i]
		if event.ts.After(now) {
			break
		}
		pendingSince, ok := t.pendingSince[event.key]
		if !ok {
			// There was a success event in between
			continue
		}

		if event.ts.Sub(pendingSince) < t.interval {
			// There was a success, and another failure in between
			// We will have another fireAt event for this key in the future
			continue
		}
		t.firing[event.key] = struct{}{}
	}
	t.fireAt = t.fireAt[i:]
}

func (t *Tracker[T]) RecordSuccess(key T) {
	t.lock.Lock()
	defer t.lock.Unlock()

	delete(t.firing, key)
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

	t.fireAt = append(t.fireAt, fireAt[T]{
		ts:  now.Add(t.interval),
		key: key,
	})

	t.forward(now)
}

func (t *Tracker[T]) FiringCount() int {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.forward(Now())
	return len(t.firing)
}

func (t *Tracker[T]) Firing() []T {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.forward(Now())
	keys := make([]T, 0, len(t.firing))
	for k := range t.firing {
		keys = append(keys, k)
	}
	return keys
}
