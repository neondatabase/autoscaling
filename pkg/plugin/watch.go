package plugin

// Implementation of watching for Pod deletions and changes to a VM's scaling settings (either
// whether it's disabled, or the scaling bounds themselves).

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

// watchPodEvents continuously tracks a handful of Pod-related events that we care about. These
// events are pod deletion or completion for VM and non-VM pods.
//
// This method starts its own goroutine, and guarantees that we have started listening for FUTURE
// events once it returns (unless it returns error).
//
// Events occurring before this method is called will not be sent.
func (e *AutoscaleEnforcer) watchPodEvents(
	ctx context.Context,
	metrics watch.Metrics,
	submitVMDeletion func(util.NamespacedName),
	submitPodDeletion func(util.NamespacedName),
) error {
	_, err := watch.Watch(
		ctx,
		e.handle.ClientSet().CoreV1().Pods(corev1.NamespaceAll),
		watch.Config{
			LogName: "pods",
			Metrics: watch.MetricsConfig{
				Metrics:  metrics,
				Instance: "Pods",
			},
			// We want to be up-to-date in tracking deletions, so that our reservations are correct.
			//
			// FIXME: make these configurable.
			RetryRelistAfter: util.NewTimeRange(time.Millisecond, 250, 750),
			RetryWatchAfter:  util.NewTimeRange(time.Millisecond, 250, 750),
		},
		watch.Accessors[*corev1.PodList, corev1.Pod]{
			Items: func(list *corev1.PodList) []corev1.Pod { return list.Items },
		},
		watch.InitModeSync, // note: doesn't matter, because AddFunc = nil.
		metav1.ListOptions{},
		watch.HandlerFuncs[*corev1.Pod]{
			UpdateFunc: func(oldPod *corev1.Pod, newPod *corev1.Pod) {
				if !util.PodCompleted(oldPod) && util.PodCompleted(newPod) {
					name := util.NamespacedName{Name: newPod.Name, Namespace: newPod.Namespace}
					klog.Infof("[autoscale-enforcer] watch: Received update event for completion of pod %v", name)

					if _, ok := newPod.Labels[LabelVM]; ok {
						submitVMDeletion(name)
					} else {
						submitPodDeletion(name)
					}
				}
			},
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				name := util.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}
				if util.PodCompleted(pod) {
					klog.Infof("[autoscale-enforcer] watch: Received delete event for completed pod %v", name)
				} else {
					klog.Infof("[autoscale-enforcer] watch: Received delete event for pod %v", name)
					if _, ok := pod.Labels[LabelVM]; ok {
						submitVMDeletion(name)
					} else {
						submitPodDeletion(name)
					}
				}
			},
		},
	)
	if err != nil {
		return fmt.Errorf("Error watching pod deletions: %w", err)
	}

	return nil
}

// watchVMEvents watches for changes in VMs: signaling when scaling becomes disabled and updating
// stored information when scaling bounds change.
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
func (e *AutoscaleEnforcer) watchVMEvents(
	ctx context.Context,
	metrics watch.Metrics,
	submitVMDisabledScaling func(util.NamespacedName),
	submitVMBoundsChanged func(_ *api.VmInfo, podName string),
	submitNonAutoscalingVmUsageChanged func(_ *api.VmInfo, podname string),
) (*watch.Store[vmapi.VirtualMachine], error) {
	return watch.Watch(
		ctx,
		e.vmClient.NeonvmV1().VirtualMachines(corev1.NamespaceAll),
		watch.Config{
			LogName: "VMs",
			Metrics: watch.MetricsConfig{
				Metrics:  metrics,
				Instance: "VirtualMachines",
			},
			// FIXME: make these durations configurable.
			RetryRelistAfter: util.NewTimeRange(time.Millisecond, 250, 750),
			RetryWatchAfter:  util.NewTimeRange(time.Millisecond, 250, 750),
		},
		watch.Accessors[*vmapi.VirtualMachineList, vmapi.VirtualMachine]{
			Items: func(list *vmapi.VirtualMachineList) []vmapi.VirtualMachine { return list.Items },
		},
		watch.InitModeSync, // Must sync here so that initial cluster state is read correctly.
		metav1.ListOptions{},
		watch.HandlerFuncs[*vmapi.VirtualMachine]{
			UpdateFunc: func(oldVM, newVM *vmapi.VirtualMachine) {
				oldInfo, err := api.ExtractVmInfo(oldVM)
				if err != nil {
					klog.Errorf("[autoscale-enforcer] Error extracting VM info for %v: %s", util.GetNamespacedName(oldVM), err)
					return
				}
				newInfo, err := api.ExtractVmInfo(newVM)
				if err != nil {
					klog.Errorf("[autoscale-enforcer] Error extracting VM info for %v: %s", util.GetNamespacedName(newVM), err)
					return
				}

				if newVM.Status.PodName == "" {
					klog.Infof(
						"[autoscale-enforcer] Skipping update for VM %v because .status.podName is empty",
						util.GetNamespacedName(newVM),
					)
					return
				}

				if oldInfo.ScalingEnabled && !newInfo.ScalingEnabled {
					name := util.NamespacedName{Namespace: newInfo.Namespace, Name: newVM.Status.PodName}
					klog.Infof(
						"[autoscale-enforcer] watch: Received update to disable autoscaling for pod %v",
						name,
					)
					submitVMDisabledScaling(name)
				}

				if (!oldInfo.ScalingEnabled || !newInfo.ScalingEnabled) && oldInfo.Using() != newInfo.Using() {
					podName := util.NamespacedName{Namespace: newInfo.Namespace, Name: newVM.Status.PodName}
					klog.Infof(
						"[autoscale-enforcer] detected change usage for pod %v non-autoscaling VM %v",
						podName, newInfo.Name,
					)
					submitNonAutoscalingVmUsageChanged(newInfo, podName.Name)
				}

				// If the pod changed, then we're going to handle a deletion event for the old pod,
				// plus creation event for the new pod. Don't worry about it - because all VM
				// information comes from this watch.Store anyways, there's no possibility of missing
				// an update.
				if oldVM.Status.PodName != newVM.Status.PodName {
					return
				}

				// If bounds didn't change, then no need to update
				if oldInfo.EqualScalingBounds(*newInfo) {
					return
				}

				submitVMBoundsChanged(newInfo, newVM.Status.PodName)
			},
		},
	)
}
