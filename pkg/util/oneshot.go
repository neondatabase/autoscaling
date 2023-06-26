package util

type OneshotSender[T any] struct {
	inner chan<- T
	sent  bool
}

type OneshotReceiver[T any] struct {
	inner    <-chan T
	received bool
}

func Oneshot[T any]() (OneshotSender[T], OneshotReceiver[T]) {
	c := make(chan T, 1)
	return OneshotSender[T]{inner: c, sent: false}, OneshotReceiver[T]{inner: c, received: false}
}

func (sender OneshotSender[T]) Send(t T) {
	if sender.sent {
		panic("Attempt to send on oneshot channel more than once")
	}
	sender.sent = true
	sender.inner <- t
}

func (receiver OneshotReceiver[T]) Recv() T {
	if receiver.received {
		panic("Attempt to receive on oneshot channel more than once")
	}
	receiver.received = true
	return <-receiver.inner
}
