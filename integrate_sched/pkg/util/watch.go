package util

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
)

// WatchHandlerFuncs provides the set of callbacks to use for events from Watch
type WatchHandlerFuncs[T any] struct {
	AddFunc    func(obj T)
	UpdateFunc func(oldObj, newObj T)
	// DeleteFunc is called whenever an object is deleted. It's possible for the deletion event
	// itself to be missed, but still discovered on a subsequent re-list. If this occurs,
	// finalStateMayBeStale will be true, because we don't know what the final state of the object
	// was, only that it was deleted.
	DeleteFunc func(obj T, finalStateMayBeStale bool)
}

// Watch will start a goroutine watching for events, using the provided WatchHandlerFuncs as the
// callbacks for each type of event.
//
// The type T is typically a pointer, like *corev1.Pod (because it must implement runtime.Object).
func Watch[T runtime.Object](
	restClient cache.Getter,
	stop <-chan struct{},
	resource corev1.ResourceName,
	namespace string,
	handlers WatchHandlerFuncs[T],
	optsModifier func(*metav1.ListOptions),
) WatchStore[T] {
	watchlist := cache.NewFilteredListWatchFromClient(restClient, string(resource), namespace, optsModifier)

	var eventHandler cache.ResourceEventHandlerFuncs

	// Preserve nil-ness of the functions, so that we don't end up making lots of calls to functions
	// that do nothing. (or worse: calling nil functions).
	if handlers.AddFunc != nil {
		eventHandler.AddFunc = func(obj any) { handlers.AddFunc(obj.(T)) }
	}
	if handlers.UpdateFunc != nil {
		eventHandler.UpdateFunc = func(oldObj, newObj any) { handlers.UpdateFunc(oldObj.(T), newObj.(T)) }
	}
	if handlers.DeleteFunc != nil {
		eventHandler.DeleteFunc = func(obj any) {
			// Note: The documentation for ResourceEventHandler says:
			//
			//   OnDelete will get the final state of the item if it is known, otherwise it will
			//   get an object of type DeletedFinalStateUnknown. This can happen if the watch is
			//   closed and misses the delete event and we don't notice the deletion until the
			//   subsequent re-list.
			//
			// So we have to check for DeletedFinalStateUnknown before casting it to type T.

			finalStateMayBeStale := false
			if d, ok := obj.(*cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
				finalStateMayBeStale = true
			}

			t := obj.(T)
			handlers.DeleteFunc(t, finalStateMayBeStale)
		}
	}

	var objType T // Only needed to provide the type T to NewInformer
	store, controller := cache.NewInformer(watchlist, objType, 0, eventHandler)
	go controller.Run(stop)

	return WatchStore[T]{store: store}
}

// WatchStore provides an interface for getting information about a list of Ts using the event
// listener from a previous call to Watch
//
// Currently this type does not have any methods; we'll add these when we need them.
type WatchStore[T any] struct {
	store cache.Store
}
