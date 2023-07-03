package util

// A channel-based sync.Cond-like interface, with support for broadcast operations (but some
// additional restrictions)

import (
	"sync"
)

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		mu:   sync.Mutex{},
		ch:   make(chan struct{}, 1),
		sent: 0,
	}
}

type Broadcaster struct {
	mu sync.Mutex
	ch chan struct{}

	sent uint64
}

type BroadcastReceiver struct {
	b *Broadcaster

	viewed uint64
}

func (b *Broadcaster) Broadcast() {
	b.mu.Lock()
	defer b.mu.Unlock()

	close(b.ch)
	b.ch = make(chan struct{}, 1)
	b.sent += 1
}

func (b *Broadcaster) NewReceiver() BroadcastReceiver {
	b.mu.Lock()
	defer b.mu.Unlock()

	return BroadcastReceiver{
		b:      b,
		viewed: b.sent,
	}
}

var closedChannel = func() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

func (r *BroadcastReceiver) Wait() <-chan struct{} {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()

	if r.b.sent == r.viewed {
		return r.b.ch
	} else {
		return closedChannel
	}
}

func (r *BroadcastReceiver) Awake() {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()

	r.viewed = r.b.sent
}
