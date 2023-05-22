package schedwatch

import (
	"context"
	"time"

	"github.com/tychoish/fun/pubsub"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/neondatabase/autoscaling/pkg/util"
)

func StartSchedulerWatcher(
	ctx context.Context,
	logger util.PrefixLogger,
	kubeClient *kubernetes.Clientset,
	eventBroker *pubsub.Broker[WatchEvent],
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
				podName := util.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}
				if util.PodReady(pod) {
					if pod.Status.PodIP == "" {
						logger.Errorf("Pod %v is ready but has no IP", podName)
						return
					}

					eventBroker.Publish(ctx, WatchEvent{info: newSchedulerInfo(pod), kind: eventKindReady})
				}
			},
			UpdateFunc: func(oldPod, newPod *corev1.Pod) {
				oldPodName := util.NamespacedName{Name: oldPod.Name, Namespace: oldPod.Namespace}
				newPodName := util.NamespacedName{Name: newPod.Name, Namespace: newPod.Namespace}

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
					eventBroker.Publish(ctx, WatchEvent{kind: eventKindReady, info: newSchedulerInfo(newPod)})
				} else if oldReady && !newReady {
					eventBroker.Publish(ctx, WatchEvent{kind: eventKindDeleted, info: newSchedulerInfo(oldPod)})
				}
			},
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				wasReady := util.PodReady(pod)
				if wasReady {
					eventBroker.Publish(ctx, WatchEvent{kind: eventKindDeleted, info: newSchedulerInfo(pod)})
				}
			},
		},
	)

}
