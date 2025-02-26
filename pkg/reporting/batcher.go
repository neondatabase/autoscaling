package reporting

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

type eventBatcher[E any] struct {
	mu sync.Mutex

	targetBatchSize int

	ongoing []E

	completed     []batch[E]
	onComplete    func()
	completedSize int

	sizeGauge prometheus.Gauge
}

type batch[E any] struct {
	events []E
}

func newEventBatcher[E any](
	targetBatchSize int,
	notifyCompletedBatch func(),
	sizeGauge prometheus.Gauge,
) *eventBatcher[E] {
	return &eventBatcher[E]{
		mu: sync.Mutex{},

		targetBatchSize: targetBatchSize,

		ongoing: []E{},

		completed:     []batch[E]{},
		onComplete:    notifyCompletedBatch,
		completedSize: 0,

		sizeGauge: sizeGauge,
	}
}

// enqueue adds an event to the current in-progress batch.
//
// If the target batch size is reached, the batch will be packaged up for consumption by
// (*eventBatcher[E]).peekCompleted() and b.onComplete() will be called.
func (b *eventBatcher[E]) enqueue(event E) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.ongoing = append(b.ongoing, event)
	b.updateGauge()

	if len(b.ongoing) >= b.targetBatchSize {
		b.finishCurrentBatch()
	}
}

// finishOngoing collects any events that have not yet been packaged up into a batch, adding them to
// a batch visible in (*eventBatcher[E]).peekCompleted().
//
// If there are outstanding events when this method is called, b.onComplete() will be called.
// Otherwise, it will not be called.
func (b *eventBatcher[E]) finishOngoing() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.ongoing) == 0 {
		return
	}

	b.finishCurrentBatch()
}

// completedCount returns the number of completed batches
func (b *eventBatcher[E]) completedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.completed)
}

// peekLatestCompleted returns the most recently completed batch that has not yet been removed by
// (*eventBatcher[E]).dropLatestCompleted().
//
// The batcher is not modified by this call.
//
// Once done with this batch, you should call (*eventBatcher[E]).dropLatestCompleted() to remove it
// from future consideration.
func (b *eventBatcher[E]) peekLatestCompleted() batch[E] {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.completed[0]
}

// dropLatestCompleted drops the most recently completed batch from internal storage.
//
// This method will panic if (*eventBatcher[E]).completedCount() is zero.
func (b *eventBatcher[e]) dropLatestCompleted() {
	b.mu.Lock()
	defer b.mu.Unlock()

	dropped := b.completed[0]
	b.completed = b.completed[1:]
	b.completedSize -= len(dropped.events)

	b.updateGauge()
}

// NB: must hold mu
func (b *eventBatcher[E]) updateGauge() {
	b.sizeGauge.Set(float64(len(b.ongoing) + b.completedSize))
}

// NB: must hold mu
func (b *eventBatcher[E]) finishCurrentBatch() {
	b.completed = append(b.completed, batch[E]{
		events: b.ongoing,
	})

	b.completedSize += len(b.ongoing)
	b.ongoing = []E{}

	b.onComplete()
}
