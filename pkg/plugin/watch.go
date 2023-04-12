package plugin

// Implementation of watching Pods for Pod/VM deletions and changes so that a VM's autoscaling is
// disabled.

// Implementation of watching for VM deletions and VM migration completions, so we can unreserve the
// associated resources

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// watchPodEvents continuously tracks a handful of Pod-related events that we care about. These
// events are: non-VM pod deletion, VM deletion, and VMs that disable scaling.
//
// This method starts its own goroutine, and guarantees that we have started listening for FUTURE
// events once it returns (unless it returns error).
//
// Events occurring before this method is called will not be sent.
//
// The reason we care about when scaling is disabled is that if we don't, we can run into the
// following race condition:
//
//  1. VM created with autoscaling enabled
//  2. Scheduler restarts and reads the state of the cluster. It records the difference between the
//     VM's current and maximum usage as "buffer"
//  3. Before the autoscaler-agent runner for the VM connects to the scheduler, the VM's label to
//     enable autoscaling is removed, and the autoscaler-agent's runner exits.
//  4. final state: The scheduler retains buffer for a VM that can't scale.
//
// To avoid (4) occurring, we track events where autoscaling is disabled for a VM and remove its
// "buffer" when that happens. There's still some other possibilities for race conditions (FIXME),
// but those are a little harder to handlle - in particular:
//
//  1. Scheduler exits
//  2. autoscaler-agent runner downscales
//  3. Scheduler starts, reads cluster state
//  4. VM gets autoscaling disabled
//  5. Scheduler removes the VM's buffer
//  6. Before noticing that event, the autoscaler-agent upscales the VM and informs the scheduler of
//     its current allocation (which it can do, because it was approved by a previous scheduler).
//  7. The scheduler denies what it sees as upscaling.
//
// This one requires a very unlikely sequence of events to occur, that should be appropriately
// handled by cancelled contexts in *almost all* cases.
func (e *AutoscaleEnforcer) watchPodEvents(
	ctx context.Context,
	vmDeletions chan<- util.NamespacedName,
	vmDisabledScaling chan<- util.NamespacedName,
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
			UpdateFunc: func(oldPod, newPod *corev1.Pod) {
				// note: it doesn't matter whether we use newPod or oldPod for the name here; the
				// pod name and namespace aren't allowed to change.
				name := util.NamespacedName{Name: newPod.Name, Namespace: newPod.Namespace}
				_, isVM := oldPod.Labels[LabelVM]
				if isVM && api.HasAutoscalingEnabled(oldPod) && !api.HasAutoscalingEnabled(newPod) {
					klog.Infof(
						"[autoscale-enforcer] watch: Received update to disable autoscaling for pod %v",
						name,
					)
					vmDisabledScaling <- name
				}
			},
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
