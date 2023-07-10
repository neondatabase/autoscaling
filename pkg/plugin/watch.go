package plugin

// Implementation of watching for Pod deletions and changes to a VM's scaling settings (either
// whether it's disabled, or the scaling bounds themselves).

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

type nodeWatchCallbacks struct {
	submitNodeDeletion func(*zap.Logger, string)
}

// watchNodeEvents watches for any deleted Nodes, so that we can clean up the resources that were
// associated with them.
func (e *AutoscaleEnforcer) watchNodeEvents(
	ctx context.Context,
	parentLogger *zap.Logger,
	metrics watch.Metrics,
	callbacks nodeWatchCallbacks,
) (*watch.Store[corev1.Node], error) {
	logger := parentLogger.Named("node-watch")

	return watch.Watch(
		ctx,
		logger.Named("watch"),
		e.handle.ClientSet().CoreV1().Nodes(),
		watch.Config{
			ObjectNameLogField: "node",
			Metrics: watch.MetricsConfig{
				Metrics:  metrics,
				Instance: "Nodes",
			},
			// FIXME: make these configurable.
			RetryRelistAfter: util.NewTimeRange(time.Second, 3, 5),
			RetryWatchAfter:  util.NewTimeRange(time.Second, 3, 5),
		},
		watch.Accessors[*corev1.NodeList, corev1.Node]{
			Items: func(list *corev1.NodeList) []corev1.Node { return list.Items },
		},
		watch.InitModeSync,
		metav1.ListOptions{},
		watch.HandlerFuncs[*corev1.Node]{
			DeleteFunc: func(node *corev1.Node, mayBeStale bool) {
				logger.Info("Received delete event for node", zap.String("node", node.Name))
				callbacks.submitNodeDeletion(logger, node.Name)
			},
		},
	)
}

type podWatchCallbacks struct {
	submitVMDeletion        func(*zap.Logger, util.NamespacedName)
	submitPodDeletion       func(*zap.Logger, util.NamespacedName)
	submitPodStartMigration func(_ *zap.Logger, podName, migrationName util.NamespacedName, source bool)
	submitPodEndMigration   func(_ *zap.Logger, podName, migrationName util.NamespacedName)
}

// watchPodEvents continuously tracks a handful of Pod-related events that we care about. These
// events are pod deletion or completion for VM and non-VM pods.
//
// This method starts its own goroutine, and guarantees that we have started listening for FUTURE
// events once it returns (unless it returns error).
//
// Events occurring before this method is called will not be sent.
func (e *AutoscaleEnforcer) watchPodEvents(
	ctx context.Context,
	parentLogger *zap.Logger,
	metrics watch.Metrics,
	callbacks podWatchCallbacks,
) error {
	logger := parentLogger.Named("pod-watch")

	_, err := watch.Watch(
		ctx,
		logger.Named("watch"),
		e.handle.ClientSet().CoreV1().Pods(corev1.NamespaceAll),
		watch.Config{
			ObjectNameLogField: "pod",
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
				name := util.GetNamespacedName(newPod)

				// Check if pod is "completed" - handle that the same as deletion.
				if !util.PodCompleted(oldPod) && util.PodCompleted(newPod) {
					logger.Info("Received update event for completion of pod", zap.Object("pod", name))

					if _, ok := newPod.Labels[LabelVM]; ok {
						callbacks.submitVMDeletion(logger, name)
					} else {
						callbacks.submitPodDeletion(logger, name)
					}
					return // no other handling worthwhile if the pod's done.
				}

				// CHeck if the pod is part of a new migration, or if a migration it *was* part of
				// has now ended.
				oldMigration := tryMigrationOwnerReference(oldPod)
				newMigration := tryMigrationOwnerReference(newPod)

				if oldMigration == nil && newMigration != nil {
					isSource := podOwnedByVirtualMachine(newPod)
					callbacks.submitPodStartMigration(logger, name, *newMigration, isSource)
				} else if oldMigration != nil && newMigration == nil {
					callbacks.submitPodEndMigration(logger, name, *oldMigration)
				}
			},
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				name := util.GetNamespacedName(pod)

				if util.PodCompleted(pod) {
					logger.Info("Received delete event for completed pod", zap.Object("pod", name))
				} else {
					logger.Info("Received delete event for pod", zap.Object("pod", name))
					if _, ok := pod.Labels[LabelVM]; ok {
						callbacks.submitVMDeletion(logger, name)
					} else {
						callbacks.submitPodDeletion(logger, name)
					}
				}
			},
		},
	)
	return err
}

// tryMigrationOwnerReference returns the name of the owning migration, if this pod *is* owned by a
// VirtualMachineMigration. Otherwise returns nil.
func tryMigrationOwnerReference(pod *corev1.Pod) *util.NamespacedName {
	for _, ref := range pod.OwnerReferences {
		// For NeonVM, *at time of writing*, the OwnerReference has an APIVersion of
		// "vm.neon.tech/v1". But:
		//
		// 1. It's good to be extra-safe around possible name collisions for the
		//    "VirtualMachineMigration" name, even though *practically* it's not going to happen;
		// 2. We can disambiguate with the APIVersion; and
		// 3. We don't want to match on a fixed version, in case we want to change the version
		//    number later.
		//
		// So, given that the format is "<NAME>/<VERSION>", we can just match on the "<NAME>/" part
		// of the APIVersion to have the safety we want with the flexibility we need.
		if strings.HasPrefix(ref.APIVersion, "vm.neon.tech/") && ref.Kind == "VirtualMachineMigration" {
			// note: OwnerReferences are not permitted to have a different namespace than the owned
			// object, so because VirtualMachineMigrations are namespaced, it must have the same
			// namespace as the Pod.
			return &util.NamespacedName{Namespace: pod.Namespace, Name: ref.Name}
		}
	}

	return nil
}

func podOwnedByVirtualMachine(pod *corev1.Pod) bool {
	// For details, see tryMigrationOwnerReference
	for _, ref := range pod.OwnerReferences {
		if strings.HasPrefix(ref.APIVersion, "vm.neon.tech/") && ref.Kind == "VirtualMachine" {
			return true
		}
	}

	return false
}

type vmWatchCallbacks struct {
	submitVMDisabledScaling            func(_ *zap.Logger, podName util.NamespacedName)
	submitVMBoundsChanged              func(_ *zap.Logger, _ *api.VmInfo, podName string)
	submitNonAutoscalingVmUsageChanged func(_ *zap.Logger, _ *api.VmInfo, podName string)
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
	parentLogger *zap.Logger,
	metrics watch.Metrics,
	callbacks vmWatchCallbacks,
) (*watch.Store[vmapi.VirtualMachine], error) {
	logger := parentLogger.Named("vm-watch")

	return watch.Watch(
		ctx,
		logger.Named("watch"),
		e.vmClient.NeonvmV1().VirtualMachines(corev1.NamespaceAll),
		watch.Config{
			ObjectNameLogField: "virtualmachine",
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
				oldInfo, err := api.ExtractVmInfo(logger, oldVM)
				if err != nil {
					logger.Error("Failed to extract VM info in update for old VM", util.VMNameFields(oldVM), zap.Error(err))
					return
				}
				newInfo, err := api.ExtractVmInfo(logger, newVM)
				if err != nil {
					logger.Error("Failed to extract VM info in update for new VM", util.VMNameFields(newVM), zap.Error(err))
					return
				}

				if newVM.Status.PodName == "" {
					logger.Info("Skipping update for VM because .status.podName is empty", util.VMNameFields(newVM))
				} else if oldInfo.ScalingEnabled && !newInfo.ScalingEnabled {
					logger.Info("Received update to disable autoscaling for VM", util.VMNameFields(newVM))
					name := util.NamespacedName{Namespace: newInfo.Namespace, Name: newVM.Status.PodName}
					callbacks.submitVMDisabledScaling(logger, name)
				} else if (!oldInfo.ScalingEnabled || !newInfo.ScalingEnabled) && oldInfo.Using() != newInfo.Using() {
					podName := util.NamespacedName{Namespace: newInfo.Namespace, Name: newVM.Status.PodName}
					logger.Info("Received update changing usage for VM", zap.Object("old", oldInfo.Using()), zap.Object("new", newInfo.Using()))
					callbacks.submitNonAutoscalingVmUsageChanged(logger, newInfo, podName.Name)
				} else if oldVM.Status.PodName != newVM.Status.PodName {
					// If the pod changed, then we're going to handle a deletion event for the old pod,
					// plus creation event for the new pod. Don't worry about it - because all VM
					// information comes from this watch.Store anyways, there's no possibility of missing
					// an update.
				} else if oldInfo.EqualScalingBounds(*newInfo) {
					// If bounds didn't change, then no need to update
				} else {
					callbacks.submitVMBoundsChanged(logger, newInfo, newVM.Status.PodName)
				}
			},
		},
	)
}
