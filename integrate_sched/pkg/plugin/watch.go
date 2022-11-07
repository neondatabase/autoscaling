package plugin

// Implementation of watching for VM deletions and VM migration completions, so we can unreserve the
// associated resources

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	klog "k8s.io/klog/v2"

	virtapi "github.com/neondatabase/virtink/pkg/apis/virt/v1alpha1"

	"github.com/neondatabase/autoscaling/pkg/api"
)

// watchVMDeletions continuously tracks pod deletion events and sends each deleted VM podName on
// deletions as they occur.
//
// This method starts its own goroutine, and guarantees that we have started listening for FUTURE
// events once it returns (unless it returns error).
//
// Events occuring before this method is called will not be sent.
func (e *AutoscaleEnforcer) watchVMDeletions(ctx context.Context, deletions chan<- api.PodName) error {
	// Only listen for events on VM pods. We might get false positives where some pods are another
	// scheduler's VMs, but that's ok.
	//
	// Setting LabelSelector = LabelVM means that we're selecting for pods that *have* that label,
	// ignoring the contents of that label.
	opts := metav1.ListOptions{LabelSelector: LabelVM}
	// note: using .Pods("") sets "" as the namespace, which watches on all namespaces.
	watcher, err := e.handle.ClientSet().CoreV1().Pods("").Watch(ctx, opts)
	if err != nil {
		return fmt.Errorf("Error starting watching VM deletions: %s", err)
	}

	// Listen to the events in a separate goroutine...
	klog.Infof("[autoscale-enforcer] Starting VM event listener")
	go func() {
		events := watcher.ResultChan()
		defer close(deletions)

		for {
			for event := range events {
				if event.Type == watch.Deleted {
					pod := event.Object.(*corev1.Pod)
					name := api.PodName{Name: pod.Name, Namespace: pod.Namespace}
					klog.Infof("[autoscale-enforcer] watch: Received delete event for pod %v", name)
					deletions <- name
				}
			}

			klog.Error("[autoscale-enforcer] watch: VM event listener unexpectedly stopped, restarting")

			watcher, err := e.handle.ClientSet().CoreV1().Pods("").Watch(ctx, opts)
			if err != nil {
				klog.Fatalf("[autoscale-enforcer] watch: Failed to restart VM event listener: %s", err)
			}

			events = watcher.ResultChan()
		}
	}()

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
