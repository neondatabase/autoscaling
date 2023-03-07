package testutil

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/neondatabase/autoscaling/pkg/util"
)

func TestWatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	ch := make(chan util.ProcessEventArgs[corev1.Pod, *corev1.Pod], 1)

	calledDelete := 0
	calledAdd := 0
	calledUpdate := 0
	store, wait := Watch(ctx, ch, util.WatchHandlerFuncs[*corev1.Pod]{
		DeleteFunc: func(*corev1.Pod, bool) { calledDelete++ },
		AddFunc:    func(*corev1.Pod, bool) { calledAdd++ },
		UpdateFunc: func(*corev1.Pod, *corev1.Pod) { calledUpdate++ },
	})
	ch <- util.ProcessEventArgs[corev1.Pod, *corev1.Pod]{
		//nolint:exhaustruct  // for testing
		Event: watch.Event{
			Type: watch.Added,
		},
	}
	time.Sleep(time.Millisecond) // to let to worker run the handcrank

	if items := store.Items(); len(items) != 1 {
		t.Error(items)
	}

	ch <- util.ProcessEventArgs[corev1.Pod, *corev1.Pod]{
		//nolint:exhaustruct  // for testing
		Event: watch.Event{
			Type: watch.Deleted,
		},
	}
	time.Sleep(time.Millisecond) // to let to worker run the handcrank

	if items := store.Items(); len(items) != 0 {
		t.Error(items)
	}
	if err := wait(); err != nil {
		t.Error(err)
	}
	if calledAdd != 1 {
		t.Error("did not call add handler")
	}
	if calledDelete != 1 {
		t.Error("did not call delete handler")
	}
	if calledUpdate != 1 {
		t.Error("did not call update handler")
	}

}
