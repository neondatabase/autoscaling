package plugin

// Implementation of watching for VM deletions and VM migration completions, so we can unreserve the
// associated resources

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// watchPodDeletions continuously tracks pod deletion events and sends each deleted podName on
// either vmDeletions or podDeletions as they occur, depending on whether the pod has the LabelVM
// label.
//
// This method starts its own goroutine, and guarantees that we have started listening for FUTURE
// events once it returns (unless it returns error).
//
// Events occuring before this method is called will not be sent.
func (e *AutoscaleEnforcer) watchPodDeletions(
	ctx context.Context, vmDeletions chan<- api.PodName, podDeletions chan<- api.PodName,
) error {
	// We're using the client-go cache here (indirectly through util.Watch) so that we don't miss
	// deletion events. Otherwise, we can run into race conditions where events are missed in the
	// small gap between event stream restarts. In practice the chance of that occuring is
	// *incredibly* small, but it's still imperative that we avoid it.

	stop := make(chan struct{})
	_ = util.Watch(
		e.handle.ClientSet().CoreV1().RESTClient(),
		stop,
		corev1.ResourcePods,
		corev1.NamespaceAll,
		util.WatchHandlerFuncs[*corev1.Pod]{
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				name := api.PodName{Name: pod.Name, Namespace: pod.Namespace}
				klog.Infof("[autoscale-enforcer] watch: Received delete event for pod %v", name)
				if _, ok := pod.Labels[LabelVM]; ok {
					vmDeletions <- name
				} else {
					podDeletions <- name
				}
			},
		},
		func(options *metav1.ListOptions) {},
	)

	return nil
}
