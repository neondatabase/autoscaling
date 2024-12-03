package reconcile

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/queue"
)

// Queue is the unified queue for managing and distributing reconcile operations for kubernetes
// objects
type Queue struct {
	mu sync.Mutex

	// queue is the changes that are due to be processed but have not yet been picked up by any
	// workers.
	queue queue.PriorityQueue[kv]
	// queued stores the handles for objects in the queue. This is needed so that we can update the
	// objects while they're in the queue, rather than requeueing on each change we receive from the
	// kubernetes API server.
	queued map[Key]queue.ItemHandle[kv]
	// pending stores the changes to objects that the Queue has received, but can't actually push to
	// the queue because there are ongoing operations for those objects that would cause conflicts.
	pending map[Key]value
	// ongoing tracks the set of all ongoing reconcile operations.
	// When we receive an update, we use ongoing to check whether we can add it to the queue, or if
	// we must instead wait for it to finish.
	ongoing map[Key]struct{}

	// next is a synchronous channel to distribute notifications that there are items in the queue.
	//
	// the sending half of the channel is owned by a separate goroutine running
	// (*Queue).handleNotifications().
	//
	// NOTE: This field is immutable.
	next <-chan struct{}

	// NOTE: This field is immutable.
	stopNotificationHandling func()
	// NOTE: This field is immutable.
	notifyEnqueued func()

	// NOTE: This field is immutable.
	handlers map[schema.GroupVersionKind]HandlerFunc

	// if not nil, a callback that records how long each item was waiting to be reconciled
	queueWaitCallback QueueWaitDurationCallback
}

type kv struct {
	k Key
	v value
}

// Key uniquely identifies a kubernetes object
type Key struct {
	GVK schema.GroupVersionKind
	UID types.UID
}

// value stores the information about a pending reconcile operation for a kubernetes object
type value struct {
	reconcileAt time.Time
	eventKind   EventKind
	object      Object
	handler     HandlerFunc
}

// HandlerFunc represents the signature of functions that will be called to reconcile objects in the
// queue.
//
// Handlers are reigstered in the call to NewQueue with the mapping from each type to its handler.
type HandlerFunc = func(*zap.Logger, EventKind, Object) (Result, error)

// Result is the outcome of reconciling, storing whether the reconcile operation should be retried
// (and if so, how long should we wait?)
type Result struct {
	// RetryAfter, if not zero, gives the duration that we should wait before retrying.
	//
	// RetryAfter != 0 implies Retry; there is no need to specify both.
	RetryAfter time.Duration
}

// NewQueue builds and returns a new Queue with the provided middleware and handlers for various
// types.
func NewQueue(handlers map[Object]HandlerFunc, opts ...QueueOption) (*Queue, error) {
	settings := defaultQueueSettings()

	for _, o := range opts {
		o.apply(settings)
	}

	types := []schema.GroupVersionKind{}
	handlersByType := make(map[schema.GroupVersionKind]HandlerFunc)
	for obj, handler := range handlers {
		// nb: second arg is whether the object is unversioned. That doesn't matter to us.
		gvk, err := util.LookupGVKForType(obj)
		if err != nil {
			return nil, err
		}

		// Check that this isn't a duplicate
		if _, ok := handlersByType[gvk]; ok {
			return nil, fmt.Errorf("Duplicate handler for object type %T with GVK %q", obj, fmtGVK(gvk))
		}

		handlersByType[gvk] = handler
		types = append(types, gvk)
	}

	middleware := defaultMiddleware(types, settings.resultCallback, settings.errorCallback, settings.panicCallback)
	middleware = append(middleware, settings.middleware...)

	// Apply middleware to all handlers
	enrichedHandlers := make(map[schema.GroupVersionKind]HandlerFunc)
	for gvk, handler := range handlersByType {
		enrichedHandlers[gvk] = applyMiddleware(middleware, handler)
	}

	next := make(chan struct{})
	ctx, cancel := context.WithCancel(settings.baseContext)

	enqueuedSndr := util.NewBroadcaster()
	enqueuedRcvr := enqueuedSndr.NewReceiver()

	q := &Queue{
		mu: sync.Mutex{},
		queue: queue.New(func(x, y kv) bool {
			return x.v.isHigherPriority(y.v)
		}),
		queued:  make(map[Key]queue.ItemHandle[kv]),
		pending: make(map[Key]value),
		ongoing: make(map[Key]struct{}),

		next: next,

		// note: context.WithCancel returns a thread-safe cancel function.
		stopNotificationHandling: cancel,
		notifyEnqueued:           enqueuedSndr.Broadcast,

		handlers: enrichedHandlers,

		queueWaitCallback: settings.waitCallback,
	}

	go q.handleNotifications(ctx, next, enqueuedRcvr)

	return q, nil
}

func (q *Queue) handleNotifications(ctx context.Context, next chan<- struct{}, enqueued util.BroadcastReceiver) {
	done := ctx.Done()

	timer := time.NewTimer(0)

	for {
		timer.Stop()

		// Check if the context is done. If so, bail early.
		select {
		case <-done:
			return
		default:
		}

		// Wait until we can send a notification:
		var deadline time.Time
		func() {
			q.mu.Lock()
			defer q.mu.Unlock()

			nextKV, ok := q.queue.Peek()
			if !ok {
				return
			}

			deadline = nextKV.v.reconcileAt
			if deadline.IsZero() {
				panic("item in queue has unexpected zero deadline")
			}
		}()

		if deadline.IsZero() {
			// Nothing in the queue. Wait until there is something.
			select {
			case <-done:
				return
			case <-enqueued.Wait():
				enqueued.Awake() // record that we got the message
				continue         // go through the loop again, so we get non-zero deadline.
			}
		}

		now := time.Now()
		waitDuration := deadline.Sub(now)
		if waitDuration > 0 {
			timer.Reset(waitDuration)

			// Sleep until the deadline is reached.
			select {
			case <-done:
				return
			case <-enqueued.Wait():
				enqueued.Awake() // record that we got the message
				continue         // go through the loop again, in case the deadline changed.
			case <-timer.C:
				// we reached the deadline; we can wake up a worker to handle the item.
			}
		}

		// Message that there's something to be handled
		select {
		case <-done:
			return
		case next <- struct{}{}:
		}
	}
}

// Stop ceases the distribution of new reconcile operations, and additionally cleans up the
// long-running goroutine that's responsible for dispatching notifications.
//
// For usage with contexts, refer to the WithBaseContext QueueOption.
func (q *Queue) Stop() {
	q.stopNotificationHandling()
}

// ReconcileCallback represents the signature of functions that are handed out to reconcile individual items
//
// Callbacks are returned by calls to (Worker).Next().
type ReconcileCallback = func(*zap.Logger)

// WaitChan returns a channel on which at least one empty struct will be sent for each item waiting
// to be reconciled (note that sometimes there may be spurious wake-ups!)
//
// The channel is shared and persistent, and only closed when (*Queue).Stop() is called or the base
// context (if provided) is canceled.
func (q *Queue) WaitChan() <-chan struct{} {
	return q.next
}

func (v value) isHigherPriority(other value) bool {
	return v.reconcileAt.Before(other.reconcileAt)
}

func fmtGVK(gvk schema.GroupVersionKind) string {
	if gvk.Empty() {
		return "<empty>"
	} else if gvk.Group == "" {
		// v1 handling
		return fmt.Sprintf("%s.%s", gvk.Version, gvk.Kind)
	} else {
		return fmt.Sprintf("%s/%s.%s", gvk.Group, gvk.Version, gvk.Kind)
	}
}

type Object interface {
	runtime.Object
	metav1.Object
}

func (q *Queue) Enqueue(eventKind EventKind, obj Object) {
	now := time.Now()
	gvk := obj.GetObjectKind().GroupVersionKind()

	// Fetch the handler for this object. This doubles as checking that the type is known.
	// We want to do this up front, so that the callstack is clear if this fails.
	//
	// Note: this doesn't require locking the queue because the handlers field is immutable.
	handler, ok := q.handlers[gvk]
	if !ok {
		panic(fmt.Sprintf("unknown object GVK %s of type %T", fmtGVK(gvk), obj))
	}

	k := Key{
		GVK: gvk,
		UID: obj.GetUID(),
	}

	v := value{
		reconcileAt: now, // reconcile as soon as possible
		eventKind:   eventKind,
		object:      obj,
		handler:     handler,
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	// If the type is already being reconciled, we should store the update in 'pending'
	_, ongoingReconcile := q.ongoing[k]
	if ongoingReconcile {
		q.enqueuePendingChange(k, v)
	} else {
		q.enqueueInactive(k, v)
	}
}

// Next returns a callback to execute the next waiting reconcile operation in the queue, or false if
// there are none.
func (q *Queue) Next() (_ ReconcileCallback, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	kv, ok := q.queue.Peek()
	if !ok || kv.v.reconcileAt.After(time.Now()) {
		return nil, false
	}
	q.queue.Pop()
	delete(q.queued, kv.k)

	// mark the item as ongoing, and then return it:
	q.ongoing[kv.k] = struct{}{}

	callback := func(logger *zap.Logger) {
		q.reconcile(logger, kv.k, kv.v)
	}

	return callback, true
}

// reconcile is the outermost function that is called in order to reconcile an object.
//
// It calls the outermost middleware, which in turn calls the next, and so forth, until the original
// handler is run.
func (q *Queue) reconcile(logger *zap.Logger, k Key, v value) {
	if q.queueWaitCallback != nil {
		wait := time.Since(v.reconcileAt)
		q.queueWaitCallback(wait)
	}

	// Noteworthy functionality that we don't need to worry about here:
	//
	// - Catching panics is already handled by CatchPanicMiddleware.
	// - Retry backoff for errors is already handled by ErrorBackoffMiddleware.
	// - Logging the result is handled by LogResultMiddleware (so, we can ignore the error)
	//
	// All of these are included by default.
	result, _ := v.handler(logger, v.eventKind, v.object)

	requeue := result.RetryAfter != 0
	if requeue {
		retryAt := time.Now().Add(result.RetryAfter)

		// Now that we know when we're retrying, let's schedule that!
		v = value{
			reconcileAt: retryAt,
			eventKind:   v.eventKind,
			object:      v.object,
			handler:     v.handler,
		}
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.finishAndMaybeRequeue(k, v, requeue)
}

func (v value) mergeWithNewer(newer value) value {
	var reconcileAt time.Time
	if v.reconcileAt.Before(newer.reconcileAt) {
		reconcileAt = v.reconcileAt
	} else {
		reconcileAt = newer.reconcileAt
	}

	return value{
		reconcileAt: reconcileAt,
		eventKind:   v.eventKind.Merge(newer.eventKind),
		object:      newer.object,
		handler:     newer.handler,
	}
}

// enqueues a change to an object that is already being reconciled.
//
// NOTE: this method assumes that the caller has acquired q.mu.
func (q *Queue) enqueuePendingChange(k Key, v value) {
	// if there's already something pending, merge with that:
	if pendingValue, ok := q.pending[k]; ok {
		v = pendingValue.mergeWithNewer(v)
	}
	q.pending[k] = v
}

// enqueues a change to an object that is not currently being reconciled
//
// NOTE: this method assumes that the caller has acquired q.mu.
func (q *Queue) enqueueInactive(k Key, v value) {
	// if there's already something in the queue, just merge with that:
	if queuedHandle, ok := q.queued[k]; ok {
		queuedHandle.Update(func(queuedValue *kv) {
			queuedValue.v = queuedValue.v.mergeWithNewer(v)
		})
		// the value of reconcileAt for the item may have changed; we should notify just in case, so
		// it's not waiting.
		q.notifyEnqueued()
		return
	}

	// ... otherwise, add it to the queue!
	handle := q.queue.Push(kv{k, v})
	q.queued[k] = handle
	// and make sure that someone picks it up:
	q.notifyEnqueued()
}

// finalizes the state for an object that has just finished being reconciled, and requeues
//
// NOTE: this method assumes that the caller has acquired q.mu.
func (q *Queue) finishAndMaybeRequeue(k Key, v value, requeue bool) {
	// First, mark the item as no longer in progress:
	delete(q.ongoing, k)

	// Then, merge with anything pending, if we should requeue
	// Note that if there IS something pending, then it's actually newer than this value, so we
	// merge backwards to how we normally would.
	if pendingValue, ok := q.pending[k]; ok {
		if requeue {
			// we should merge, because explicit requeueing was requested
			v = v.mergeWithNewer(pendingValue)
		} else {
			// just use what was in pending -- but mark that we should actually requeue, because
			// there are pending changes.
			v = pendingValue
			requeue = true
		}
		delete(q.pending, k)
	}

	// now that everything has been cleared, we can actually add it to the queue, if desired
	if requeue {
		handle := q.queue.Push(kv{k, v})
		q.queued[k] = handle
		// ... and make sure someone picks it up:
		q.notifyEnqueued()
	}
}
