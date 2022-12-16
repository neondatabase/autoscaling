package util

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

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
	DeleteFunc func(obj P)
}

// Watch starts a goroutine for watching events, using the provided WatchHandlerFuncs as the
// callbacks for each type of event.
//
// The type C is the kubernetes client we use to get the objects, L representing a list of these,
// T representing the object type, and P as a pointer to T.
func Watch[C WatchClient[L], L metav1.ListMetaAccessor, T any, P WatchObject[T]](
	ctx context.Context,
	client C,
	accessors WatchAccessors[L, T],
	opts metav1.ListOptions,
	handlers WatchHandlerFuncs[P],
) (*WatchStore[T], error) {
	if accessors.Items == nil {
		panic("accessors.Items == nil")
	}

	if handlers.AddFunc == nil {
		handlers.AddFunc = func(obj P, preexisting bool) {}
	}
	if handlers.UpdateFunc == nil {
		handlers.UpdateFunc = func(oldObj, newObj P) {}
	}
	if handlers.DeleteFunc == nil {
		handlers.DeleteFunc = func(obj P) {}
	}

	// Perform an initial listing
	initialList, err := client.List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("Initial list failed: %w", err)
	}

	// set ResourceVersion so that the client.Watch request(s) show only the changes since we made
	// the initial list
	opts.ResourceVersion = initialList.GetListMeta().GetResourceVersion()

	store := WatchStore[T]{
		objects: make(map[types.UID]*T),
		stopCh:  make(chan struct{}),
	}

	// FIXME: Always calling AddFunc makes it easy to deadlock. Instead, we should provide a method
	// on WatchStore that allows the caller to wait until the events from the initial listing have
	// been processed. We can still create the watcher before processing them, by starting it at the
	// already-updated ResourceVersion
	items := accessors.Items(initialList)
	for i := range items {
		obj := &items[i]
		uid := P(obj).GetObjectMeta().GetUID()
		store.objects[uid] = obj
		handlers.AddFunc(obj, true)
	}

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

		defer func() {
			if !store.closed.Swap(true) {
				close(store.stopCh)
			} else {
				// Make sure we consume any "close" messages that might not have already been
				// handled.
				_, _ = <-store.stopCh
			}
		}()

		for {
			for {
				select {
				case <-store.stopCh:
					return
				case <-ctx.Done():
					return
				case event, ok := <-watcher.ResultChan():
					if !ok {
						goto newWatcher
					} else if event.Type == watch.Error {
						err := apierrors.FromObject(event.Object)
						klog.Errorf("received watch error: %s", err)
						return
					}

					obj, ok := event.Object.(P)
					if !ok {
						var p P
						klog.Errorf(
							"watch: error casting event type %s object as type %T, got type %T",
							event.Type, p, event.Object,
						)
						continue
					}

					meta := obj.GetObjectMeta()
					uid := meta.GetUID()
					// Update ResourceVersion so subsequent calls to client.Watch won't include this
					// event, which we're currently processing.
					opts.ResourceVersion = meta.GetResourceVersion()

					name := func(m metav1.Object) string {
						return fmt.Sprintf("%s:%s", m.GetNamespace(), m.GetName())
					}

					// Wrap the remainder in a function, so we can have deferred unlocks.
					err := func() error {
						switch event.Type {
						case watch.Added:
							store.mutex.Lock()
							defer store.mutex.Unlock()

							if _, ok := store.objects[uid]; ok {
								return fmt.Errorf(
									"watch: received add event for object %s that we already have",
									name(meta),
								)
							}
							store.objects[uid] = (*T)(obj)
							handlers.AddFunc((*T)(obj), false)
						case watch.Bookmark:
							// Nothing to do, just serves to give us a new ResourceVersion.
						case watch.Deleted:
							// We're given the state of the object immediately before deletion, which
							// *may* be different to what we currently have stored.
							store.mutex.Lock()
							defer store.mutex.Unlock()

							old, ok := store.objects[uid]
							if !ok {
								return fmt.Errorf(
									"watch: received delete event for object %s that's not present",
									name(meta),
								)
							}
							handlers.UpdateFunc(old, (*T)(obj))
							delete(store.objects, uid)
							handlers.DeleteFunc((*T)(obj))
						case watch.Modified:
							old, ok := store.objects[uid]
							if !ok {
								return fmt.Errorf(
									"watch: received update event for object %s that's not present",
									name(meta),
								)
							}
							store.objects[uid] = (*T)(obj)
							handlers.UpdateFunc(old, (*T)(obj))
						case watch.Error:
							panic("unreachable code reached") // handled above
						default:
							panic("unknown watch event")
						}
						return nil
					}()

					if err != nil {
						klog.Error(err.Error())
						return
					}
				}
			}
		newWatcher:
			klog.Info("watcher ended gracefully, restarting")

			// FIXME: we need some kind of retry here, *and* a timeout accompanying it. We probably
			// don't need a timeout if the watcher was long-running (i.e. > 1s) and exited without
			// error.

			watcher, err = client.Watch(ctx, opts)
			if err != nil {
				klog.Errorf("non-initial watch failed: %s", err)
				return
			}
		}
	}()

	return &store, nil
}

// WatchStore provides an interface for getting information about a list of Ts using the event
// listener from a previous call to Watch
type WatchStore[T any] struct {
	objects map[types.UID]*T
	mutex   sync.Mutex
	stopCh  chan struct{}
	closed  atomic.Bool
}

func (w *WatchStore[T]) Stop() {
	if !w.closed.Swap(true) {
		w.stopCh <- struct{}{}
		close(w.stopCh)
	}
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
