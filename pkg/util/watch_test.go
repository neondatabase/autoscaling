package util

import (
	"strings"
	"testing"

	"github.com/tychoish/fun"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestProcessEvent(t *testing.T) {
	t.Run("Add", func(t *testing.T) {
		store := NewWatchStore[corev1.Pod]()
		calledHandler := false
		err := ProcessEvent(
			store,
			WatchHandlerFuncs[*corev1.Pod]{
				AddFunc: func(*corev1.Pod, bool) { calledHandler = true },
			},
			ProcessEventArgs[corev1.Pod, *corev1.Pod]{
				//nolint:exhaustruct  // for testing
				Event: watch.Event{
					Type: watch.Added,
				},
			},
		)
		t.Run("AddedOneEvent", func(t *testing.T) {
			if err != nil {
				t.Error(err)
			}
			if !calledHandler {
				t.Error("event hook was not called")
			}
			if len(store.objects) != 1 {
				t.Error("store not updated")
			}
		})
		t.Run("DuplicateEvent", func(t *testing.T) {
			err := ProcessEvent(
				store,
				WatchHandlerFuncs[*corev1.Pod]{
					AddFunc: func(*corev1.Pod, bool) { calledHandler = true },
				},
				ProcessEventArgs[corev1.Pod, *corev1.Pod]{
					//nolint:exhaustruct  // for testing
					Event: watch.Event{
						Type: watch.Added,
					},
				},
			)
			if err == nil {
				t.Error("expected error")
			}
		})
	})
	t.Run("Delete", func(t *testing.T) {
		store := NewWatchStore[corev1.Pod]()
		calledDelete := 0
		calledAdd := 0
		calledUpdate := 0
		handlers := WatchHandlerFuncs[*corev1.Pod]{
			DeleteFunc: func(*corev1.Pod, bool) { calledDelete++ },
			AddFunc:    func(*corev1.Pod, bool) { calledAdd++ },
			UpdateFunc: func(*corev1.Pod, *corev1.Pod) { calledUpdate++ },
		}

		err := ProcessEvent(
			store,
			handlers,
			ProcessEventArgs[corev1.Pod, *corev1.Pod]{
				//nolint:exhaustruct  // for testing
				Event: watch.Event{
					Type: watch.Added,
				},
			},
		)
		if err != nil {
			t.Error(err)
		}
		if calledDelete > 0 {
			t.Error("should not have called handler yet")
		}
		if len(store.objects) != 1 {
			t.Error(len(store.objects))
		}

		t.Run("DeleteObject", func(t *testing.T) {
			err := ProcessEvent(
				store,
				handlers,
				ProcessEventArgs[corev1.Pod, *corev1.Pod]{
					//nolint:exhaustruct  // for testing
					Event: watch.Event{
						Type: watch.Deleted,
					},
				},
			)
			if err != nil {
				t.Error(err)
			}
			if calledDelete != 1 {
				t.Error("should have called handler once")
			}
			if calledUpdate != 1 {
				t.Error("should have called update handler once")
			}
			if len(store.objects) != 0 {
				t.Error(len(store.objects))
			}
		})
		t.Run("DeleteNonExistingObject", func(t *testing.T) {
			err := ProcessEvent(
				store,
				handlers,
				ProcessEventArgs[corev1.Pod, *corev1.Pod]{
					//nolint:exhaustruct  // for testing
					Event: watch.Event{
						Type: watch.Deleted,
					},
				},
			)
			if err == nil {
				t.Error(err)
			}

			if calledDelete != 1 {
				t.Error("should only have called handler once")
			}
			if len(store.objects) != 0 {
				t.Error(len(store.objects))
			}
		})
	})
	t.Run("PanicsForErrors", func(t *testing.T) {
		store := NewWatchStore[corev1.Pod]()
		calledHandler := false
		err, perr := fun.Safe(func() error {
			return ProcessEvent(
				store,
				WatchHandlerFuncs[*corev1.Pod]{
					AddFunc: func(*corev1.Pod, bool) { calledHandler = true },
				},
				ProcessEventArgs[corev1.Pod, *corev1.Pod]{
					//nolint:exhaustruct  // for testing
					Event: watch.Event{
						Type: watch.Error,
					},
				},
			)
		})
		if err != nil {
			t.Error("we caught the panic so we don't expect an error")
		}
		if perr == nil {
			t.Fatal("expected panic")
		}
		if !strings.Contains(perr.Error(), "unreachable code") {
			t.Error(perr)
		}
		if calledHandler {
			t.Error("should not have called")
		}
	})
}
