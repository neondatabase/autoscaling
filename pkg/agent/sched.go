package agent

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/exp/slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type schedulerInfo struct {
	podName api.PodName
	uid     types.UID
	ip      string
}

func newSchedulerInfo(pod *corev1.Pod) schedulerInfo {
	return schedulerInfo{
		podName: api.PodName{Name: pod.Name, Namespace: pod.Namespace},
		uid:     pod.UID,
		ip:      pod.Status.PodIP,
	}
}

// schedulerWatch is the interface returned by watchSchedulerUpdates
type schedulerWatch struct {
	broker *Broker[schedulerInfo]

	using chan<- schedulerInfo

	current *schedulerInfo

	stop         util.SignalSender
	hasScheduler bool
}

func (w schedulerWatch) Using(sched schedulerInfo) {
	w.using <- sched
}

func (w schedulerWatch) Stop() {
	w.stop.Send()
}

const schedulerNamespace string = "kube-system"

func schedulerLabelSelector(schedulerName string) string {
	return fmt.Sprintf("name=%s", schedulerName)
}

type watchEvent struct {
	info schedulerInfo
	kind eventKind
}

type eventKind string

const (
	eventKindReady   eventKind = "ready"
	eventKindDeleted eventKind = "deleted"
)

func watchSchedulerUpdates(
	ctx context.Context,
	logger RunnerLogger,
	kubeClient *kubernetes.Clientset,
	schedulerName string,
) (schedulerWatch, error) {
	events := make(chan watchEvent, 0)
	broker := NewBroker[schedulerInfo]()
	using := make(chan schedulerInfo)
	stopSender, stopListener := util.NewSingleSignalPair()

	state := schedulerWatchState{
		queue:      make([]watchEvent, 0, 1),
		nextReady:  -1,
		nextDelete: -1,
		events:     events,
		store:      nil,
		publish:    broker.Publish,
		using:      using,
		stop:       stopListener,
		logger:     logger,
	}

	setStore := make(chan *util.WatchStore[corev1.Pod])
	defer close(setStore)

	watcher := schedulerWatch{
		broker: broker,
		using:  using,
		stop:   stopSender,
	}

	go state.run(ctx, setStore)
	go broker.Start(ctx)

	store, err := util.Watch(
		ctx,
		kubeClient.CoreV1().Pods(schedulerNamespace),
		util.WatchConfig{
			LogName: "scheduler",
			// We don't need to be super responsive to scheduler changes.
			//
			// FIXME: make these configurable.
			RetryRelistAfter: util.NewTimeRange(time.Second, 4, 5),
			RetryWatchAfter:  util.NewTimeRange(time.Second, 4, 5),
		},
		util.WatchAccessors[*corev1.PodList, corev1.Pod]{
			Items: func(list *corev1.PodList) []corev1.Pod { return list.Items },
		},
		util.InitWatchModeSync,
		metav1.ListOptions{LabelSelector: schedulerLabelSelector(schedulerName)},
		util.WatchHandlerFuncs[*corev1.Pod]{
			AddFunc: func(pod *corev1.Pod, preexisting bool) {
				podName := api.PodName{Name: pod.Name, Namespace: pod.Namespace}
				if util.PodReady(pod) {
					if pod.Status.PodIP == "" {
						logger.Errorf("Pod %v is ready but has no IP", podName)
						return
					}
					watcher.hasScheduler = true
					events <- watchEvent{info: newSchedulerInfo(pod), kind: eventKindReady}
				}
			},
			UpdateFunc: func(oldPod, newPod *corev1.Pod) {
				oldPodName := api.PodName{Name: oldPod.Name, Namespace: oldPod.Namespace}
				newPodName := api.PodName{Name: newPod.Name, Namespace: newPod.Namespace}

				if oldPod.Name != newPod.Name || oldPod.Namespace != newPod.Namespace {
					logger.Errorf(
						"Unexpected scheduler pod update, old pod name %v != new pod name %v",
						oldPodName, newPodName,
					)
				}

				oldReady := util.PodReady(oldPod)
				newReady := util.PodReady(newPod)

				if !oldReady && newReady {
					if newPod.Status.PodIP == "" {
						logger.Errorf("Pod %v is ready but has no IP", newPodName)
						return
					}
					watcher.hasScheduler = true
					events <- watchEvent{kind: eventKindReady, info: newSchedulerInfo(newPod)}
				} else if oldReady && !newReady {
					watcher.hasScheduler = false
					events <- watchEvent{kind: eventKindDeleted, info: newSchedulerInfo(oldPod)}
				}
			},
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				wasReady := util.PodReady(pod)
				if wasReady {
					watcher.hasScheduler = false
					events <- watchEvent{kind: eventKindDeleted, info: newSchedulerInfo(pod)}
				}
			},
		},
	)

	if err != nil {
		watcher.Stop()
		return schedulerWatch{}, fmt.Errorf("Error starting scheduler pod watch: %w", err)
	}

	setStore <- store

	var candidates []*corev1.Pod
	for _, pod := range store.Items() {
		if util.PodReady(pod) {
			candidates = append(candidates, pod)
		}
	}

	if len(candidates) > 1 {
		watcher.Stop()
		return schedulerWatch{}, fmt.Errorf("Multiple initial candidate scheduler pods")
	} else if len(candidates) == 1 && candidates[0].Status.PodIP == "" {
		watcher.Stop()
		return schedulerWatch{}, fmt.Errorf("Scheduler pod is ready but IP is not available")
	}

	if len(candidates) == 0 {
		return watcher, nil
	} else {
		info := newSchedulerInfo(candidates[0])
		watcher.Using(info)
		watcher.current = &info
		watcher.hasScheduler = true
		logger.Infof("Got initial scheduler pod %v (UID = %v) with IP %v",
			info.podName, info.uid, info.ip,
		)
		return watcher, nil

	}
}

type schedulerWatchState struct {
	queue      []watchEvent
	nextReady  int
	nextDelete int

	events <-chan watchEvent
	store  *util.WatchStore[corev1.Pod]

	publish func(ctx context.Context, info schedulerInfo)

	using <-chan schedulerInfo

	stop   util.SignalReceiver
	logger RunnerLogger
}

func (w schedulerWatchState) run(ctx context.Context, setStore chan *util.WatchStore[corev1.Pod]) {
	sndSetStore := make(chan *util.WatchStore[corev1.Pod])
	defer close(sndSetStore)

	defer w.stop.Close()
	defer func() {
		if w.store != nil {
			w.store.Stop()
		} else if store, ok := <-setStore; ok {
			store.Stop()
		}
	}()

	for {
		// NOTE(tycho): This might be right, but I don't have
		// enough of a mental model. It sort of seems like we
		// only really need to store the "last ready" event,
		// and processing more things, though correct,
		// wouldn't produce more desireable behavior.
		var qIdx int
		if w.nextDelete < w.nextReady {
			qIdx = w.nextReady
		} else {
			qIdx = w.nextDelete
		}

		// this is a function so that we can defer as we
		// bridge the interface between channels and the
		// broker interface
		func() {
			opctx, opcancel := context.WithCancel(ctx)
			defer opcancel()
			pipe := make(chan schedulerInfo)
			sig := make(chan struct{})

			go func() {
				defer close(sig)
				select {
				case <-opctx.Done():
					return
				case info := <-pipe:
					w.publish(ctx, info)
				}

			}()

			select {
			case <-ctx.Done():
				return
			case <-w.stop.Recv():
				return
			case store, ok := <-setStore:
				if ok {
					w.store = store
				}
				setStore = sndSetStore
			case e := <-w.events:
				w.handleEvent(e)
			case info := <-w.using:
				w.handleUsing(info)
			case pipe <- w.queue[qIdx].info:
				select {
				case <-ctx.Done():
					return
				case <-w.stop.Recv():
					return
				case <-sig:
					w.handleSent(qIdx)
				}
			}
		}()
	}
}

func (w *schedulerWatchState) resetNextReady() {
	w.nextReady = slices.IndexFunc(w.queue, func(e watchEvent) bool {
		return e.kind == eventKindReady
	})
}

func (w *schedulerWatchState) resetNextDelete() {
	w.nextDelete = slices.IndexFunc(w.queue, func(e watchEvent) bool {
		return e.kind == eventKindDeleted
	})
}

func eventFinder(kind eventKind, uid types.UID) func(watchEvent) bool {
	return func(e watchEvent) bool { return e.kind == kind && e.info.uid == uid }
}

func (w *schedulerWatchState) handleEvent(event watchEvent) {
	w.logger.Infof("Received watch event %+v", event)

	switch event.kind {
	case eventKindReady:
		w.queue = append(w.queue, event)
		if w.nextReady == -1 {
			w.nextReady = len(w.queue) - 1
		}
	case eventKindDeleted:
		w.queue = append(w.queue, event)
		if w.nextDelete == -1 {
			w.nextDelete = len(w.queue) - 1
		}
	}
}

func (w *schedulerWatchState) handleUsing(info schedulerInfo) {
	// If there's a "ready" event for the pod, remove it and recalculate nextReady
	readyIdx := slices.IndexFunc(w.queue, eventFinder(eventKindReady, info.uid))
	if readyIdx != -1 {
		w.queue = slices.Delete(w.queue, readyIdx, readyIdx+1)
		if readyIdx < w.nextDelete {
			w.nextDelete -= 1
		}
		w.resetNextReady()
	}
}

func (w *schedulerWatchState) handleSent(qIdx int) {
	switch w.queue[qIdx].kind {
	case eventKindReady:
		w.queue = slices.Delete(w.queue, qIdx, qIdx+1)
		if w.nextDelete > qIdx {
			w.nextDelete -= 1
		}
		if w.nextReady == qIdx {
			w.resetNextReady()
		}
	case eventKindDeleted:
		// If there was a corresponding unhandled ready event before this, remove it as well
		readyIdx := slices.IndexFunc(w.queue[:qIdx], eventFinder(eventKindReady, w.queue[qIdx].info.uid))
		// Remove the "delete" event
		w.queue = slices.Delete(w.queue, qIdx, qIdx+1)
		if w.nextReady > qIdx {
			w.nextReady -= 1
		} else if readyIdx != -1 {
			// remove the matching "ready" event
			w.queue = slices.Delete(w.queue, readyIdx, readyIdx+1)
			if readyIdx == w.nextReady {
				w.resetNextReady()
			}
			qIdx -= 1 // reduce qIdx for the 'nextDelete == qIdx' check below
		}

		if w.nextDelete == qIdx {
			w.resetNextDelete()
		}
	}
}
