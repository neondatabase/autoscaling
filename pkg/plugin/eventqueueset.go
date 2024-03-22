package plugin

import (
	"context"
	"hash/fnv"

	"github.com/tychoish/fun/pubsub"
)

type eventQueueSet[T any] struct {
	queues []*pubsub.Queue[T]
}

func newEventQueueSet[T any](size int) eventQueueSet[T] {
	queues := make([]*pubsub.Queue[T], size)
	for i := 0; i < size; i += 1 {
		queues[i] = pubsub.NewUnlimitedQueue[T]()
	}
	return eventQueueSet[T]{queues: queues}
}

func (s eventQueueSet[T]) enqueue(key string, item T) error {
	hasher := fnv.New64()
	// nb: Hash guarantees that Write never returns an error
	_, _ = hasher.Write([]byte(key))
	hash := hasher.Sum64()

	idx := int(hash % uint64(len(s.queues)))
	return s.queues[idx].Add(item)
}

func (s eventQueueSet[T]) wait(ctx context.Context, idx int) (T, error) {
	return s.queues[idx].Wait(ctx)
}
