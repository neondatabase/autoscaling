package util

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	stdruntime "runtime"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"
)

// WatchClient is implemented by the specific interfaces of kubernetes clients, like
// `Clientset.CoreV1().Pods(namespace)` or `..Nodes()`
//
// This interface should be *already implemented* by whatever the correct client is.
type WatchClient[L any] interface {
	List(context.Context, metav1.ListOptions) (L, error)
	Watch(context.Context, metav1.ListOptions) (watch.Interface, error)
}

// WatchConfig is the miscellaneous configuration used by Watch
type WatchConfig struct {
	// LogName is the name of the watcher for use in logs
	LogName string

	// RetryRelistAfter gives a retry interval when a re-list fails. If left nil, then Watch will
	// not retry.
	RetryRelistAfter *TimeRange
	// RetryWatchAfter gives a retry interval when a non-initial watch fails. If left nil, then
	// Watch will not retry.
	RetryWatchAfter *TimeRange
}

type TimeRange struct {
	min   int
	max   int
	units time.Duration
}

func NewTimeRange(units time.Duration, min, max int) *TimeRange {
	if min < 0 {
		panic(errors.New("bad time range: min < 0"))
	} else if min == 0 && max == 0 {
		panic(errors.New("bad time range: min and max = 0"))
	} else if max < min {
		panic(errors.New("bad time range: max < min"))
	}

	return &TimeRange{min: min, max: max, units: units}
}

// Random returns a random time.Duration within the range
func (r TimeRange) Random() time.Duration {
	if r.max == r.min {
		return time.Duration(r.min) * r.units
	}

	count := rand.Intn(r.max-r.min) + r.min
	return time.Duration(count) * r.units
}

// WatchAccessors provides the "glue" functions for Watch to go from a list L (returned by the
// client's List) to the underlying slice of items []T
type WatchAccessors[L any, T any] struct {
	Items func(L) []T
}

// WatchObject is implemented by pointers to T, where T is typically the resource that we're
// actually watching.
//
// Example implementors: *corev1.Pod, *corev1.Node
type WatchObject[T any] interface {
	~*T
	runtime.Object
	metav1.ObjectMetaAccessor
}

// WatchHandlerFuncs provides the set of callbacks to use for events from Watch
type WatchHandlerFuncs[P any] struct {
	AddFunc    func(obj P, preexisting bool)
	UpdateFunc func(oldObj P, newObj P)
	DeleteFunc func(obj P, mayBeStale bool)
}

// WatchIndex represents types that provide some kind of additional index on top of the base listing
//
// Indexing is functionally implemented in the same way that WatchHandlerFuncs is, with the main
// difference being that more things are done for you with WatchIndexes. In particular, indexes can
// be added and removed after the Watch has already started, and the locking behavior is explicit.
type WatchIndex[T any] interface {
	Add(obj *T)
	Update(oldObj, newObj *T)
	Delete(obj *T)
}

// InitWatchMode dictates the behavior of Watch with respect to any initial calls to
// handlers.AddFunc before returning
//
// If set to InitWatchModeSync, then AddFunc will be called while processing the initial listing,
// meaning that the returned WatchStore is guaranteed contain the state of the cluster (although it
// may update before any access).
//
// Otherwise, if set to InitWatchModeDefer, then AddFunc will not be called until after Watch
// returns. Correspondingly, the WatchStore will not update until then either.
type InitWatchMode string

const (
	InitWatchModeSync  InitWatchMode = "sync"
	InitWatchModeDefer InitWatchMode = "defer"
)

// Watch starts a goroutine for watching events, using the provided WatchHandlerFuncs as the
// callbacks for each type of event.
//
// The type C is the kubernetes client we use to get the objects, L representing a list of these,
// T representing the object type, and P as a pointer to T.
func Watch[C WatchClient[L], L metav1.ListMetaAccessor, T any, P WatchObject[T]](
	ctx context.Context,
	client C,
	config WatchConfig,
	accessors WatchAccessors[L, T],
	mode InitWatchMode,
	opts metav1.ListOptions,
	handlers WatchHandlerFuncs[P],
) (*WatchStore[T], error) {
	if accessors.Items == nil {
		panic(errors.New("accessors.Items == nil"))
	}

	if handlers.AddFunc == nil {
		handlers.AddFunc = func(obj P, preexisting bool) {}
	}
	if handlers.UpdateFunc == nil {
		handlers.UpdateFunc = func(oldObj, newObj P) {}
	}
	if handlers.DeleteFunc == nil {
		handlers.DeleteFunc = func(obj P, mayBeStale bool) {}
	}

	// Handling bookmarks means that sometimes the API server will be kind, allowing us to continue
	// the watch instead of resyncing.
	opts.AllowWatchBookmarks = true

	// Perform an initial listing
	initialList, err := client.List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("Initial list failed: %w", err)
	}

	// set ResourceVersion so that the client.Watch request(s) show only the changes since we made
	// the initial list
	opts.ResourceVersion = initialList.GetListMeta().GetResourceVersion()

	sendStop, stopSignal := NewSingleSignalPair()

	store := WatchStore[T]{
		objects:       make(map[types.UID]*T),
		triggerRelist: make(chan struct{}, 1), // ensure sends are non-blocking
		relisted:      make(chan struct{}),
		nextIndexID:   0,
		indexes:       make(map[uint64]WatchIndex[T]),
		stopSignal:    sendStop,
	}

	items := accessors.Items(initialList)

	var deferredAdds []T

	if mode == InitWatchModeDefer {
		deferredAdds = items
	} else {
		for i := range items {
			obj := &items[i]
			uid := P(obj).GetObjectMeta().GetUID()
			store.objects[uid] = obj
			handlers.AddFunc(obj, true)

			// Check if the context has been cancelled. This can happen in practice if AddFunc may
			// take a long time to complete.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
	}
	items = nil // reset to allow GC

	// Start watching
	watcher, err := client.Watch(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("Initial watch failed: %w", err)
	}

	// With the successful Watch call underway, we hand off responsibility to a new goroutine.
	go func() {
		// note: instead of deferring watcher.Stop() directly, wrapping it in an outer function
		// means that we'll always Stop the most recent watcher.
		defer func() {
			watcher.Stop()
		}()

		// explicitly stop on exit so that it's possible to know when the store is stopped
		defer store.Stop()

		// Handle any deferred calls to AddFunc
		for i := range deferredAdds {
			obj := &deferredAdds[i]
			uid := P(obj).GetObjectMeta().GetUID()
			store.objects[uid] = obj
			handlers.AddFunc(obj, true)

			if err := ctx.Err(); err != nil {
				return
			}
		}

		for {
			// this is used exclusively for relisting, but must be defined up here so that our gotos
			// don't jump over variables.
			var signalRelistComplete []chan struct{}
			for {
				select {
				case <-stopSignal.Recv():
					return
				case <-ctx.Done():
					return
				case <-store.triggerRelist:
					continue
				case event, ok := <-watcher.ResultChan():
					if !ok {
						klog.Infof("watch %s: watcher ended gracefully, restarting", config.LogName)
						goto newWatcher
					} else if event.Type == watch.Error {
						err := apierrors.FromObject(event.Object)
						// note: we can get 'too old resource version' errors when there's been a
						// lot of resource updates that our ListOptions filtered out.
						if apierrors.IsResourceExpired(err) {
							klog.Warningf("watch %s: received error: %s", config.LogName, err)
						} else {
							klog.Errorf("watch %s: received error: %s", config.LogName, err)
						}
						goto relist
					}

					obj, ok := event.Object.(P)
					if !ok {
						var p P
						klog.Errorf(
							"watch %s: error casting event type %s object as type %T, got type %T",
							config.LogName, event.Type, p, event.Object,
						)
						continue
					}

					meta := obj.GetObjectMeta()
					// Update ResourceVersion so subsequent calls to client.Watch won't include this
					// event, which we're currently processing.
					opts.ResourceVersion = meta.GetResourceVersion()

					// Wrap the remainder in a function, so we can have deferred unlocks.
					err := handleEvent(&store, config, handlers, event.Type, meta, obj)

					if err != nil {
						klog.Error(err.Error())
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

			klog.Infof("watch %s: re-listing", config.LogName)
			for first := true; ; first = false {
				func() {
					store.mutex.Lock()
					defer store.mutex.Unlock()

					newRelistTriggered := false

					// consume any additional relist request
					select {
					case <-store.triggerRelist:
						newRelistTriggered = true
					default:
					}

					if first || newRelistTriggered {
						signalRelistComplete = append(signalRelistComplete, store.relisted)
						store.relisted = make(chan struct{})
					}
				}()

				relistList, err := client.List(ctx, opts)
				if err != nil {
					klog.Errorf("watch %s: re-list failed: %s", config.LogName, err)
					if config.RetryRelistAfter == nil {
						klog.Infof("watch %s: ending, re-list failed and RetryWatchAfter is nil", config.LogName)
						return
					}
					retryAfter := config.RetryRelistAfter.Random()
					klog.Infof("watch %s: retrying re-list after %s", config.LogName, retryAfter)

					select {
					case <-time.After(retryAfter):
						klog.Infof("watch %s: retrying re-list", config.LogName)
						continue
					case <-ctx.Done():
						return
					case <-stopSignal.Recv():
						return
					}
				}

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
						handlers.DeleteFunc(obj, true)
					}

					for i := range relistItems {
						obj := &relistItems[i]
						uid := P(obj).GetObjectMeta().GetUID()

						store.objects[uid] = obj
						oldObj, hasObj := oldObjects[uid]

						if hasObj {
							for _, index := range store.indexes {
								index.Update(oldObj, obj)
							}
							handlers.UpdateFunc(oldObj, obj)
						} else {
							for _, index := range store.indexes {
								index.Add(obj)
							}
							handlers.AddFunc(obj, false)
						}
					}
				}()

				// Update ResourceVersion, recreate watcher.
				opts.ResourceVersion = relistList.GetListMeta().GetResourceVersion()
				klog.Infof("watch %s: re-list complete, restarting watcher", config.LogName)
				for _, ch := range signalRelistComplete {
					close(ch)
				}
				goto newWatcher
			}

		newWatcher:
			for {
				watcher, err = client.Watch(ctx, opts)
				if err != nil {
					klog.Errorf("watch %s: re-watch failed: %s", config.LogName, err)
					if config.RetryWatchAfter == nil {
						klog.Infof("watch %s: ending, re-watch failed and RetryWatchAfter is nil", config.LogName)
						return
					}
					retryAfter := config.RetryWatchAfter.Random()
					klog.Infof("watch %s: retrying re-watch after %s", config.LogName, retryAfter)

					select {
					case <-time.After(retryAfter):
						klog.Infof("watch %s: retrying re-watch after %s", config.LogName, retryAfter)
						continue
					case <-ctx.Done():
						return
					case <-stopSignal.Recv():
						return
					}
				}

				// err == nil
				break newWatcher
			}
		}
	}()

	return &store, nil
}

// helper for Watch. Error events are expected to already have been handled by the caller.
func handleEvent[T any, P ~*T](
	store *WatchStore[T],
	config WatchConfig,
	handlers WatchHandlerFuncs[P],
	eventType watch.EventType,
	meta metav1.Object,
	ptr P,
) error {
	uid := meta.GetUID()
	name := NamespacedName{Namespace: meta.GetNamespace(), Name: meta.GetName()}
	obj := (*T)(ptr)

	// Some of the cases below don't actually require locking the store. Most of the events that we
	// recieve *do* though, so we're better off doing it here for simplicity.
	store.mutex.Lock()
	defer store.mutex.Unlock()

	switch eventType {
	case watch.Added:
		if _, ok := store.objects[uid]; ok {
			return fmt.Errorf(
				"watch %s: received add event for object %v that we already have",
				config.LogName, name,
			)
		}
		store.objects[uid] = obj
		for _, index := range store.indexes {
			index.Add(obj)
		}
		handlers.AddFunc(obj, false)
	case watch.Deleted:
		// We're given the state of the object immediately before deletion, which
		// *may* be different to what we currently have stored.
		old, ok := store.objects[uid]
		if !ok {
			return fmt.Errorf(
				"watch %s: received delete event for object %v that's not present",
				config.LogName, name,
			)
		}
		// Update:
		for _, index := range store.indexes {
			index.Update(old, obj)
		}
		handlers.UpdateFunc(old, obj)
		// Delete:
		delete(store.objects, uid)
		for _, index := range store.indexes {
			index.Delete(obj)
		}
		handlers.DeleteFunc(obj, false)
	case watch.Modified:
		old, ok := store.objects[uid]
		if !ok {
			return fmt.Errorf(
				"watch %s: received update event for object %v that's not present",
				config.LogName, name,
			)
		}
		store.objects[uid] = obj
		for _, index := range store.indexes {
			index.Update(old, obj)
		}
		handlers.UpdateFunc(old, obj)
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

// WatchStore provides an interface for getting information about a list of Ts using the event
// listener from a previous call to Watch
type WatchStore[T any] struct {
	objects map[types.UID]*T
	mutex   sync.Mutex

	triggerRelist chan struct{}
	relisted      chan struct{}

	nextIndexID uint64
	indexes     map[uint64]WatchIndex[T]

	stopSignal SignalSender
	stopped    atomic.Bool
}

// Relist triggers re-listing the WatchStore, returning a channel that will be closed once the
// re-list is complete
func (w *WatchStore[T]) Relist() <-chan struct{} {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	select {
	case w.triggerRelist <- struct{}{}:
	default:
	}

	return w.relisted
}

func (w *WatchStore[T]) Stop() {
	w.stopSignal.Send()
	w.stopped.Store(true)
}

func (w *WatchStore[T]) Stopped() bool {
	return w.stopped.Load()
}

func (w *WatchStore[T]) Items() []*T {
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

// NewIndexedWatchStore creates a new IndexedWatchStore from the WatchStore and the index to use.
//
// Note: the index type is assumed to have reference semantics; i.e. any shallow copy of the value
// will affect any other shallow copy.
//
// For more information, refer to IndexedWatchStore.
func NewIndexedWatchStore[T any, I WatchIndex[T]](store *WatchStore[T], index I) IndexedWatchStore[T, I] {
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

	return IndexedWatchStore[T, I]{store, index, id, collector}
}

// IndexedWatchStore represents a WatchStore, wrapped with a privileged WatchIndex that can be used
// to efficiently answer queries.
type IndexedWatchStore[T any, I WatchIndex[T]] struct {
	*WatchStore[T]

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
func (w IndexedWatchStore[T, I]) WithIndex(f func(I)) {
	w.WatchStore.mutex.Lock()
	defer w.WatchStore.mutex.Unlock()

	f(w.index)
}

func (w IndexedWatchStore[T, I]) GetIndexed(f func(I) (*T, bool)) (obj *T, ok bool) {
	w.WithIndex(func(i I) {
		obj, ok = f(i)
	})
	return
}

func (w IndexedWatchStore[T, I]) ListIndexed(f func(I) []*T) (list []*T) {
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
		namespacedNames: make(map[NamespacedName]*T),
	}
}

// NameIndex is a WatchIndex that provides efficient lookup for a value with a particular name
type NameIndex[T any] struct {
	namespacedNames map[NamespacedName]*T
}

// note: requires that *T implements metav1.ObjectMetaAccessor
func keyForObj[T any](obj *T) NamespacedName {
	meta := any(obj).(metav1.ObjectMetaAccessor).GetObjectMeta()

	return NamespacedName{Namespace: meta.GetNamespace(), Name: meta.GetName()}
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
	obj, ok = i.namespacedNames[NamespacedName{namespace, name}]
	return
}
