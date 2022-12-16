package util

// Lightweight single-signal sender/receiver pair

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
