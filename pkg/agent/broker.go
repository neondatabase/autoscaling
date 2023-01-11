package agent

import (
	"context"
	"sync"
)

// stole this from https://stackoverflow.com/questions/36417199/how-to-broadcast-message-using-channel

type Broker[T any] struct {
	wg        sync.WaitGroup
	close     context.CancelFunc
	publishCh chan T
	subCh     chan chan T
	unsubCh   chan chan T
}

func NewBroker[T any]() *Broker[T] {
	return &Broker[T]{
		publishCh: make(chan T),
		subCh:     make(chan chan T),
		unsubCh:   make(chan chan T),
	}
}

func (b *Broker[T]) Start(ctx context.Context) {
	if b.close != nil {
		return
	}

	ctx, b.close = context.WithCancel(ctx)

	b.wg.Add(1)
	go func() {
		subs := map[chan T]struct{}{}
		for {
			select {
			case <-ctx.Done():
				return
			case msgCh := <-b.subCh:
				subs[msgCh] = struct{}{}
			case msgCh := <-b.unsubCh:
				delete(subs, msgCh)
			case msg := <-b.publishCh:
				for msgCh := range subs {
					select {
					case msgCh <- msg:
					case <-ctx.Done():
					}
				}
			}
		}
	}()
}

func (b *Broker[T]) Stop() { b.close() }

func (b *Broker[T]) Wait(ctx context.Context) {
	sig := make(chan struct{})
	go func() { defer close(sig); b.wg.Wait() }()

	select {
	case <-sig:
		b.close = nil
	case <-ctx.Done():
	}
}

func (b *Broker[T]) Subscribe(ctx context.Context) chan T {
	msgCh := make(chan T)
	select {
	case <-ctx.Done():
		return nil
	case b.subCh <- msgCh:
		return msgCh
	}
}

func (b *Broker[T]) Unsubscribe(ctx context.Context, msgCh chan T) {
	select {
	case <-ctx.Done():
	case b.unsubCh <- msgCh:
	}
}

func (b *Broker[T]) Publish(ctx context.Context, msg T) {
	select {
	case <-ctx.Done():
	case b.publishCh <- msg:
	}
}
