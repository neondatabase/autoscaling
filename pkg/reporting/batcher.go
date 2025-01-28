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

// peekCompleted returns the batches that have been completed but not yet dropped, without modifying
// the batcher.
//
// Once done with these batches, you should call (*eventBatcher[E]).dropCompleted() to remove them
// from future consideration.
func (b *eventBatcher[E]) peekCompleted() []batch[E] {
	b.mu.Lock()
	defer b.mu.Unlock()

	tmp := make([]batch[E], len(b.completed))
	copy(tmp, b.completed)
	return tmp
}

// dropCompleted drops the given number of batches from internal storage, removing them from view in
// (*eventBatcher[E]).peekCompleted().
func (b *eventBatcher[e]) dropCompleted(batchCount int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	dropped := b.completed[:batchCount]
	b.completed = b.completed[batchCount:]

	for _, batch := range dropped {
		b.completedSize -= len(batch.events)
	}

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
