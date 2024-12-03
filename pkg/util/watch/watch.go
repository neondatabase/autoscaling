package watch

import (
	"context"
	"errors"
	"fmt"
	stdruntime "runtime"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/neondatabase/autoscaling/pkg/util"
)

// Client is implemented by the specific interfaces of kubernetes clients, like
// `Clientset.CoreV1().Pods(namespace)` or `..Nodes()`
//
// This interface should be *already implemented* by whatever the correct client is.
type Client[L any] interface {
	List(context.Context, metav1.ListOptions) (L, error)
	Watch(context.Context, metav1.ListOptions) (watch.Interface, error)
}

// Config is the miscellaneous configuration used by Watch
type Config struct {
	// ObjectNameLogField determines the key given to the logger to use when describing the type
	// being watched -- for example, "pod" or "virtualmachine"
	//
	// This can help with standardizing keys between the watcher and everything else using it.
	ObjectNameLogField string

	// Metrics will be used by the Watch call to report some information about its internal
	// operations
	//
	// Refer to the Metrics and MetricsConfig types for more information.
	Metrics MetricsConfig

	// RetryRelistAfter gives a retry interval when a re-list fails. If left nil, then Watch will
	// not retry.
	RetryRelistAfter *util.TimeRange
	// RetryWatchAfter gives a retry interval when a non-initial watch fails. If left nil, then
	// Watch will not retry.
	RetryWatchAfter *util.TimeRange
}

// Accessors provides the "glue" functions for Watch to go from a list L (returned by the
// client's List) to the underlying slice of items []T
type Accessors[L any, T any] struct {
	Items func(L) []T
}

// Object is implemented by pointers to T, where T is typically the resource that we're
// actually watching.
//
// Example implementers: *corev1.Pod, *corev1.Node
type Object[T any] interface {
	~*T
	runtime.Object
	metav1.ObjectMetaAccessor
}

// HandlerFuncs provides the set of callbacks to use for events from Watch
type HandlerFuncs[P any] struct {
	AddFunc    func(obj P, preexisting bool)
	UpdateFunc func(oldObj P, newObj P)
	DeleteFunc func(obj P, mayBeStale bool)
}

// Index represents types that provide some kind of additional index on top of the base listing
//
// Indexing is functionally implemented in the same way that WatchHandlerFuncs is, with the main
// difference being that more things are done for you with WatchIndexes. In particular, indexes can
// be added and removed after the Watch has already started, and the locking behavior is explicit.
type Index[T any] interface {
	Add(obj *T)
	Update(oldObj, newObj *T)
	Delete(obj *T)
}

// InitMode dictates the behavior of Watch with respect to any initial calls to
// handlers.AddFunc before returning
//
// If set to InitWatchModeSync, then AddFunc will be called while processing the initial listing,
// meaning that the returned WatchStore is guaranteed contain the state of the cluster (although it
// may update before any access).
//
// Otherwise, if set to InitWatchModeDefer, then AddFunc will not be called until after Watch
// returns. Correspondingly, the WatchStore will not update until then either.
type InitMode string

const (
	InitModeSync  InitMode = "sync"
	InitModeDefer InitMode = "defer"
)

// Watch starts a goroutine for watching events, using the provided WatchHandlerFuncs as the
// callbacks for each type of event.
//
// The type C is the kubernetes client we use to get the objects, L representing a list of these,
// T representing the object type, and P as a pointer to T.
func Watch[C Client[L], L metav1.ListMetaAccessor, T any, P Object[T]](
	ctx context.Context,
	logger *zap.Logger,
	client C,
	config Config,
	accessors Accessors[L, T],
	mode InitMode,
	listOpts metav1.ListOptions,
	handlers HandlerFuncs[P],
) (*Store[T], error) {
	if accessors.Items == nil {
		panic(errors.New("accessors.Items == nil"))
	}

	// Workaround for https://github.com/kubernetes/kubernetes/issues/98925 :
	//
	// Pre-calculate the GVK for the object types, because List() operations only set the
	// Kind+APIVersion on the List type, and not the individual elements.
	sampleObj := P(new(T))
	gvk, err := util.LookupGVKForType(sampleObj)
	if err != nil {
		return nil, err
	}

	// do the conversion from P -> *T. We wanted the handlers to be provided with P so that the
	// caller doesn't need to manually specify the generics, but in order to store the callbacks
	// inside the watch store, we need to convert them so we're not carrying around more generic
	// parameters than we need.
	actualHandlers := HandlerFuncs[*T]{
		AddFunc:    func(obj *T, preexisting bool) {},
		UpdateFunc: func(oldObj, newObj *T) {},
		DeleteFunc: func(obj *T, mayBeStale bool) {},
	}
	if handlers.AddFunc != nil {
		actualHandlers.AddFunc = func(obj *T, preexisting bool) {
			handlers.AddFunc(P(obj), preexisting)
		}
	}
	if handlers.UpdateFunc != nil {
		actualHandlers.UpdateFunc = func(oldObj, newObj *T) {
			handlers.UpdateFunc(P(oldObj), P(newObj))
		}
	}
	if handlers.DeleteFunc != nil {
		actualHandlers.DeleteFunc = func(obj *T, mayBeStale bool) {
			handlers.DeleteFunc(P(obj), mayBeStale)
		}
	}

	// use a copy of the options for watching vs listing:
	// We want to avoid setting some values for the list requests - specifically, in order to
	// provide synchronization guarantees that the contents of the store are up-to-date strictly
	// *after* the start of an explicit Relist() request, we need to *not* set a resource version in
	// the request to get the most recent data.
	// For more, see: https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions
	watchOpts := listOpts

	// Handling bookmarks means that sometimes the API server will be kind, allowing us to continue
	// the watch instead of resyncing.
	watchOpts.AllowWatchBookmarks = true

	// Perform an initial listing
	config.Metrics.startList()
	initialList, err := client.List(ctx, listOpts)
	config.Metrics.doneList(err)
	if err != nil {
		return nil, fmt.Errorf("Initial list failed: %w", err)
	}

	// set ResourceVersion so that the client.Watch request(s) show only the changes since we made
	// the initial list
	watchOpts.ResourceVersion = initialList.GetListMeta().GetResourceVersion()

	sendStop, stopSignal := util.NewSingleSignalPair[struct{}]()

	store := Store[T]{
		mutex:         sync.Mutex{},
		objects:       make(map[types.UID]*T),
		handlers:      actualHandlers,
		triggerRelist: make(chan struct{}, 1), // ensure sends are non-blocking
		relisted:      make(chan struct{}),
		nextIndexID:   0,
		indexes:       make(map[uint64]Index[T]),
		stopSignal:    sendStop,
		stopped:       atomic.Bool{},
		failing:       atomic.Bool{},

		deepCopy: func(t *T) *T {
			return (*T)(P(t).DeepCopyObject().(P))
		},
	}

	items := accessors.Items(initialList)

	var deferredAdds []T

	if mode == InitModeDefer {
		deferredAdds = items
	} else {
		for i := range items {
			obj := &items[i]
			P(obj).GetObjectKind().SetGroupVersionKind(gvk)
			uid := P(obj).GetObjectMeta().GetUID()
			store.objects[uid] = obj
			store.handlers.AddFunc(obj, true)

			// Check if the context has been cancelled. This can happen in practice if AddFunc may
			// take a long time to complete.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
	}
	items = nil // reset to allow GC

	// Start watching
	config.Metrics.startWatch()
	watcher, err := client.Watch(ctx, watchOpts)
	config.Metrics.doneWatch(err)
	if err != nil {
		return nil, fmt.Errorf("Initial watch failed: %w", err)
	}

	// Lock the store to pass it into the goroutine, so that we don't have to worry about immediate
	// operations on the store racing with any deferred additions.
	store.mutex.Lock()

	// With the successful Watch call underway, we hand off responsibility to a new goroutine.
	go func() {
		holdingInitialLock := true
		defer func() {
			if holdingInitialLock {
				store.mutex.Unlock()
			}
		}()

		// note: instead of deferring watcher.Stop() directly, wrapping it in an outer function
		// means that we'll always Stop the most recent watcher.
		defer func() {
			watcher.Stop()
		}()

		// explicitly stop on exit so that it's possible to know when the store is stopped
		defer store.Stop()

		config.Metrics.alive()
		defer config.Metrics.unalive()

		if len(deferredAdds) != 0 {
			logger.Info("Handling deferred adds")
		}

		// Handle any deferred calls to AddFunc
		// NB: This is only sound because we're still holding store.mutex; otherwise we'd have to
		// deal with possible racy operations (including adding an index).
		for i := range deferredAdds {
			obj := &deferredAdds[i]
			P(obj).GetObjectKind().SetGroupVersionKind(gvk)
			uid := P(obj).GetObjectMeta().GetUID()
			store.objects[uid] = obj
			store.handlers.AddFunc(obj, true)

			if err := ctx.Err(); err != nil {
				logger.Warn("Ending: because Context expired", zap.Error(ctx.Err()))
				return
			}
		}

		holdingInitialLock = false
		store.mutex.Unlock()

		defer config.Metrics.unfailing()

		logger.Info("All setup complete, entering event loop")

		for {
			// this is used exclusively for relisting, but must be defined up here so that our gotos
			// don't jump over variables.
			var signalRelistComplete []chan struct{}
			for {
				select {
				case <-stopSignal.Recv():
					logger.Info("Ending: because we got a stop signal")
					return
				case <-ctx.Done():
					logger.Info("Ending: because Context expired", zap.Error(ctx.Err()))
					return
				case <-store.triggerRelist:
					config.Metrics.relistRequested()
					goto relist
				case event, ok := <-watcher.ResultChan():
					if !ok {
						logger.Info("Watcher ended gracefully, restarting")
						goto newWatcher
					}

					config.Metrics.recordEvent(event.Type)

					if event.Type == watch.Error {
						err := apierrors.FromObject(event.Object)
						// note: we can get 'too old resource version' errors when there's been a
						// lot of resource updates that our ListOptions filtered out.
						if apierrors.IsResourceExpired(err) {
							logger.Warn("Received error event", zap.Error(err))
						} else {
							logger.Error("Received error event", zap.Error(err))
						}
						goto relist
					}

					obj, ok := event.Object.(P)
					if !ok {
						var p P
						logger.Error(
							"Error casting event object to desired type",
							zap.String("eventType", string(event.Type)),
							zap.String("eventObjectType", fmt.Sprintf("%T", event.Object)),
							zap.String("desiredObjectType", fmt.Sprintf("%T", p)),
						)
						continue
					}
					P(obj).GetObjectKind().SetGroupVersionKind(gvk)

					meta := obj.GetObjectMeta()
					// Update ResourceVersion so subsequent calls to client.Watch won't include this
					// event, which we're currently processing.
					watchOpts.ResourceVersion = meta.GetResourceVersion()

					// Wrap the remainder in a function, so we can have deferred unlocks.
					uid := meta.GetUID()
					err := store.handleEvent(event.Type, uid, obj)
					if err != nil {
						name := util.NamespacedName{Namespace: meta.GetNamespace(), Name: meta.GetName()}
						logger.Error(
							"failed to handle event",
							zap.Error(err),
							zap.String("UID", string(uid)),
							zap.Object(config.ObjectNameLogField, name),
						)
						goto relist
					}
				}
			}

		relist:
			// Every time we make a new request, we create a channel for it. That's because we need
			// to make sure that any user's call to WatchStore.Relist() that happens *while* we're
			// actually making the request to K8s won't get overwritten by that request. Basically,
			// we need to make sure that relisting is only marked as complete if there was a request
			// that occurred *after* the call to Relist() returned.
			//
			// There's probably other ways we could do this - it's an area for possible improvement.
			//
			// Note: if we didn't do this at all, the alternative would be to ignore additional
			// relist requests, having them handled naturally as we get around to watching again.
			// This can amplify request failures - particularly if the K8s API server is overloaded.
			signalRelistComplete = make([]chan struct{}, 0, 1)

			// When we get to this point in the control flow, it's not guaranteed that the watcher
			// has stopped.
			//
			// As of 2023-12-05, the implementation of the API's watchers (internally handled by
			// k8s.io/apimachinery@.../pkg/watch/streamwatcher.go) explicitly allows multiple calls
			// to Stop().
			//
			// This all means that it's always safe for us to call Stop() here, and sometimes we
			// MUST call it here (to avoid leaking watchers after relisting), so it's worth just
			// always calling it.
			watcher.Stop()

			logger.Info("Relisting")
			for first := true; ; first = false {
				func() {
					store.mutex.Lock()
					defer store.mutex.Unlock()

					newRelistTriggered := false

					// Consume any additional relist request.
					// All usage of triggerRelist from within (*Store[T]).Relist() is asynchronous,
					// because triggerRelist has capacity=1 and has an item in it iff relisting has
					// been requested, so if Relist() *would* block on sending, the signal has
					// already been given.
					// That's all to say: Receiving only once from triggerRelist is sufficient.
					select {
					case <-store.triggerRelist:
						newRelistTriggered = true
						config.Metrics.relistRequested()
					default:
					}

					if first || newRelistTriggered {
						signalRelistComplete = append(signalRelistComplete, store.relisted)
						store.relisted = make(chan struct{})
					}
				}()

				config.Metrics.startList()
				relistList, err := client.List(ctx, listOpts) // don't include resource version, so it's guaranteed most recent
				config.Metrics.doneList(err)
				if err != nil {
					logger.Error("Relist failed", zap.Error(err))
					if config.RetryRelistAfter == nil {
						logger.Info("Ending: because relist failed and RetryWatchAfter is nil")
						return
					}
					retryAfter := config.RetryRelistAfter.Random()
					logger.Info("Retrying relist after delay", zap.Duration("delay", retryAfter))

					store.failing.Store(true)
					config.Metrics.failing()

					select {
					case <-time.After(retryAfter):
						logger.Info("Relist delay reached, retrying", zap.Duration("delay", retryAfter))
						continue
					case <-ctx.Done():
						logger.Info("Ending: because Context expired", zap.Error(ctx.Err()))
						return
					case <-stopSignal.Recv():
						logger.Info("Ending: because we got a stop signal")
						return
					}
				}

				store.failing.Store(false)
				config.Metrics.unfailing()

				// err == nil, process relistList
				relistItems := accessors.Items(relistList)

				func() {
					store.mutex.Lock()
					defer store.mutex.Unlock()

					// Copy the current contents of objects, and start tracking which ones have
					// since been deleted.
					oldObjects := make(map[types.UID]*T)
					deleted := make(map[types.UID]struct{}) // set of UIDs that have been deleted
					for uid, obj := range store.objects {
						oldObjects[uid] = obj
						deleted[uid] = struct{}{} // initially mark everything as deleted, until we find it isn't
					}

					// Mark all items we still have as not deleted
					for i := range relistItems {
						uid := P(&relistItems[i]).GetObjectMeta().GetUID()
						delete(deleted, uid)
					}

					// Generate deletion events for all objects that are no longer present. We do
					// this first so that when there's externally-enforced uniqueness that isn't
					// unique *across time* (e.g. object names), users can still rely on uniqueness
					// at any time that handlers are called.
					for uid := range deleted {
						obj := store.objects[uid]
						delete(store.objects, uid)
						for _, index := range store.indexes {
							index.Delete(obj)
						}
						store.handlers.DeleteFunc(obj, true)
					}

					for i := range relistItems {
						obj := &relistItems[i]
						uid := P(obj).GetObjectMeta().GetUID()
						P(obj).GetObjectKind().SetGroupVersionKind(gvk)

						store.objects[uid] = obj
						oldObj, hasObj := oldObjects[uid]

						if hasObj {
							for _, index := range store.indexes {
								index.Update(oldObj, obj)
							}
							store.handlers.UpdateFunc(oldObj, obj)
						} else {
							for _, index := range store.indexes {
								index.Add(obj)
							}
							store.handlers.AddFunc(obj, false)
						}
					}
				}()

				// Update ResourceVersion, recreate watcher.
				watchOpts.ResourceVersion = relistList.GetListMeta().GetResourceVersion()
				logger.Info("Relist complete, restarting watcher")
				for _, ch := range signalRelistComplete {
					close(ch)
				}
				goto newWatcher
			}

		newWatcher:
			// In the loop, retry the API call to watch.
			//
			// It's possible that we attempt to watch with a resource version that's too old, in
			// which case the API call *does* succeed, but the first event is an error (which we use
			// to trigger relisting).
			for {
				config.Metrics.startWatch()
				watcher, err = client.Watch(ctx, watchOpts)
				config.Metrics.doneWatch(err)
				if err != nil {
					logger.Error("Re-watch failed", zap.Error(err))
					if config.RetryWatchAfter == nil {
						logger.Info("Ending: because re-watch failed and RetryWatchAfter is nil")
						return
					}
					retryAfter := config.RetryWatchAfter.Random()
					logger.Info("Retrying re-watch after delay", zap.Duration("delay", retryAfter))

					store.failing.Store(true)
					config.Metrics.failing()

					select {
					case <-time.After(retryAfter):
						logger.Info("Re-watch delay reached, retrying", zap.Duration("delay", retryAfter))
						continue
					case <-ctx.Done():
						logger.Info("Ending: because Context expired", zap.Error(ctx.Err()))
						return
					case <-stopSignal.Recv():
						logger.Info("Ending: because we got a stop signal")
						return
					}
				}

				// err == nil
				store.failing.Store(false)
				config.Metrics.unfailing()
				break
			}
		}
	}()

	return &store, nil
}

// helper for Watch. Error events are expected to already have been handled by the caller.
func (store *Store[T]) handleEvent(
	eventType watch.EventType,
	uid types.UID,
	obj *T,
) error {
	// Some of the cases below don't actually require locking the store. Most of the events that we
	// receive *do* though, so we're better off doing it here for simplicity.
	store.mutex.Lock()
	defer store.mutex.Unlock()

	switch eventType {
	case watch.Added:
		if _, ok := store.objects[uid]; ok {
			return fmt.Errorf("received add event for object we already have")
		}
		store.objects[uid] = obj
		for _, index := range store.indexes {
			index.Add(obj)
		}
		store.handlers.AddFunc(obj, false)
	case watch.Deleted:
		// We're given the state of the object immediately before deletion, which
		// *may* be different to what we currently have stored.
		old, ok := store.objects[uid]
		if !ok {
			return errors.New("received delete event for object that's not present")
		}
		// Update:
		for _, index := range store.indexes {
			index.Update(old, obj)
		}
		store.handlers.UpdateFunc(old, obj)
		// Delete:
		delete(store.objects, uid)
		for _, index := range store.indexes {
			index.Delete(obj)
		}
		store.handlers.DeleteFunc(obj, false)
	case watch.Modified:
		old, ok := store.objects[uid]
		if !ok {
			return errors.New("received update event for object that's not present")
		}
		store.objects[uid] = obj
		for _, index := range store.indexes {
			index.Update(old, obj)
		}
		store.handlers.UpdateFunc(old, obj)
	case watch.Bookmark:
		// Nothing to do, just serves to give us a new ResourceVersion, which should be handled by
		// the caller.
	case watch.Error:
		panic(errors.New("handleEvent unexpectedly called with eventType Error"))
	default:
		panic(errors.New("unknown watch event"))
	}
	return nil
}

// Store provides an interface for getting information about a list of Ts using the event
// listener from a previous call to Watch
type Store[T any] struct {
	objects map[types.UID]*T
	mutex   sync.Mutex

	handlers HandlerFuncs[*T]

	// helper function, created in Watch() using knowledge that *T (or, something based on it) is a
	// runtime.Object.
	// This is required for the implementation of (*Store[T]).NopUpdate() in order to produce a
	// second object without having any guarantees about T.
	deepCopy func(*T) *T

	// triggerRelist has capacity=1 and *if* the channel contains an item, then relisting has been
	// requested by some call to (*Store[T]).Relist().
	triggerRelist chan struct{}
	// relisted is replaced and closed whenever relisting happens. Refer to its usage in Watch or
	// (*Store[T]).Relist() for more detail.
	relisted chan struct{}

	nextIndexID uint64
	indexes     map[uint64]Index[T]

	stopSignal util.SignalSender[struct{}]
	stopped    atomic.Bool
	failing    atomic.Bool
}

// Relist triggers re-listing the WatchStore, returning a channel that will be closed once the
// re-list is complete
func (w *Store[T]) Relist() <-chan struct{} {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	// Because triggerRelist has capacity=1, failing to immediately send to the channel means that
	// there's already a signal to request relisting that has not yet been processed.
	select {
	case w.triggerRelist <- struct{}{}:
	default:
	}

	// note: w.relisted is replaced immediately before every attempt at the API call for relisting,
	// so that there's a strict happens-before relation that guarantees that *when* w.relisted is
	// closed, the relevant List call *must* have happened after any attempted send on
	// w.triggerRelist.
	return w.relisted
}

// NopUpdate runs the update handler for the object with the given UID, blocking until completion.
//
// This method returns false if there is no object with the given UID.
//
// Why does this exist? Well, watch events are often going to be handled by adding the object to a
// queue. And sometimes you want to re-inject something into the queue. But it's tricky for that to
// be synchronized unless it's guaranteed to agree with the ongoing watch -- so this method allows
// one to re-inject something into the queue if and only if the watch still belives it exists in
// kubernetes.
func (w *Store[T]) NopUpdate(uid types.UID) (ok bool) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	obj, ok := w.objects[uid]
	if !ok {
		return false
	}

	copied := w.deepCopy(obj)
	w.handlers.UpdateFunc(copied, obj)
	return true
}

func (w *Store[T]) Stop() {
	w.stopSignal.Send(struct{}{})
	w.stopped.Store(true)
}

func (w *Store[T]) Failing() bool {
	return w.failing.Load()
}

func (w *Store[T]) Stopped() bool {
	return w.stopped.Load()
}

func (w *Store[T]) Items() []*T {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	items := make([]*T, len(w.objects))
	i := 0
	for _, val := range w.objects {
		items[i] = val
		i += 1
	}

	return items
}

// NewIndexedStore creates a new IndexedWatchStore from the WatchStore and the index to use.
//
// Note: the index type is assumed to have reference semantics; i.e. any shallow copy of the value
// will affect any other shallow copy.
//
// For more information, refer to IndexedWatchStore.
func NewIndexedStore[T any, I Index[T]](store *Store[T], index I) IndexedStore[T, I] {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	for _, obj := range store.objects {
		index.Add(obj)
	}

	id := store.nextIndexID
	store.nextIndexID += 1
	store.indexes[id] = index

	collector := &struct{}{}
	// when this IndexedWatchStore is GC'd, remove its index from the WatchStore. This should
	// provide a reliable way of making sure that indexes always get cleaned up.
	stdruntime.SetFinalizer(collector, func(_ any) {
		// note: finalizers always run in a separate goroutine, so it's ok to lock here.
		store.mutex.Lock()
		defer store.mutex.Unlock()
		delete(store.indexes, id)
	})

	return IndexedStore[T, I]{store, index, id, collector}
}

// IndexedStore represents a WatchStore, wrapped with a privileged WatchIndex that can be used
// to efficiently answer queries.
type IndexedStore[T any, I Index[T]] struct {
	*Store[T]

	index I

	// id stores the id of this index in the WatchStore
	id uint64
	// collector has a destructor attached to it so that the index can be automatically removed from
	// the WatchStore when it's no longer in use, without requiring users to manually get rid of it.
	collector *struct{}
}

// WithIndex calls a function with the current state of the index, locking the WatchStore around it.
//
// It is almost guaranteed to be an error to indirectly return the index with this function.
func (w IndexedStore[T, I]) WithIndex(f func(I)) {
	w.Store.mutex.Lock()
	defer w.Store.mutex.Unlock()

	f(w.index)
}

func (w IndexedStore[T, I]) GetIndexed(f func(I) (*T, bool)) (obj *T, ok bool) {
	w.WithIndex(func(i I) {
		obj, ok = f(i)
	})
	return
}

func (w IndexedStore[T, I]) ListIndexed(f func(I) []*T) (list []*T) {
	w.WithIndex(func(i I) {
		list = f(i)
	})
	return
}

func NewNameIndex[T any]() *NameIndex[T] {
	// check that *T implements metav1.ObjectMetaAccessor
	var zero T
	ptrToZero := any(&zero)
	if _, ok := ptrToZero.(metav1.ObjectMetaAccessor); !ok {
		panic("type *T must implement metav1.ObjectMetaAccessor")
	}

	// This doesn't *need* to be a pointer, but the intent is a little more clear this way.
	return &NameIndex[T]{
		namespacedNames: make(map[util.NamespacedName]*T),
	}
}

// NameIndex is a WatchIndex that provides efficient lookup for a value with a particular name
type NameIndex[T any] struct {
	namespacedNames map[util.NamespacedName]*T
}

// note: requires that *T implements metav1.ObjectMetaAccessor
func keyForObj[T any](obj *T) util.NamespacedName {
	meta := any(obj).(metav1.ObjectMetaAccessor).GetObjectMeta()

	return util.NamespacedName{Namespace: meta.GetNamespace(), Name: meta.GetName()}
}

func (i *NameIndex[T]) Add(obj *T) {
	i.namespacedNames[keyForObj(obj)] = obj
}

func (i *NameIndex[T]) Update(oldObj, newObj *T) {
	i.Delete(oldObj)
	i.Add(newObj)
}

func (i *NameIndex[T]) Delete(obj *T) {
	delete(i.namespacedNames, keyForObj(obj))
}

func (i *NameIndex[T]) Get(namespace string, name string) (obj *T, ok bool) {
	obj, ok = i.namespacedNames[util.NamespacedName{Namespace: namespace, Name: name}]
	return
}

func NewFlatNameIndex[T any]() *FlatNameIndex[T] {
	// check that *T implements metav1.ObjectMetaAccessor
	var zero T
	ptrToZero := any(&zero)
	if _, ok := ptrToZero.(metav1.ObjectMetaAccessor); !ok {
		panic("type *T must implement metav1.ObjectMetaAccessor")
	}

	return &FlatNameIndex[T]{
		names: make(map[string]*T),
	}
}

type FlatNameIndex[T any] struct {
	names map[string]*T
}

// note: requires that *T implements metav1.ObjectMetaAccessor
func getName[T any](obj *T) string {
	meta := any(obj).(metav1.ObjectMetaAccessor).GetObjectMeta()
	return meta.GetName()
}

func (i *FlatNameIndex[T]) Add(obj *T) {
	i.names[getName(obj)] = obj
}

func (i *FlatNameIndex[T]) Update(oldObj, newObj *T) {
	i.Delete(oldObj)
	i.Add(newObj)
}

func (i *FlatNameIndex[T]) Delete(obj *T) {
	delete(i.names, getName(obj))
}

func (i *FlatNameIndex[T]) Get(name string) (obj *T, ok bool) {
	obj, ok = i.names[name]
	return
}
