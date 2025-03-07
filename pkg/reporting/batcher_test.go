package reporting

import (
	"bytes"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
)

type csvBatchBuilder struct {
	buf     bytes.Buffer
	started bool
}

func (b *csvBatchBuilder) Add(event string) {
	if b.started {
		b.buf.Write([]byte{','})
	}
	b.buf.Write([]byte(event))
	b.started = true
}

func (b *csvBatchBuilder) Finish() []byte {
	return b.buf.Bytes()
}

func TestEventBatching(t *testing.T) {
	targetBatchSize := 3

	var notified bool
	notify := func() {
		notified = true
	}
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{})

	newBatch := func() BatchBuilder[string] {
		return &csvBatchBuilder{
			buf:     bytes.Buffer{},
			started: false,
		}
	}

	batcher := newEventBatcher(targetBatchSize, newBatch, notify, gauge)

	// First batch:
	// Add a small number of items to the batch, and then explicitly request early completion.
	batcher.enqueue("b1-1")
	batcher.enqueue("b1-2")
	// check that there is not anything completed:
	assert.Equal(t, false, notified)
	assert.Equal(t, 0, batcher.completedCount())
	// Request early completion:
	batcher.finishOngoing()
	// check that this batch was completed:
	assert.Equal(t, true, notified)
	assert.Equal(t, 1, batcher.completedCount())
	assert.Equal(t,
		batch[string]{count: 2, serialized: []byte("b1-1,b1-2")},
		batcher.peekLatestCompleted())
	// clear the current batch:
	notified = false
	batcher.dropLatestCompleted()

	// Second, third, and fourth batches:
	// Add enough events that three batches are automatically created of appropriate sizes, and
	// check that
	batcher.enqueue("b2-1")
	batcher.enqueue("b2-2")
	assert.Equal(t, false, notified)
	batcher.enqueue("b2-3")
	assert.Equal(t, true, notified)
	notified = false // reset the notification
	batcher.enqueue("b3-1")
	batcher.enqueue("b3-2")
	assert.Equal(t, false, notified)
	batcher.enqueue("b3-3")
	assert.Equal(t, true, notified)
	notified = false // reset the notification
	// check that the batches so far match what we expect:
	assert.Equal(t, 2, batcher.completedCount())
	assert.Equal(t,
		batch[string]{count: 3, serialized: []byte("b2-1,b2-2,b2-3")},
		batcher.peekLatestCompleted())
	// add the last batch:
	batcher.enqueue("b4-1")
	batcher.enqueue("b4-2")
	assert.Equal(t, false, notified)
	batcher.enqueue("b4-3")
	assert.Equal(t, true, notified)
	// Check that the final batches are what we expect
	assert.Equal(t, 3, batcher.completedCount())
	assert.Equal(t,
		batch[string]{count: 3, serialized: []byte("b2-1,b2-2,b2-3")},
		batcher.peekLatestCompleted())
	// Consume one batch:
	batcher.dropLatestCompleted()
	// and now, it should just be b3 and b4:
	assert.Equal(t, 2, batcher.completedCount())
	assert.Equal(t,
		batch[string]{count: 3, serialized: []byte("b3-1,b3-2,b3-3")},
		batcher.peekLatestCompleted())
	// consume b3:
	batcher.dropLatestCompleted()
	// ... so it should just be b4:
	assert.Equal(t, 1, batcher.completedCount())
	assert.Equal(t,
		batch[string]{count: 3, serialized: []byte("b4-1,b4-2,b4-3")},
		batcher.peekLatestCompleted())
	// and after consuming the last one...
	batcher.dropLatestCompleted()
	// ... there should be nothing left:
	assert.Equal(t, 0, batcher.completedCount())
}
