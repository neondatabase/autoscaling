package plugin

// Implementation of watching for VM deletions and VM migration completions, so we can unreserve the
// associated resources

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/util"
)

// watchPodDeletions continuously tracks pod deletion events and sends each deleted podName on
// either vmDeletions or podDeletions as they occur, depending on whether the pod has the LabelVM
// label.
//
// This method starts its own goroutine, and guarantees that we have started listening for FUTURE
// events once it returns (unless it returns error).
//
// Events occurring before this method is called will not be sent.
func (e *AutoscaleEnforcer) watchPodDeletions(
	ctx context.Context,
	vmDeletions chan<- util.NamespacedName,
	podDeletions chan<- util.NamespacedName,
) error {
	_, err := util.Watch(
		ctx,
		e.handle.ClientSet().CoreV1().Pods(corev1.NamespaceAll),
		util.WatchConfig{
			LogName: "pods",
			// We want to be up-to-date in tracking deletions, so that our reservations are correct.
			//
			// FIXME: make these configurable.
			RetryRelistAfter: util.NewTimeRange(time.Millisecond, 250, 750),
			RetryWatchAfter:  util.NewTimeRange(time.Millisecond, 250, 750),
		},
		util.WatchAccessors[*corev1.PodList, corev1.Pod]{
			Items: func(list *corev1.PodList) []corev1.Pod { return list.Items },
		},
		util.InitWatchModeSync, // note: doesn't matter, because AddFunc = nil.
		metav1.ListOptions{},
		util.WatchHandlerFuncs[*corev1.Pod]{
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				name := util.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}
				klog.Infof("[autoscale-enforcer] watch: Received delete event for pod %v", name)
				if _, ok := pod.Labels[LabelVM]; ok {
					vmDeletions <- name
				} else {
					podDeletions <- name
				}
			},
		},
	)
	return err
}
