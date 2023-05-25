package schedwatch

import (
	"context"
	"time"

	"github.com/tychoish/fun/pubsub"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

func isActivePod(pod *corev1.Pod) bool {
	return pod.Status.PodIP != "" && util.PodReady(pod)
}

func StartSchedulerWatcher(
	ctx context.Context,
	logger util.PrefixLogger,
	kubeClient *kubernetes.Clientset,
	eventBroker *pubsub.Broker[WatchEvent],
	schedulerName string,
) (*watch.WatchStore[corev1.Pod], error) {
	return watch.Watch(
		ctx,
		kubeClient.CoreV1().Pods(schedulerNamespace),
		watch.WatchConfig{
			LogName: "scheduler",
			// We don't need to be super responsive to scheduler changes.
			//
			// FIXME: make these configurable.
			RetryRelistAfter: util.NewTimeRange(time.Second, 4, 5),
			RetryWatchAfter:  util.NewTimeRange(time.Second, 4, 5),
		},
		watch.WatchAccessors[*corev1.PodList, corev1.Pod]{
			Items: func(list *corev1.PodList) []corev1.Pod { return list.Items },
		},
		watch.InitWatchModeSync,
		metav1.ListOptions{LabelSelector: schedulerLabelSelector(schedulerName)},
		watch.WatchHandlerFuncs[*corev1.Pod]{
			AddFunc: func(pod *corev1.Pod, preexisting bool) {
				if isActivePod(pod) {
					event := WatchEvent{kind: eventKindReady, info: newSchedulerInfo(pod)}
					logger.Infof("New scheduler, already ready: %+v", event)
					eventBroker.Publish(ctx, event)
				}
			},
			UpdateFunc: func(oldPod, newPod *corev1.Pod) {
				oldReady := isActivePod(oldPod)
				newReady := isActivePod(newPod)

				if !oldReady && newReady {
					event := WatchEvent{kind: eventKindReady, info: newSchedulerInfo(newPod)}
					logger.Infof("Existing scheduler became ready: %+v", event)
					eventBroker.Publish(ctx, event)
				} else if oldReady && !newReady {
					event := WatchEvent{kind: eventKindDeleted, info: newSchedulerInfo(oldPod)}
					logger.Infof("Existing scheduler no longer ready: %+v", event)
					eventBroker.Publish(ctx, event)
				}
			},
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				wasReady := isActivePod(pod)
				if wasReady {
					event := WatchEvent{kind: eventKindDeleted, info: newSchedulerInfo(pod)}
					logger.Infof("Previously-ready scheduler deleted: %+v", event)
					eventBroker.Publish(ctx, event)
				}
			},
		},
	)

}
