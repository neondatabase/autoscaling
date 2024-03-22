package plugin

import (
	"context"
	"hash/fnv"
	"time"

	"github.com/tychoish/fun/pubsub"
)

type queueItem[T any] struct {
	item    T
	addTime time.Time
}

type eventQueueSet[T any] struct {
	queues  []*pubsub.Queue[queueItem[T]]
	metrics PromMetrics
}

func newEventQueueSet[T any](size int, metrics PromMetrics) eventQueueSet[T] {
	queues := make([]*pubsub.Queue[queueItem[T]], size)
	for i := 0; i < size; i += 1 {
		queues[i] = pubsub.NewUnlimitedQueue[queueItem[T]]()
	}
	return eventQueueSet[T]{
		queues:  queues,
		metrics: metrics,
	}
}

func (s eventQueueSet[T]) enqueue(key string, item T) error {
	hasher := fnv.New64()
	// nb: Hash guarantees that Write never returns an error
	_, _ = hasher.Write([]byte(key))
	hash := hasher.Sum64()

	idx := int(hash % uint64(len(s.queues)))

	s.metrics.eventQueueDepth.Inc()
	s.metrics.eventQueueAddsTotal.Inc()
	queueItem := queueItem[T]{
		item:    item,
		addTime: time.Now(),
	}
	return s.queues[idx].Add(queueItem)
}

func (s eventQueueSet[T]) wait(ctx context.Context, idx int) (T, error) {
	queueItem, err := s.queues[idx].Wait(ctx)

	if err == nil {
		s.metrics.eventQueueDepth.Dec()
		s.metrics.eventQueueLatency.Observe(float64(time.Since(queueItem.addTime).Seconds()))
	}

	return queueItem.item, err
}
