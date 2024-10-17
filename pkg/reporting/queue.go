package reporting

// Implementation of the event queue for mediating event generation and event sending.
//
// The "public" (ish - it's all one package) types are eventQueuePuller and eventQueuePusher, two
// halves of the same queue. Each half is only safe for use from a single thread, but *together*
// they can be used in separate threads.

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/exp/slices"
)

type eventQueueInternals[E any] struct {
	mu        sync.Mutex
	items     []E
	sizeGauge prometheus.Gauge
}

type eventQueuePuller[E any] struct {
	internals *eventQueueInternals[E]
}

type eventQueuePusher[E any] struct {
	internals *eventQueueInternals[E]
}

func newEventQueue[E any](sizeGauge prometheus.Gauge) (eventQueuePusher[E], eventQueuePuller[E]) {
	internals := &eventQueueInternals[E]{
		mu:        sync.Mutex{},
		items:     nil,
		sizeGauge: sizeGauge,
	}
	return eventQueuePusher[E]{internals}, eventQueuePuller[E]{internals}
}

// NB: must hold mu
func (qi *eventQueueInternals[E]) updateGauge() {
	qi.sizeGauge.Set(float64(len(qi.items)))
}

func (q eventQueuePusher[E]) enqueue(events ...E) {
	q.internals.mu.Lock()
	defer q.internals.mu.Unlock()

	q.internals.items = append(q.internals.items, events...)
	q.internals.updateGauge()
}

func (q eventQueuePuller[E]) size() int {
	q.internals.mu.Lock()
	defer q.internals.mu.Unlock()

	return len(q.internals.items)
}

func (q eventQueuePuller[E]) get(limit int) []E {
	q.internals.mu.Lock()
	defer q.internals.mu.Unlock()

	count := min(limit, len(q.internals.items))
	// NOTE: this kind of access escaping the mutex is only sound because this access is only
	// granted to the puller, and there's only one puller, and it isn't sound to use the output of a
	// previous get() after calling drop().
	return q.internals.items[:count]
}

func (q eventQueuePuller[E]) drop(count int) {
	q.internals.mu.Lock()
	defer q.internals.mu.Unlock()

	q.internals.items = slices.Replace(q.internals.items, 0, count)
	q.internals.updateGauge()
}
