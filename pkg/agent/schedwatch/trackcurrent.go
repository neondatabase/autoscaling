package schedwatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/tychoish/fun/pubsub"
	"go.uber.org/zap"
	"golang.org/x/exp/slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

// SchedulerWatch is the interface returned by WatchSchedulerUpdates
type SchedulerWatch struct {
	ReadyQueue <-chan SchedulerInfo
	Deleted    <-chan SchedulerInfo

	cmd   chan<- watchCmd
	using chan<- SchedulerInfo

	stop            util.SignalSender[struct{}]
	stopEventStream func()
}

func (w SchedulerWatch) ExpectingDeleted() {
	w.cmd <- watchCmdDeleted
}

func (w SchedulerWatch) ExpectingReady() {
	w.cmd <- watchCmdReady
}

func (w SchedulerWatch) Using(sched SchedulerInfo) {
	w.using <- sched
}

func (w SchedulerWatch) Stop() {
	w.stopEventStream()
	w.stop.Send(struct{}{})
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

func WatchSchedulerUpdates(
	ctx context.Context,
	logger *zap.Logger,
	eventBroker *pubsub.Broker[WatchEvent],
	store *watch.Store[corev1.Pod],
) (SchedulerWatch, *SchedulerInfo, error) {
	events := eventBroker.Subscribe(ctx)
	readyQueue := make(chan SchedulerInfo)
	deleted := make(chan SchedulerInfo)
	cmd := make(chan watchCmd)
	using := make(chan SchedulerInfo)
	stopSender, stopListener := util.NewSingleSignalPair[struct{}]()

	state := schedulerWatchState{
		queue:      make([]WatchEvent, 0, 1),
		nextReady:  -1,
		nextDelete: -1,
		mode:       watchCmdReady,
		events:     events,
		readyQueue: readyQueue,
		deleted:    deleted,
		cmd:        cmd,
		using:      using,
		stop:       stopListener,
		logger:     logger,
	}

	watcher := SchedulerWatch{
		ReadyQueue:      readyQueue,
		Deleted:         deleted,
		cmd:             cmd,
		using:           using,
		stop:            stopSender,
		stopEventStream: func() { eventBroker.Unsubscribe(ctx, events) },
	}
	go state.run(ctx)

	var candidates []*corev1.Pod
	for _, pod := range store.Items() {
		if util.PodReady(pod) {
			candidates = append(candidates, pod)
		}
	}

	if len(candidates) > 1 {
		watcher.Stop()
		return SchedulerWatch{}, nil, errors.New("Multiple initial candidate scheduler pods")
	} else if len(candidates) == 1 && candidates[0].Status.PodIP == "" {
		watcher.Stop()
		return SchedulerWatch{}, nil, errors.New("Scheduler pod is ready but IP is not available")
	}

	if len(candidates) == 0 {
		return watcher, nil, nil
	} else {
		info := newSchedulerInfo(candidates[0])
		return watcher, &info, nil
	}
}

type schedulerWatchState struct {
	queue      []WatchEvent
	nextReady  int
	nextDelete int

	mode   watchCmd
	events <-chan WatchEvent

	readyQueue chan<- SchedulerInfo
	deleted    chan<- SchedulerInfo

	cmd   <-chan watchCmd
	using <-chan SchedulerInfo

	stop   util.SignalReceiver[struct{}]
	logger *zap.Logger
}

func (w schedulerWatchState) run(ctx context.Context) {
	defer w.stop.Close()

	for {
		var sendCh chan<- SchedulerInfo
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
	w.nextReady = slices.IndexFunc(w.queue, func(e WatchEvent) bool {
		return e.kind == eventKindReady
	})
}

func (w *schedulerWatchState) resetNextDelete() {
	w.nextDelete = slices.IndexFunc(w.queue, func(e WatchEvent) bool {
		return e.kind == eventKindDeleted
	})
}

func eventFinder(kind eventKind, uid types.UID) func(WatchEvent) bool {
	return func(e WatchEvent) bool { return e.kind == kind && e.info.UID == uid }
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
		var newQueue []WatchEvent

		for i := range w.queue {
			switch w.queue[i].kind {
			case eventKindReady:
				finder := eventFinder(eventKindDeleted, w.queue[i].info.UID)
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

func (w *schedulerWatchState) handleEvent(event WatchEvent) {
	w.logger.Info("Received watch event", zap.Object("event", event))

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
			readyIdx := slices.IndexFunc(w.queue, eventFinder(eventKindReady, event.info.UID))
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

func (w *schedulerWatchState) handleUsing(info SchedulerInfo) {
	// If there's a "ready" event for the pod, remove it and recalculate nextReady
	readyIdx := slices.IndexFunc(w.queue, eventFinder(eventKindReady, info.UID))
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
		readyIdx := slices.IndexFunc(w.queue[:qIdx], eventFinder(eventKindReady, w.queue[qIdx].info.UID))
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
