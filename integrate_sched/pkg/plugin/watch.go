package plugin

// Implementation of watching for VM deletions and VM migration completions, so we can unreserve the
// associated resources

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"

	virtapi "github.com/neondatabase/virtink/pkg/apis/virt/v1alpha1"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// watchVMDeletions continuously tracks pod deletion events and sends each deleted VM podName on
// deletions as they occur.
//
// This method starts its own goroutine, and guarantees that we have started listening for FUTURE
// events once it returns (unless it returns error).
//
// Events occuring before this method is called will not be sent.
func (e *AutoscaleEnforcer) watchVMDeletions(ctx context.Context, deletions chan<- api.PodName) error {
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
				deletions <- name
			},
		},
		// Setting LabelSelector = LabelVM means that we're selecting for pods that *have* that
		// label, ignoring the contents of that label.
		func(options *metav1.ListOptions) {
			options.LabelSelector = LabelVM
		},
	)

	return nil
}

// removeOldMigrations is called periodically to clean up the VirtualMachineMigrations that Virtink
// hasn't
//
// This was originally supposed to be implemented in a similar fashion to watchVMDeletions, but that
// was erroring on startup, and the error messages were too annoying to fix.
func (e *AutoscaleEnforcer) removeOldMigrations(ctx context.Context) error {
	vmms, err := e.virtClient.VirtV1alpha1().VirtualMachineMigrations("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("Error listing migrations: %s", err)
	}

	for _, vmm := range vmms.Items {
		name := api.PodName{Name: vmm.Name, Namespace: vmm.Namespace}

		shouldDelete := vmm.Status.Phase == virtapi.VirtualMachineMigrationSucceeded

		if vmm.Status.Phase == virtapi.VirtualMachineMigrationFailed {
			klog.Infof("[autoscale-enforcer] Deleting failed migration %v", name)
			shouldDelete = true
		}

		if shouldDelete {
			e.handleVMMFinished(ctx, name)
		}
	}

	return nil
}
