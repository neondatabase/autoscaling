package reporting

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// BatchBuilder is an interface for gradually converting []E to []byte, allowing us to construct
// batches of events without buffering them uncompressed, in memory.
//
// Implementations of BatchBuilder are defined in various 'batch_*.go' files.
type BatchBuilder[E any] interface {
	// Add appends an event to the in-progress batch.
	Add(event E)
	// Finish completes the in-progress batch, returning the events serialized as bytes.
	Finish() []byte
}

type eventBatcher[E any] struct {
	mu sync.Mutex

	targetBatchSize int

	newBatch    func() BatchBuilder[E]
	ongoing     BatchBuilder[E]
	ongoingSize int

	completed     []batch[E]
	onComplete    func()
	completedSize int

	sizeGauge prometheus.Gauge
}

type batch[E any] struct {
	serialized []byte
	count      int
}

func newEventBatcher[E any](
	targetBatchSize int,
	newBatch func() BatchBuilder[E],
	notifyCompletedBatch func(),
	sizeGauge prometheus.Gauge,
) *eventBatcher[E] {
	return &eventBatcher[E]{
		mu: sync.Mutex{},

		targetBatchSize: targetBatchSize,

		newBatch:    newBatch,
		ongoing:     newBatch(),
		ongoingSize: 0,

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

	b.ongoing.Add(event)
	b.ongoingSize += 1
	b.updateGauge()

	if b.ongoingSize >= b.targetBatchSize {
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

	if b.ongoingSize == 0 {
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

	batch := b.completed[0]
	b.completed = b.completed[1:]
	b.completedSize -= batch.count

	b.updateGauge()
}

// NB: must hold mu
func (b *eventBatcher[E]) updateGauge() {
	b.sizeGauge.Set(float64(b.ongoingSize + b.completedSize))
}

// NB: must hold mu
func (b *eventBatcher[E]) finishCurrentBatch() {
	b.completed = append(b.completed, batch[E]{
		serialized: b.ongoing.Finish(),
		count:      b.ongoingSize,
	})

	b.completedSize += b.ongoingSize
	b.ongoingSize = 0
	b.ongoing = b.newBatch()

	b.onComplete()
}
