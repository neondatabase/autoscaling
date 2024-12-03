package initevents

// Reconcile middleware to allow us to know when all of a set of events have been handled.

import (
	"sync"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/plugin/reconcile"
)

// compile-time check that InitEventsMiddleware implements reconcile.Middleware
var _ reconcile.Middleware = (*InitEventsMiddleware)(nil)

// InitEventsMiddleware is middleware for reconcile.Queue that allows us to be notified when all of
// a set of events have been successfully processed.
//
// The initial setup of the scheduler plugin uses this to ensure that we don't make any decisions on
// partial state.
type InitEventsMiddleware struct {
	done       atomic.Bool
	notifyDone chan struct{}

	mu         sync.Mutex
	doneAdding bool
	remaining  map[reconcile.Key]struct{}
}

func NewInitEventsMiddleware() *InitEventsMiddleware {
	return &InitEventsMiddleware{
		done:       atomic.Bool{},
		notifyDone: make(chan struct{}),
		mu:         sync.Mutex{},
		doneAdding: false,
		remaining:  make(map[reconcile.Key]struct{}),
	}
}

// Call implements reconcile.Middleware.
func (m *InitEventsMiddleware) Call(
	logger *zap.Logger,
	params reconcile.MiddlewareParams,
	handler reconcile.MiddlewareHandlerFunc,
) (reconcile.Result, error) {
	result, err := handler(logger, params)

	if err == nil {
		m.success(params.Key())
	}

	return result, err
}

// AddRequired adds the object to the set that are required to reconcile successfully.
//
// This method can be called up until the first call to (*InitEventsMiddleware).Done(), after which
// the set of objects is sealed and further calls to this method will panic.
func (m *InitEventsMiddleware) AddRequired(obj reconcile.Object) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.doneAdding {
		panic("AddRequired() called after Done()")
	}

	k := reconcile.Key{
		GVK: obj.GetObjectKind().GroupVersionKind(),
		UID: obj.GetUID(),
	}
	m.remaining[k] = struct{}{}
}

// Done returns a channel that will be closed when all of the required objects have been
// successfully reconciled.
func (m *InitEventsMiddleware) Done() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.doneAdding = true
	m.checkDone()

	return m.notifyDone
}

// Remaining returns the set of objects that we're waiting on to be successfully reconciled.
func (m *InitEventsMiddleware) Remaining() []reconcile.Key {
	m.mu.Lock()
	defer m.mu.Unlock()

	var keys []reconcile.Key
	for k := range m.remaining {
		keys = append(keys, k)
	}
	return keys
}

// helper function for when reconciling is successful
func (m *InitEventsMiddleware) success(k reconcile.Key) {
	// fast path: don't do anything if we're already done, avoiding waiting on an extra lock.
	if m.done.Load() {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.remaining, k)
	m.checkDone()
}

// NOTE: this method expects that the caller has acquired m.mu.
func (m *InitEventsMiddleware) checkDone() {
	// we've already signaled that we're done. Avoid double-closing the channel.
	if m.done.Load() {
		return
	}

	if m.doneAdding && len(m.remaining) == 0 {
		close(m.notifyDone)
		m.done.Store(true)
	}
}
