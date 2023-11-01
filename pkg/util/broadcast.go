package util

// A channel-based sync.Cond-like interface, with support for broadcast operations (but some
// additional restrictions). Refer to the documentation of Wait for detailed usage.

import (
	"sync"
)

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		mu:   sync.Mutex{},
		ch:   make(chan struct{}),
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

// Broadcast sends a signal to all receivers
func (b *Broadcaster) Broadcast() {
	b.mu.Lock()
	defer b.mu.Unlock()

	close(b.ch)
	b.ch = make(chan struct{})
	b.sent += 1
}

// NewReceiver creates a new BroadcastReceiver that will receive only future broadcasted events.
//
// It's generally not recommended to call (*BroadcastReceiver).Wait() on a single BroadcastReceiver
// from more than one thread at a time, although it *is* thread-safe.
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

// Wait returns a channel that will be closed once there has been an event broadcasted since
// the BroadcastReceiver was created, or the last call to Awake().
//
// Typical usage of Wait will involve selecting on the channel returned and calling Awake
// immediately in the branch handling the event, for example:
//
//	select {
//	case <-ctx.Done():
//	    return
//	case <-receiver.Wait():
//	    receiver.Awake()
//	    ...
//	}
func (r *BroadcastReceiver) Wait() <-chan struct{} {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()

	if r.b.sent == r.viewed {
		return r.b.ch
	} else {
		return closedChannel
	}
}

// Awake marks the most recent broadcast event as received, so that the next call to Wait returns a
// channel that will only be closed once there's been a new event after this call to Awake.
func (r *BroadcastReceiver) Awake() {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()

	r.viewed = r.b.sent
}
