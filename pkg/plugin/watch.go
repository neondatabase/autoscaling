package plugin

// Implementation of watching for Pod deletions and changes to a VM's scaling settings (either
// whether it's disabled, or the scaling bounds themselves).

import (
	"context"
	"time"

	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

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
			RelistRateLimit:  nil,
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
	submitPodStarted        func(*zap.Logger, *corev1.Pod)
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
) (*watch.Store[corev1.Pod], error) {
	logger := parentLogger.Named("pod-watch")

	return watch.Watch(
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
			RelistRateLimit:  nil,
		},
		watch.Accessors[*corev1.PodList, corev1.Pod]{
			Items: func(list *corev1.PodList) []corev1.Pod { return list.Items },
		},
		watch.InitModeSync, // note: doesn't matter, because AddFunc = nil.
		metav1.ListOptions{},
		watch.HandlerFuncs[*corev1.Pod]{
			AddFunc: func(pod *corev1.Pod, preexisting bool) {
				name := util.GetNamespacedName(pod)

				if e.state.conf.ignoredNamespace(pod.Namespace) {
					logger.Info("Received add event for ignored pod", zap.Object("pod", name))
					return
				}

				isVM := util.TryPodOwnerVirtualMachine(pod) != nil ||
					util.TryPodOwnerVirtualMachineMigration(pod) != nil

				// Generate events for all non-VM pods that are running
				if !isVM && pod.Status.Phase == corev1.PodRunning {
					if !preexisting {
						// Generally pods shouldn't be immediately running, so we log this as a
						// warning. If it was preexisting, then it'll be handled on the initial
						// cluster read already (but we generate the events anyways so that we
						// definitely don't miss anything).
						logger.Warn("Received add event for new non-VM pod already running", zap.Object("pod", name))
					}
					callbacks.submitPodStarted(logger, pod)
				}
			},
			UpdateFunc: func(oldPod *corev1.Pod, newPod *corev1.Pod) {
				name := util.GetNamespacedName(newPod)

				if e.state.conf.ignoredNamespace(newPod.Namespace) {
					logger.Info("Received update event for ignored pod", zap.Object("pod", name))
					return
				}

				isVM := util.TryPodOwnerVirtualMachine(newPod) != nil ||
					util.TryPodOwnerVirtualMachineMigration(newPod) != nil

				// Check if a non-VM pod is now running.
				if !isVM && oldPod.Status.Phase == corev1.PodPending && newPod.Status.Phase == corev1.PodRunning {
					logger.Info("Received update event for non-VM pod now running", zap.Object("pod", name))
					callbacks.submitPodStarted(logger, newPod)
				}

				// Check if pod is "completed" - handle that the same as deletion.
				if !util.PodCompleted(oldPod) && util.PodCompleted(newPod) {
					logger.Info("Received update event for completion of pod", zap.Object("pod", name))

					if isVM {
						callbacks.submitVMDeletion(logger, name)
					} else {
						callbacks.submitPodDeletion(logger, name)
					}
					return // no other handling worthwhile if the pod's done.
				}

				// Check if the pod is part of a new migration, or if a migration it *was* part of
				// has now ended.
				oldMigration := util.TryPodOwnerVirtualMachineMigration(oldPod)
				newMigration := util.TryPodOwnerVirtualMachineMigration(newPod)

				if oldMigration == nil && newMigration != nil {
					isSource := util.TryPodOwnerVirtualMachine(newPod) == nil
					callbacks.submitPodStartMigration(logger, name, *newMigration, isSource)
				} else if oldMigration != nil && newMigration == nil {
					callbacks.submitPodEndMigration(logger, name, *oldMigration)
				}
			},
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				name := util.GetNamespacedName(pod)

				if e.state.conf.ignoredNamespace(pod.Namespace) {
					logger.Info("Received delete event for ignored pod", zap.Object("pod", name))
					return
				}

				if util.PodCompleted(pod) {
					logger.Info("Received delete event for completed pod", zap.Object("pod", name))
				} else {
					logger.Info("Received delete event for pod", zap.Object("pod", name))
					isVM := util.TryPodOwnerVirtualMachine(pod) != nil ||
						util.TryPodOwnerVirtualMachineMigration(pod) != nil
					if isVM {
						callbacks.submitVMDeletion(logger, name)
					} else {
						callbacks.submitPodDeletion(logger, name)
					}
				}
			},
		},
	)
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
	podIndex watch.IndexedStore[corev1.Pod, *watch.NameIndex[corev1.Pod]],
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
			RelistRateLimit:  util.NewTimeRange(time.Second, 1, 2),
		},
		watch.Accessors[*vmapi.VirtualMachineList, vmapi.VirtualMachine]{
			Items: func(list *vmapi.VirtualMachineList) []vmapi.VirtualMachine { return list.Items },
		},
		watch.InitModeSync, // Must sync here so that initial cluster state is read correctly.
		metav1.ListOptions{},
		watch.HandlerFuncs[*vmapi.VirtualMachine]{
			UpdateFunc: func(oldVM, newVM *vmapi.VirtualMachine) {
				if e.state.conf.ignoredNamespace(newVM.Namespace) {
					logger.Info("Received update event for ignored VM", util.VMNameFields(newVM))
					return
				}

				newInfo, err := api.ExtractVmInfo(logger, newVM)
				if err != nil {
					// Try to get the runner pod associated with the VM, if we can, but don't worry
					// about it if we can't.
					var runnerPod runtime.Object
					if podName := newVM.Status.PodName; podName != "" {
						// NB: index.Get returns nil if not found, so we only have a non-nil
						// runnerPod if it's currently known.
						rp, _ := podIndex.GetIndexed(func(index *watch.NameIndex[corev1.Pod]) (*corev1.Pod, bool) {
							return index.Get(newVM.Namespace, podName)
						})
						// avoid typed nils by only assigning if non-nil
						// See <https://github.com/neondatabase/autoscaling/issues/689> for more.
						if rp != nil {
							runnerPod = rp
						}
					}

					logger.Error("Failed to extract VM info in update for new VM", util.VMNameFields(newVM), zap.Error(err))
					e.handle.EventRecorder().Eventf(
						newVM,            // regarding
						runnerPod,        // related
						"Warning",        // eventtype
						"ExtractVmInfo",  // reason
						"HandleVmUpdate", // action
						"Failed to extract autoscaling info about VM: %s", // note
						err,
					)
					return
				}
				oldInfo, err := api.ExtractVmInfo(logger, oldVM)
				if err != nil {
					logger.Error("Failed to extract VM info in update for old VM", util.VMNameFields(oldVM), zap.Error(err))
					return
				}

				if newVM.Status.PodName == "" {
					logger.Info("Skipping update for VM because .status.podName is empty", util.VMNameFields(newVM))
					return
				}

				if oldInfo.ScalingEnabled && !newInfo.ScalingEnabled {
					logger.Info("Received update to disable autoscaling for VM", util.VMNameFields(newVM))
					name := util.NamespacedName{Namespace: newInfo.Namespace, Name: newVM.Status.PodName}
					callbacks.submitVMDisabledScaling(logger, name)
				}

				if (!oldInfo.ScalingEnabled || !newInfo.ScalingEnabled) && oldInfo.Using() != newInfo.Using() {
					podName := util.NamespacedName{Namespace: newInfo.Namespace, Name: newVM.Status.PodName}
					logger.Info("Received update changing usage for VM", zap.Object("old", oldInfo.Using()), zap.Object("new", newInfo.Using()))
					callbacks.submitNonAutoscalingVmUsageChanged(logger, newInfo, podName.Name)
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

				callbacks.submitVMBoundsChanged(logger, newInfo, newVM.Status.PodName)
			},
		},
	)
}

type migrationWatchCallbacks struct {
	submitMigrationFinished func(*vmapi.VirtualMachineMigration)
}

// watchMigrationEvents *only* looks at migrations that were created by the scheduler plugin (or a
// previous version of it).
//
// We use this to trigger cleaning up migrations once they're finished, because they don't
// auto-delete, and our deterministic naming means that each we won't be able to create a new
// migration for the same VM until the old one's gone.
//
// Tracking whether a migration was created by the scheduler plugin is done by adding the label
// 'autoscaling.neon.tech/created-by-scheduler' to every migration we create.
func (e *AutoscaleEnforcer) watchMigrationEvents(
	ctx context.Context,
	parentLogger *zap.Logger,
	metrics watch.Metrics,
	callbacks migrationWatchCallbacks,
) (*watch.Store[vmapi.VirtualMachineMigration], error) {
	logger := parentLogger.Named("vmm-watch")

	return watch.Watch(
		ctx,
		logger.Named("watch"),
		e.vmClient.NeonvmV1().VirtualMachineMigrations(corev1.NamespaceAll),
		watch.Config{
			ObjectNameLogField: "virtualmachinemigration",
			Metrics: watch.MetricsConfig{
				Metrics:  metrics,
				Instance: "VirtualMachineMigrations",
			},
			// FIXME: make these durations configurable.
			RetryRelistAfter: util.NewTimeRange(time.Second, 3, 5),
			RetryWatchAfter:  util.NewTimeRange(time.Second, 3, 5),
			RelistRateLimit:  nil,
		},
		watch.Accessors[*vmapi.VirtualMachineMigrationList, vmapi.VirtualMachineMigration]{
			Items: func(list *vmapi.VirtualMachineMigrationList) []vmapi.VirtualMachineMigration { return list.Items },
		},
		watch.InitModeSync,
		metav1.ListOptions{
			// NB: Including just the label itself means that we select for objects that *have* the
			// label, without caring about the actual value.
			//
			// See also:
			// https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#set-based-requirement
			LabelSelector: LabelPluginCreatedMigration,
		},
		watch.HandlerFuncs[*vmapi.VirtualMachineMigration]{
			UpdateFunc: func(oldObj, newObj *vmapi.VirtualMachineMigration) {
				if e.state.conf.ignoredNamespace(newObj.Namespace) {
					logger.Info(
						"Received update event for ignored VM Migration",
						zap.Object("virtualmachinemigration", util.GetNamespacedName(newObj)),
					)
					return
				}

				shouldDelete := newObj.Status.Phase != oldObj.Status.Phase &&
					(newObj.Status.Phase == vmapi.VmmSucceeded || newObj.Status.Phase == vmapi.VmmFailed)

				if shouldDelete {
					callbacks.submitMigrationFinished(newObj)
				}
			},
		},
	)
}
