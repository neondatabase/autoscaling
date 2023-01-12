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

func NewCondChannelPair() (CondChannelSender, CondChannelReceiver) {
	ch := make(chan struct{}, 1)
	return CondChannelSender{ch: ch}, CondChannelReceiver{ch: ch}
}

type CondChannelSender struct {
	ch chan struct{}
}

type CondChannelReceiver struct {
	ch chan struct{}
}

func (c *CondChannelSender) Send() {
	select {
	case c.ch <- struct{}{}:
	default:
	}
}

func (c *CondChannelReceiver) Consume() {
	select {
	case <-c.ch:
	default:
	}
}

func (c *CondChannelReceiver) Recv() <-chan struct{} {
	return c.ch
}
