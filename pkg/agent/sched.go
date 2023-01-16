package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/tychoish/fun"
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
	ReadyQueue <-chan schedulerInfo
	Deleted    <-chan schedulerInfo

	cmd   chan<- watchCmd
	using chan<- schedulerInfo

	stop            util.SignalSender
	stopEventStream func()
}

func (w schedulerWatch) ExpectingDeleted() {
	w.cmd <- watchCmdDeleted
}

func (w schedulerWatch) ExpectingReady() {
	w.cmd <- watchCmdReady
}

func (w schedulerWatch) Using(sched schedulerInfo) {
	w.using <- sched
}

func (w schedulerWatch) Stop() {
	w.stopEventStream()
	w.stop.Send()
}

const schedulerNamespace string = "kube-system"

func schedulerLabelSelector(schedulerName string) string {
	return fmt.Sprintf("name=%s", schedulerName)
}

type watchCmd string

const (
	watchCmdDeleted watchCmd = "expecting deleted"
	watchCmdReady   watchCmd = "expecting ready"
)

type watchEvent struct {
	info schedulerInfo
	kind eventKind
}

type eventKind string

const (
	eventKindReady   eventKind = "ready"
	eventKindDeleted eventKind = "deleted"
)

func startSchedulerWatcher(
	ctx context.Context,
	logger RunnerLogger,
	kubeClient *kubernetes.Clientset,
	eventBroker *fun.Broker[watchEvent],
	schedulerName string,
) (*util.WatchStore[corev1.Pod], error) {
	return util.Watch(
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

					eventBroker.Publish(ctx, watchEvent{info: newSchedulerInfo(pod), kind: eventKindReady})
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
					eventBroker.Publish(ctx, watchEvent{kind: eventKindReady, info: newSchedulerInfo(newPod)})
				} else if oldReady && !newReady {
					eventBroker.Publish(ctx, watchEvent{kind: eventKindDeleted, info: newSchedulerInfo(oldPod)})
				}
			},
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				wasReady := util.PodReady(pod)
				if wasReady {
					eventBroker.Publish(ctx, watchEvent{kind: eventKindDeleted, info: newSchedulerInfo(pod)})
				}
			},
		},
	)

}

func watchSchedulerUpdates(
	ctx context.Context,
	logger RunnerLogger,
	eventBroker *fun.Broker[watchEvent],
	store *util.WatchStore[corev1.Pod],
) (schedulerWatch, *schedulerInfo, error) {
	events := eventBroker.Subscribe(ctx)
	readyQueue := make(chan schedulerInfo)
	deleted := make(chan schedulerInfo)
	cmd := make(chan watchCmd)
	using := make(chan schedulerInfo)
	stopSender, stopListener := util.NewSingleSignalPair()

	state := schedulerWatchState{
		queue:      make([]watchEvent, 0, 1),
		nextReady:  -1,
		nextDelete: -1,
		mode:       watchCmdReady,
		events:     events,
		store:      nil,
		readyQueue: readyQueue,
		deleted:    deleted,
		cmd:        cmd,
		using:      using,
		stop:       stopListener,
		logger:     logger,
	}

	setStore := make(chan *util.WatchStore[corev1.Pod])
	defer close(setStore)

	watcher := schedulerWatch{
		ReadyQueue:      readyQueue,
		Deleted:         deleted,
		cmd:             cmd,
		using:           using,
		stop:            stopSender,
		stopEventStream: func() { eventBroker.Unsubscribe(ctx, events) },
	}
	go state.run(ctx, setStore)

	setStore <- store

	var candidates []*corev1.Pod
	for _, pod := range store.Items() {
		if util.PodReady(pod) {
			candidates = append(candidates, pod)
		}
	}

	if len(candidates) > 1 {
		watcher.Stop()
		return schedulerWatch{}, nil, fmt.Errorf("Multiple initial candidate scheduler pods")
	} else if len(candidates) == 1 && candidates[0].Status.PodIP == "" {
		watcher.Stop()
		return schedulerWatch{}, nil, fmt.Errorf("Scheduler pod is ready but IP is not available")
	}

	if len(candidates) == 0 {
		return watcher, nil, nil
	} else {
		info := newSchedulerInfo(candidates[0])
		return watcher, &info, nil
	}
}

type schedulerWatchState struct {
	queue      []watchEvent
	nextReady  int
	nextDelete int

	mode   watchCmd
	events <-chan watchEvent
	store  *util.WatchStore[corev1.Pod]

	readyQueue chan<- schedulerInfo
	deleted    chan<- schedulerInfo

	cmd   <-chan watchCmd
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
		var sendCh chan<- schedulerInfo
		var qIdx int
		switch w.mode {
		case watchCmdReady:
			sendCh = w.readyQueue
			qIdx = w.nextReady
		case watchCmdDeleted:
			sendCh = w.deleted
			qIdx = w.nextDelete
		}

		if qIdx == -1 {
			// Nothing to send
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
			case newMode := <-w.cmd:
				w.handleNewMode(newMode)
			case e := <-w.events:
				w.handleEvent(e)
			case info := <-w.using:
				w.handleUsing(info)
			}
		} else {
			// Something to send
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
			case newMode := <-w.cmd:
				w.handleNewMode(newMode)
			case e := <-w.events:
				w.handleEvent(e)
			case info := <-w.using:
				w.handleUsing(info)
			case sendCh <- w.queue[qIdx].info:
				w.handleSent(qIdx)
			}
		}
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

func (w *schedulerWatchState) handleNewMode(newMode watchCmd) {
	if newMode == w.mode {
		return
	}
	w.mode = newMode

	switch w.mode {
	case watchCmdDeleted:
		// When switching from "ready" to "deleted", there's nothing extra we need to do. We just
		// stop removing ready-delete pairs, which will be done on further calls to handleEvent.
		return
	case watchCmdReady:
		// When switching to "ready", remove all unprocessed deletion events *and* all creation
		// events that have a linked deletion event
		//
		// Removing the deletion events isn't *strictly* necessary, but it means that we don't
		// process or store extra deletions when we don't need to.

		w.nextReady = -1
		w.nextDelete = -1

		// Gradually create the new queue, with all of the deletion events removed
		var newQueue []watchEvent

		for i := range w.queue {
			switch w.queue[i].kind {
			case eventKindReady:
				finder := eventFinder(eventKindDeleted, w.queue[i].info.uid)
				matchingDeletion := slices.IndexFunc(w.queue[i+1:], finder)
				// Only if there's no matching deletion, add it to the new queue
				if matchingDeletion == -1 {
					newQueue = append(newQueue, w.queue[i])
				}
			case eventKindDeleted:
				// Filter out all deletion events. (don't add this to the new queue)
			}
		}

		if len(newQueue) != 0 {
			w.nextReady = 0
		}

		w.queue = newQueue
	}
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
		switch w.mode {
		case watchCmdReady:
			// If there was a prior "ready" event, remove both. Otherwise add the deletion
			readyIdx := slices.IndexFunc(w.queue, eventFinder(eventKindReady, event.info.uid))
			if readyIdx != -1 {
				w.queue = slices.Delete(w.queue, readyIdx, readyIdx+1)
				// if nextReady < readyIdx, we don't need to do anything. And we can't have
				// nextReady > readyIdx (because then it wouldn't be nextReady).
				if w.nextReady == readyIdx {
					w.resetNextReady()
				}
			} else {
				w.queue = append(w.queue, event)
				if w.nextDelete == -1 {
					w.nextDelete = len(w.queue) - 1
				}
			}
		case watchCmdDeleted:
			w.queue = append(w.queue, event)
			if w.nextDelete == -1 {
				w.nextDelete = len(w.queue) - 1
			}
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
