package util

// Signalling primitives: single-signal sender/receiver pair and sync.Cond-ish exposed over a
// channel instead

import (
	"sync"
)

func NewSingleSignalPair() (SignalSender, SignalReceiver) {
	sigCh := make(chan struct{})
	once := &sync.Once{}
	closeSigCh := func() { once.Do(func() { close(sigCh) }) }

	return SignalSender{send: closeSigCh}, SignalReceiver{sigCh: sigCh, closeSigCh: closeSigCh}
}

type SignalSender struct {
	send func()
}

type SignalReceiver struct {
	sigCh      chan struct{}
	closeSigCh func()
}

func (s SignalSender) Send() {
	s.send()
}

func (s SignalReceiver) Recv() chan struct{} {
	return s.sigCh
}

func (s SignalReceiver) Close() {
	s.closeSigCh()
}

// NewCondChannelPair creates a sender/receiver pair for a sync.Cond-like interface
//
// The differences from sync.Cond are that receiving is exposed through a channel (so it can be
// select-ed) and there is no equivalent to (*Cond).Broadcast()
func NewCondChannelPair() (CondChannelSender, CondChannelReceiver) {
	ch := make(chan struct{}, 1)
	return CondChannelSender{ch: ch}, CondChannelReceiver{ch: ch}
}

// CondChannelSender is the sending half of a sync.Cond-like interface
type CondChannelSender struct {
	ch chan struct{}
}

// CondChannelReceiver is the receiving half of a sync.Cond-like interface
type CondChannelReceiver struct {
	ch chan struct{}
}

// Send performs a non-blocking notify of the associated CondChannelReceiver
//
// If there is currently a receiver waiting via Recv, then this will immediately wake them.
// Otherwise, the next receive on the channel returned by Recv will complete immediately.
func (c *CondChannelSender) Send() {
	select {
	case c.ch <- struct{}{}:
	default:
	}
}

// Unsend cancels an existing signal that has been sent but not yet received.
//
// It returns whether there was a signal to be cancelled.
func (c *CondChannelSender) Unsend() bool {
	select {
	case <-c.ch:
		return true
	default:
		return false
	}
}

// Consume removes any existing signal created by Send, requiring an additional Send to be made
// before the receiving on Recv will unblock
//
// This method is non-blocking.
func (c *CondChannelReceiver) Consume() {
	select {
	case <-c.ch:
	default:
	}
}

// Recv returns a channel for which receiving will complete either (a) immediately, if Send has been
// called without Consume or another receive since; or (b) as soon as Send is next called
//
// This method is non-blocking but receiving on the returned channel may block.
func (c *CondChannelReceiver) Recv() <-chan struct{} {
	return c.ch
}
