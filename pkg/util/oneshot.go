package util

type OneshotSender[T any] struct {
	inner chan<- T
	sent  bool
}

type OneshotReceiver[T any] struct {
	inner    <-chan T
	received bool
}

// Create a oneshot channel, a la tokio::oneshot.
func Oneshot[T any]() (OneshotSender[T], OneshotReceiver[T]) {
	c := make(chan T, 1)
	return OneshotSender[T]{inner: c, sent: false}, OneshotReceiver[T]{inner: c, received: false}
}

// Send a message. Trying to send a multiple times will panic.
func (sender *OneshotSender[T]) Send(t T) {
	if sender.sent {
		panic("Attempt to send on oneshot channel more than once")
	}
	sender.sent = true
	sender.inner <- t
}

// Receive a message. Trying to receive multiple times will panic.
func (receiver *OneshotReceiver[T]) Recv() T {
	if receiver.received {
		panic("Attempt to receive on oneshot channel more than once")
	}
	receiver.received = true
	return <-receiver.inner
}
