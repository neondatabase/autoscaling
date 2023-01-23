package util

// Implementation of a channel-based mutex, so that it can be combined with Context.Done and other
// select-able methods, without dealing with the hassle of creating separate goroutines

import (
	"context"
)

// ChanMutex is a select-able mutex
//
// It is fair if and only if receiving on a channel is fair. As of Go 1.19/2022-01-17, receiving on
// a channel appears to be fair. However: this is a runtime implementation detail, and so it may
// change without notice in the future.
//
// Unlike sync.Mutex, ChanMutex requires initialization before use because it's basically just a
// channel.
//
// Also unlike sync.Mutex, a ChanMutex may be copied without issue (again, because it's just a
// channel).
type ChanMutex struct {
	ch chan struct{}
}

// NewChanMutex creates a new ChanMutex
func NewChanMutex() ChanMutex {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	return ChanMutex{ch}
}

// Lock locks m
//
// This method is semantically equivalent to sync.Mutex.Lock
func (m *ChanMutex) Lock() {
	if m.ch == nil {
		panic("called Lock on uninitialized ChanMutex")
	}
	<-m.ch
}

// WaitLock is like Lock, but instead returns a channel
//
// If receiving on the channel succeeds, the caller "holds" the lock and must now be responsible for
// Unlock-ing it.
func (m *ChanMutex) WaitLock() <-chan struct{} {
	if m.ch == nil {
		panic("called WaitLock on uninitialized ChanMutex")
	}
	return m.ch
}

// TryLock blocks until locking m succeeds or the context is cancelled
//
// If the context is cancelled while waiting to lock m, the lock will be left unchanged and
// ctx.Err() will be returned.
func (m *ChanMutex) TryLock(ctx context.Context) error {
	if m.ch == nil {
		panic("called TryLock on uninitialized ChanMutex")
	}
	select {
	case <-m.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock unlocks m
//
// This method is semantically equivalent to sync.Mutex.Unlock
func (m *ChanMutex) Unlock() {
	select {
	case m.ch <- struct{}{}:
	default:
		panic("ChanMutex.Unlock called while already unlocked")
	}
}
