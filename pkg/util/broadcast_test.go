package util_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neondatabase/autoscaling/pkg/util"
)

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestBroadcast(t *testing.T) {
	broadcast := util.NewBroadcaster()

	receiver := broadcast.NewReceiver()
	waitCh := receiver.Wait()

	// Not yet closed, no events yet
	require.False(t, closed(waitCh))

	// Send event, now should be closed, and continue to be closed on subsequent calls to Wait
	broadcast.Broadcast()
	require.True(t, closed(waitCh))
	require.True(t, closed(receiver.Wait()))

	// After we mark the event as received, we should go back to waiting
	receiver.Awake()

	waitCh = receiver.Wait()
	require.False(t, closed(waitCh))

	// Multiple events should get collapsed into one:
	broadcast.Broadcast()
	broadcast.Broadcast()
	require.True(t, closed(waitCh))
	receiver.Awake()
	require.False(t, closed(receiver.Wait()))

	// If we first call Wait() after the unreceived event has already happened, then it should
	// already be closed
	broadcast.Broadcast()
	require.True(t, closed(receiver.Wait()))

	// Creating a receiver after there's already been some events should behave like normal:
	receiver = broadcast.NewReceiver()
	require.False(t, closed(receiver.Wait()))
	broadcast.Broadcast()
	require.True(t, closed(receiver.Wait()))
	receiver.Awake()
	require.False(t, closed(receiver.Wait()))
}
