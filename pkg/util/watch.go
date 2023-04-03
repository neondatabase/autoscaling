package util

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
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
		objects:    make(map[types.UID]*T),
		stopSignal: sendStop,
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
			for {
				select {
				case <-stopSignal.Recv():
					return
				case <-ctx.Done():
					return
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
									"watch %s: received add event for object %s that we already have",
									config.LogName, name(meta),
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
									"watch %s: received delete event for object %s that's not present",
									config.LogName, name(meta),
								)
							}
							handlers.UpdateFunc(old, (*T)(obj))
							delete(store.objects, uid)
							handlers.DeleteFunc((*T)(obj), false)
						case watch.Modified:
							old, ok := store.objects[uid]
							if !ok {
								return fmt.Errorf(
									"watch %s: received update event for object %s that's not present",
									config.LogName, name(meta),
								)
							}
							store.objects[uid] = (*T)(obj)
							handlers.UpdateFunc(old, (*T)(obj))
						case watch.Error:
							panic(errors.New("unreachable code reached")) // handled above
						default:
							panic(errors.New("unknown watch event"))
						}
						return nil
					}()

					if err != nil {
						klog.Error(err.Error())
						goto relist
					}
				}
			}

		relist:
			klog.Infof("watch %s: re-listing", config.LogName)
			for {
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
					// First, copy the contents of objects into oldObjects. We do this so that we can
					// uphold some guarantees about the contents of the store.
					oldObjects := make(map[types.UID]*T)
					for uid, obj := range store.objects {
						oldObjects[uid] = obj
					}

					store.mutex.Lock()
					defer store.mutex.Unlock()

					for i := range relistItems {
						obj := &relistItems[i]
						uid := P(obj).GetObjectMeta().GetUID()

						store.objects[uid] = obj
						oldObj, hasObj := oldObjects[uid]

						if hasObj {
							handlers.UpdateFunc(oldObj, obj)
							delete(oldObjects, uid)
						} else {
							handlers.AddFunc(obj, false)
						}
					}

					// For everything that's still in oldObjects (i.e. wasn't covered by relistItems),
					// generate deletion events:
					for uid, obj := range oldObjects {
						delete(store.objects, uid)
						handlers.DeleteFunc(obj, true)
					}
				}()

				// Update ResourceVersion, recreate watcher.
				opts.ResourceVersion = relistList.GetListMeta().GetResourceVersion()
				klog.Infof("watch %s: re-list complete, restarting watcher", config.LogName)
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

// WatchStore provides an interface for getting information about a list of Ts using the event
// listener from a previous call to Watch
type WatchStore[T any] struct {
	objects map[types.UID]*T
	mutex   sync.Mutex

	stopSignal SignalSender
}

func (w *WatchStore[T]) Stop() {
	w.stopSignal.Send()
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
