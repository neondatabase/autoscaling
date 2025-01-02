/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/client-go/tools/record"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/neonvm/controllers/buildtag"
)

const virtualmachinemigrationFinalizer = "vm.neon.tech/finalizer"

// Definitions to manage status conditions
const (
	// typeAvailableVirtualMachineMigration represents the status of the Deployment reconciliation
	typeAvailableVirtualMachineMigration = "Available"
	// typeDegradedVirtualMachineMigration represents the status used when the custom resource is deleted and the finalizer operations are must to occur.
	typeDegradedVirtualMachineMigration = "Degraded"
)

// VirtualMachineMigrationReconciler reconciles a VirtualMachineMigration object
type VirtualMachineMigrationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Config   *ReconcilerConfig

	Metrics ReconcilerMetrics
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when controller-gen (used by 'make generate') is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachinemigrations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachinemigrations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachinemigrations/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.

// It is essential for the controller's reconciliation loop to be idempotent. By following the Operator
// pattern you will create Controllers which provide a reconcile function
// responsible for synchronizing resources until the desired state is reached on the cluster.
// Breaking this recommendation goes against the design principles of controller-runtime.
// and may lead to unforeseen consequences such as resources becoming stuck and requiring manual intervention.
// For further info:
// - About Operator Pattern: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
// - About Controllers: https://kubernetes.io/docs/concepts/architecture/controller/
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *VirtualMachineMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the VirtualMachineMigration instance
	// The purpose is check if the Custom Resource for the Kind VirtualMachineMigration
	// is applied on the cluster if not we return nil to stop the reconciliation
	migration := new(vmv1.VirtualMachineMigration)
	if err := r.Get(ctx, req.NamespacedName, migration); err != nil {
		// ignore error and stop reconcile loop if object not found (already deleted?)
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "Unable to fetch Migration")
		return ctrl.Result{}, err
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if migration.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(migration, virtualmachinemigrationFinalizer) {
			log.Info("Adding Finalizer to Migration")
			if !controllerutil.AddFinalizer(migration, virtualmachinemigrationFinalizer) {
				return ctrl.Result{}, errors.New("Failed to add finalizer to Migration")
			}
			if err := r.Update(ctx, migration); err != nil {
				return ctrl.Result{}, err
			}
			// stop this reconciliation cycle, new will be triggered as Migration updated
			return ctrl.Result{}, nil
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(migration, virtualmachinemigrationFinalizer) {
			// our finalizer is present, so lets handle any external dependency
			log.Info("Performing Finalizer Operations for Migration")
			vm := new(vmv1.VirtualMachine)
			err := r.Get(ctx, types.NamespacedName{Name: migration.Spec.VmName, Namespace: migration.Namespace}, vm)
			if err != nil {
				log.Error(err, "Failed to get VM", "VmName", migration.Spec.VmName)
			}
			if err := r.doFinalizerOperationsForVirtualMachineMigration(ctx, migration, vm); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return ctrl.Result{}, err
			}
			// remove our finalizer from the list and update it.
			log.Info("Removing Finalizer from Migration")
			if !controllerutil.RemoveFinalizer(migration, virtualmachinemigrationFinalizer) {
				return ctrl.Result{}, errors.New("Failed to remove finalizer from Migration")
			}
			if err := r.Update(ctx, migration); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	// Fetch the corresponding VirtualMachine instance
	vm := new(vmv1.VirtualMachine)
	err := r.Get(ctx, types.NamespacedName{Name: migration.Spec.VmName, Namespace: migration.Namespace}, vm)
	if err != nil {
		log.Error(err, "Failed to get VM", "VmName", migration.Spec.VmName)
		if apierrors.IsNotFound(err) {
			// stop reconcile loop if vm not found (already deleted?)
			message := fmt.Sprintf("VM (%s) not found", migration.Spec.VmName)
			r.Recorder.Event(migration, "Warning", "Failed", message)
			meta.SetStatusCondition(&migration.Status.Conditions,
				metav1.Condition{
					Type:    typeDegradedVirtualMachineMigration,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: message,
				})
			migration.Status.Phase = vmv1.VmmFailed
			return r.updateMigrationStatus(ctx, migration)
		}
		// return err and try reconcile again
		return ctrl.Result{}, err
	}

	// Set owner for VM migration object
	if !metav1.IsControlledBy(migration, vm) {
		log.Info("Set VM as owner for Migration", "vm.Name", vm.Name)
		if err := ctrl.SetControllerReference(vm, migration, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Update(ctx, migration); err != nil {
			log.Info("Failed to add owner to Migration", "error", err)
			return ctrl.Result{}, err
		}
		// stop this reconciliation cycle, new will be triggered as Migration updated
		return ctrl.Result{}, nil
	}

	// MAIN RECONCILE LOOP START

	// Let's check and just set the condition status as Unknown when no status are available
	if len(migration.Status.Conditions) == 0 {
		log.Info("Set initial Unknown condition status")
		meta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{Type: typeAvailableVirtualMachineMigration, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
		return r.updateMigrationStatus(ctx, migration)
	}

	// target runner pod details - generate name
	if len(migration.Status.TargetPodName) == 0 {
		targetPodName := names.SimpleNameGenerator.GenerateName(fmt.Sprintf("%s-", vm.Name))
		log.Info("Set Target Pod Name", "TargetPod.Name", targetPodName)
		migration.Status.TargetPodName = targetPodName
		return r.updateMigrationStatus(ctx, migration)
	}

	switch migration.Status.Phase {

	case "":
		// need change VM status asap to prevent autoscler change CPU/RAM in VM
		// but only if VM running
		if vm.Status.Phase == vmv1.VmRunning {
			vm.Status.Phase = vmv1.VmPreMigrating
			if err := r.Status().Update(ctx, vm); err != nil {
				log.Error(err, "Failed to update VM status to PreMigrating", "Status", vm.Status.Phase)
				return ctrl.Result{}, err
			}
			// Migration just created, change Phase to "Pending"
			migration.Status.Phase = vmv1.VmmPending
			return r.updateMigrationStatus(ctx, migration)
		}
		// some other VM status (Scaling may be), requeue after second
		return ctrl.Result{RequeueAfter: time.Second}, nil

	case vmv1.VmmPending:
		// Check if the target runner pod already exists,
		// if not create a new one using source pod as template
		targetRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: migration.Status.TargetPodName, Namespace: vm.Namespace}, targetRunner)
		if err != nil && apierrors.IsNotFound(err) {
			// NB: .Spec.EnableSSH guaranteed non-nil because the k8s API server sets the default for us.
			enableSSH := *vm.Spec.EnableSSH
			var sshSecret *corev1.Secret
			if enableSSH {
				// We require the SSH secret to exist because we cannot unmount and
				// mount the new secret into the VM after the live migration. If a
				// VM's SSH secret is deleted accidentally then live migration is
				// not possible.
				if len(vm.Status.SSHSecretName) == 0 {
					err := errors.New("VM has .Spec.EnableSSH but its .Status.SSHSecretName is empty")
					log.Error(err, "Failed to get VM's SSH Secret")
					r.Recorder.Event(migration, "Warning", "Failed", err.Error())
					return ctrl.Result{}, err
				}
				sshSecret = &corev1.Secret{}
				err := r.Get(ctx, types.NamespacedName{Name: vm.Status.SSHSecretName, Namespace: vm.Namespace}, sshSecret)
				if err != nil {
					log.Error(err, "Failed to get VM's SSH Secret")
					r.Recorder.Event(migration, "Warning", "Failed", fmt.Sprintf("Failed to get VM's SSH Secret: %v", err))
					return ctrl.Result{}, err
				}
			}

			// Define a new target pod
			tpod, err := r.targetPodForVirtualMachine(vm, migration, sshSecret)
			if err != nil {
				log.Error(err, "Failed to generate Target Pod spec")
				return ctrl.Result{}, err
			}
			log.Info("Creating a Target Pod", "Pod.Namespace", tpod.Namespace, "Pod.Name", tpod.Name)
			if err = r.Create(ctx, tpod); err != nil {
				log.Error(err, "Failed to create Target Pod", "Pod.Namespace", tpod.Namespace, "Pod.Name", tpod.Name)
				return ctrl.Result{}, err
			}
			log.Info("Target runner Pod was created", "Pod.Namespace", tpod.Namespace, "Pod.Name", tpod.Name)
			// add event with some info
			r.Recorder.Event(migration, "Normal", "Created",
				fmt.Sprintf("VM (%s) ready migrate to target pod (%s)",
					vm.Name, tpod.Name))
			// target pod was just created, so requeue reconcile
			return ctrl.Result{RequeueAfter: time.Second}, nil
		} else if err != nil {
			log.Error(err, "Failed to get Target Pod")
			return ctrl.Result{}, err
		}

		// Update the metadata (including "usage" annotation) before anything else, so that it
		// will be correctly set even if the rest of the reconcile operation fails.
		if err := updatePodMetadataIfNecessary(ctx, r.Client, vm, targetRunner); err != nil {
			log.Error(err, "Failed to sync pod labels and annotations", "TargetPod.Name", targetRunner.Name)
		}

		// If not already, set an additional (non-controller) owner reference for the source pod:
		sourceRunner := &corev1.Pod{}
		err = r.Get(ctx, types.NamespacedName{Name: vm.Status.PodName, Namespace: vm.Namespace}, sourceRunner)
		if err != nil {
			log.Error(err, "Failed to get migration source pod")
			return ctrl.Result{}, err
		}
		ownedByMigration := false
		for _, ref := range sourceRunner.OwnerReferences {
			if ref.UID == migration.UID {
				ownedByMigration = true
				break
			}
		}
		if !ownedByMigration {
			if err = controllerutil.SetOwnerReference(migration, sourceRunner, r.Scheme); err != nil {
				log.Error(err, "Failed to set owner reference for source pod")
				return ctrl.Result{}, err
			}
			if err = r.Update(ctx, sourceRunner); err != nil {
				log.Error(err, "Failed to update owner of source runner")
				// Requeue so that we try again, even though we're not an owner of the source runner
				return ctrl.Result{RequeueAfter: time.Second}, err
			}
		}

		// now inspect target pod status and update migration
		switch runnerStatus(targetRunner) {
		case runnerRunning:
			// update migration status
			migration.Status.SourcePodName = vm.Status.PodName
			migration.Status.SourcePodIP = vm.Status.PodIP
			migration.Status.TargetPodIP = targetRunner.Status.PodIP

			// do hotplugCPU in targetRunner before migration
			log.Info("Syncing CPUs in Target runner", "TargetPod.Name", migration.Status.TargetPodName)
			if err := QmpSyncCpuToTarget(vm, migration); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("CPUs in Target runner synced", "TargetPod.Name", migration.Status.TargetPodName)

			// do hotplug Memory in targetRunner -- only needed for dimm slots; virtio-mem Just Works™
			switch *vm.Status.MemoryProvider {
			case vmv1.MemoryProviderVirtioMem:
				// ref "Migration works out of the box" - https://lwn.net/Articles/755423/
				log.Info(
					"No need to sync memory in Target runner because MemoryProvider is VirtioMem",
					"TargetPod.Name", migration.Status.TargetPodName,
				)
			case vmv1.MemoryProviderDIMMSlots:
				log.Info("Syncing Memory in Target runner", "TargetPod.Name", migration.Status.TargetPodName)
				if err := QmpSyncMemoryToTarget(vm, migration); err != nil {
					return ctrl.Result{}, err
				}
				log.Info("Memory in Target runner synced", "TargetPod.Name", migration.Status.TargetPodName)
			default:
				panic(fmt.Errorf("unexpected vm.status.memoryProvider %q", *vm.Status.MemoryProvider))
			}

			// Migrate only running VMs to target with plugged devices
			if vm.Status.Phase == vmv1.VmPreMigrating {
				// update VM status
				vm.Status.Phase = vmv1.VmMigrating
				if err := r.Status().Update(ctx, vm); err != nil {
					log.Error(err, "Failed to update VirtualMachine status to 'Migrating'")
					return ctrl.Result{}, err
				}
				// trigger migration
				if err := QmpStartMigration(vm, migration); err != nil {
					migration.Status.Phase = vmv1.VmmFailed
					return ctrl.Result{}, err
				}
				message := fmt.Sprintf("Migration was started to target runner (%s)", targetRunner.Name)
				log.Info(message)
				r.Recorder.Event(migration, "Normal", "Started", message)
				meta.SetStatusCondition(&migration.Status.Conditions,
					metav1.Condition{
						Type:    typeAvailableVirtualMachineMigration,
						Status:  metav1.ConditionTrue,
						Reason:  "Reconciling",
						Message: message,
					})
				// finally update migration phase to Running
				migration.Status.Phase = vmv1.VmmRunning
				return r.updateMigrationStatus(ctx, migration)
			}
		case runnerSucceeded:
			// target runner pod finished without error? but it shouldn't finish
			message := fmt.Sprintf("Target Pod (%s) completed suddenly", targetRunner.Name)
			log.Info(message)
			r.Recorder.Event(migration, "Warning", "Failed", message)
			meta.SetStatusCondition(&migration.Status.Conditions,
				metav1.Condition{
					Type:    typeDegradedVirtualMachineMigration,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: message,
				})
			migration.Status.Phase = vmv1.VmmFailed
			return r.updateMigrationStatus(ctx, migration)
		case runnerFailed:
			message := fmt.Sprintf("Target Pod (%s) failed", targetRunner.Name)
			log.Info(message)
			r.Recorder.Event(migration, "Warning", "Failed", message)
			meta.SetStatusCondition(&migration.Status.Conditions,
				metav1.Condition{
					Type:    typeDegradedVirtualMachineMigration,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: message,
				})
			migration.Status.Phase = vmv1.VmmFailed
			return r.updateMigrationStatus(ctx, migration)
		default:
			// not sure what to do, so try rqueue
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}

	case vmv1.VmmRunning:
		// retrieve target pod details
		targetRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: migration.Status.TargetPodName, Namespace: migration.Namespace}, targetRunner)
		if err != nil && apierrors.IsNotFound(err) {
			// lost target pod for running Migration ?
			message := fmt.Sprintf("Target Pod (%s) disappeared", migration.Status.TargetPodName)
			r.Recorder.Event(migration, "Error", "NotFound", message)
			meta.SetStatusCondition(&migration.Status.Conditions,
				metav1.Condition{
					Type:    typeDegradedVirtualMachineMigration,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: message,
				})
			migration.Status.Phase = vmv1.VmmFailed
			return r.updateMigrationStatus(ctx, migration)
		} else if err != nil {
			log.Error(err, "Failed to get target runner Pod")
			return ctrl.Result{}, err
		}

		// Update the metadata (including "usage" annotation) before anything else, so that it
		// will be correctly set even if the rest of the reconcile operation fails.
		if err := updatePodMetadataIfNecessary(ctx, r.Client, vm, targetRunner); err != nil {
			log.Error(err, "Failed to sync pod labels and annotations", "TargetPod.Name", targetRunner.Name)
		}

		// retrieve migration statistics
		migrationInfo, err := QmpGetMigrationInfo(QmpAddr(vm))
		if err != nil {
			log.Error(err, "Failed to get migration info")
			return ctrl.Result{}, err
		}

		// check if migration done
		if migrationInfo.Status == "completed" {
			message := fmt.Sprintf("Migration finished with success to target pod (%s)",
				targetRunner.Name)
			log.Info(message)
			r.Recorder.Event(migration, "Normal", "Finished", message)

			// re-fetch the vm
			err := r.Get(ctx, types.NamespacedName{Name: migration.Spec.VmName, Namespace: migration.Namespace}, vm)
			if err != nil {
				log.Error(err, "Failed to re-fetch VM", "VmName", migration.Spec.VmName)
				return ctrl.Result{}, err
			}
			// Redefine runner Pod for VM
			vm.Status.PodName = migration.Status.TargetPodName
			vm.Status.PodIP = migration.Status.TargetPodIP
			vm.Status.Phase = vmv1.VmRunning
			// update VM status
			if err := r.Status().Update(ctx, vm); err != nil {
				log.Error(err, "Failed to redefine runner pod in VM")
				return ctrl.Result{}, err
			}

			// Redefine ownerRef for the target Pod
			targetRunner.OwnerReferences = []metav1.OwnerReference{}
			if err := ctrl.SetControllerReference(vm, targetRunner, r.Scheme); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.Update(ctx, targetRunner); err != nil {
				log.Error(err, "Failed to update ownerRef for target runner pod")
				return ctrl.Result{}, err
			}

			// Redefine ownerRef for the source Pod
			sourceRunner := &corev1.Pod{}
			err = r.Get(ctx, types.NamespacedName{Name: migration.Status.SourcePodName, Namespace: migration.Namespace}, sourceRunner)
			if err == nil {
				sourceRunner.OwnerReferences = []metav1.OwnerReference{}
				if err := ctrl.SetControllerReference(migration, sourceRunner, r.Scheme); err != nil {
					return ctrl.Result{}, err
				}
				if err := r.Update(ctx, sourceRunner); err != nil {
					log.Error(err, "Failed to update ownerRef for source runner pod")
					return ctrl.Result{}, err
				}
			} else if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			// try to stop hypervisor in source runner if it running still
			if sourceRunner.Status.Phase == corev1.PodRunning {
				if err := QmpQuit(migration.Status.SourcePodIP, vm.Spec.QMP); err != nil {
					log.Error(err, "Failed stop hypervisor in source runner pod")
				} else {
					log.Info("Hypervisor in source runner pod stopped")
				}
			} else {
				log.Info("Skip stopping hypervisor in source runner pod", "pod.Status.Phase", sourceRunner.Status.Phase)
			}

			// finally update migration phase to Succeeded
			migration.Status.Phase = vmv1.VmmSucceeded
			migration.Status.Info.Status = migrationInfo.Status
			return r.updateMigrationStatus(ctx, migration)
		}

		// check if migration failed
		if migrationInfo.Status == "failed" {
			// oops, migration failed
			message := fmt.Sprintf("Migration to target pod (%s) was failed",
				targetRunner.Name)
			log.Info(message)
			r.Recorder.Event(migration, "Warning", "Failed", message)

			// try to stop hypervisor in target runner
			if targetRunner.Status.Phase == corev1.PodRunning {
				if err := QmpQuit(migration.Status.TargetPodIP, vm.Spec.QMP); err != nil {
					log.Error(err, "Failed stop hypervisor in target runner pod")
				} else {
					log.Info("Hypervisor in target runner pod stopped")
				}
			} else {
				log.Info("Skip stopping hypervisor in target runner pod", "pod.Status.Phase", targetRunner.Status.Phase)
			}
			// change VM status to Running
			vm.Status.Phase = vmv1.VmRunning
			if err := r.Status().Update(ctx, vm); err != nil {
				log.Error(err, "Failed to update VM status from Migrating back to Running as Migration was failed")
				return ctrl.Result{}, err
			}
			// finally update migration phase to Failed
			migration.Status.Phase = vmv1.VmmFailed
			migration.Status.Info.Status = migrationInfo.Status
			return r.updateMigrationStatus(ctx, migration)
		}
		// seems migration still going on, just update status with migration progress once per second
		time.Sleep(time.Second)
		// re-retrieve migration statistics
		migrationInfo, err = QmpGetMigrationInfo(QmpAddr(vm))
		if err != nil {
			log.Error(err, "Failed to re-get migration info")
			return ctrl.Result{}, err
		}
		// re-fetch the vm
		err = r.Get(ctx, types.NamespacedName{Name: migration.Spec.VmName, Namespace: migration.Namespace}, vm)
		if err != nil {
			log.Error(err, "Failed to re-fetch VM before Mgration progress update", "VmName", migration.Spec.VmName)
			return ctrl.Result{}, err
		}
		migration.Status.Info.Status = migrationInfo.Status
		migration.Status.Info.TotalTimeMs = migrationInfo.TotalTimeMs
		migration.Status.Info.SetupTimeMs = migrationInfo.SetupTimeMs
		migration.Status.Info.DowntimeMs = migrationInfo.DowntimeMs
		migration.Status.Info.Ram.Transferred = migrationInfo.Ram.Transferred
		migration.Status.Info.Ram.Remaining = migrationInfo.Ram.Remaining
		migration.Status.Info.Ram.Total = migrationInfo.Ram.Total
		migration.Status.Info.Compression.CompressedSize = migrationInfo.Compression.CompressedSize
		migration.Status.Info.Compression.CompressionRate = int64(math.Round(migrationInfo.Compression.CompressionRate))
		return r.updateMigrationStatus(ctx, migration)

	case vmv1.VmmSucceeded:
		// do additional VM status checks
		if vm.Status.Phase == vmv1.VmMigrating {
			// migration Succeeded and VM should have status Running
			vm.Status.Phase = vmv1.VmRunning
			// update VM status
			if err := r.Status().Update(ctx, vm); err != nil {
				log.Error(err, "Failed to update VM status from Migrating to Running as Migration succeeded")
				return ctrl.Result{}, err
			}
		}

		if len(migration.Status.SourcePodName) > 0 {
			// try to find and remove source runner Pod
			sourceRunner := &corev1.Pod{}
			err := r.Get(ctx, types.NamespacedName{Name: migration.Status.SourcePodName, Namespace: migration.Namespace}, sourceRunner)
			if err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to get source runner Pod for deletion")
				return ctrl.Result{}, err
			}
			var msg, eventReason string
			if buildtag.NeverDeleteRunnerPods {
				msg = fmt.Sprintf("Source runner pod deletion was skipped due to '%s' build tag", buildtag.TagnameNeverDeleteRunnerPods)
				eventReason = "DeleteSkipped"
			} else {
				if err := r.Delete(ctx, sourceRunner); err != nil {
					log.Error(err, "Failed to delete source runner Pod")
					return ctrl.Result{}, err
				}
				msg = "Source runner was deleted"
				eventReason = "Deleted"
			}
			log.Info(msg, "Pod.Namespace", sourceRunner.Namespace, "Pod.Name", sourceRunner.Name)
			r.Recorder.Event(migration, "Normal", eventReason, fmt.Sprintf("%s: %s", msg, sourceRunner.Name))
			migration.Status.SourcePodName = ""
			migration.Status.SourcePodIP = ""
			return r.updateMigrationStatus(ctx, migration)
		}
		// all done, stop reconciliation
		return ctrl.Result{}, nil

	case vmv1.VmmFailed:
		// do additional VM status checks
		if vm.Status.Phase == vmv1.VmMigrating {
			// migration Failed and VM should back to Running state
			vm.Status.Phase = vmv1.VmRunning
			if err := r.Status().Update(ctx, vm); err != nil {
				log.Error(err, "Failed to update VM status from Migrating back to Running as Migration was failed")
				return ctrl.Result{}, err
			}
		}
		// all done, stop reconciliation
		return ctrl.Result{}, nil

	default:
		// not sure what to do, so try rqueue
		log.Info("Requeuing current request")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// MAIN RECONCILE LOOP END
	return ctrl.Result{}, nil
}

func (r *VirtualMachineMigrationReconciler) updateMigrationStatus(ctx context.Context, migration *vmv1.VirtualMachineMigration) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	if err := r.Status().Update(ctx, migration); err != nil {
		log.Error(err, "Failed update Migration status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// finalizeVirtualMachineMigration will perform the required operations before delete the CR.
func (r *VirtualMachineMigrationReconciler) doFinalizerOperationsForVirtualMachineMigration(ctx context.Context, migration *vmv1.VirtualMachineMigration, vm *vmv1.VirtualMachine) error {
	log := log.FromContext(ctx)

	if migration.Status.Phase == vmv1.VmmRunning || vm.Status.Phase == vmv1.VmPreMigrating {
		message := fmt.Sprintf("Running Migration (%s) is being deleted", migration.Name)
		log.Info(message)
		r.Recorder.Event(migration, "Warning", "Deleting", message)

		// try to cancel migration
		log.Info("Canceling migration")
		if err := QmpCancelMigration(QmpAddr(vm)); err != nil {
			// inform about error but not return error to avoid stuckness in reconciliation cycle
			log.Error(err, "Migration canceling failed")
		}

		if vm.Status.Phase == vmv1.VmMigrating || vm.Status.Phase == vmv1.VmPreMigrating {
			// migration being deleted and VM should have status Running
			vm.Status.Phase = vmv1.VmRunning
			// update VM status
			if err := r.Status().Update(ctx, vm); err != nil {
				log.Error(err, "Failed to update VM status from Migrating to Running on Migration deletion")
				return err
			}
		}

		// try to remove target runner pod
		if len(migration.Status.TargetPodName) > 0 {
			pod := &corev1.Pod{}
			err := r.Get(ctx, types.NamespacedName{Name: migration.Status.TargetPodName, Namespace: migration.Namespace}, pod)
			if err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to get target runner Pod for deletion")
				return err
			}
			if apierrors.IsNotFound(err) {
				// pod already deleted ?
				return nil
			}
			// NB: here, we ignore buildtag.NeverDeleteRunnerPods because we delete runner pods on
			// VM object deletion with the tag anyways, so it's more consistent to keep the same
			// behavior for VMMs.
			if err := r.Delete(ctx, pod); err != nil {
				log.Error(err, "Failed to delete target runner Pod")
				return err
			}
			message := fmt.Sprintf("Target runner (%s) was deleted", pod.Name)
			log.Info(message)
			r.Recorder.Event(migration, "Normal", "Deleted", message)
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
// Note that the Pods will be also watched in order to ensure its
// desirable state on the cluster
func (r *VirtualMachineMigrationReconciler) SetupWithManager(mgr ctrl.Manager) (ReconcilerWithMetrics, error) {
	cntrlName := "virtualmachinemigration"
	reconciler := WithMetrics(
		withCatchPanic(r),
		r.Metrics,
		cntrlName,
		r.Config.FailurePendingPeriod,
		r.Config.FailingRefreshInterval,
	)
	err := ctrl.NewControllerManagedBy(mgr).
		For(&vmv1.VirtualMachineMigration{}).
		Owns(&corev1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.Config.MaxConcurrentReconciles}).
		Named(cntrlName).
		Complete(reconciler)
	return reconciler, err
}

// targetPodForVirtualMachine returns a VirtualMachine Pod object
func (r *VirtualMachineMigrationReconciler) targetPodForVirtualMachine(
	vm *vmv1.VirtualMachine,
	migration *vmv1.VirtualMachineMigration,
	sshSecret *corev1.Secret,
) (*corev1.Pod, error) {
	if vm.Status.MemoryProvider == nil {
		return nil, errors.New("cannot create target pod because vm.status.memoryProvider is not set")
	}
	// TODO: this is technically racy because target pod creation happens before we set the
	// migration source pod, so in between reading this and starting the migration, it's
	// *technically* possible that we create a target pod with a different memory provider than a
	// newer source pod.
	// Given that this requires (a) restart *during* initial live migration, and (b) that restart to
	// change the memory provider, this is low enough risk that it's ok to leave to a follow-up.
	memoryProvider := *vm.Status.MemoryProvider

	pod, err := podSpec(vm, memoryProvider, sshSecret, r.Config, false)
	if err != nil {
		return nil, err
	}

	// override pod name
	pod.Name = migration.Status.TargetPodName

	// add env variable to turn on migration receiver
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{Name: "RECEIVE_MIGRATION", Value: "true"})

	// add podAntiAffinity to schedule target pod to another k8s node
	if migration.Spec.PreventMigrationToSameHost {
		if pod.Spec.Affinity == nil {
			pod.Spec.Affinity = &corev1.Affinity{}
		}
		if pod.Spec.Affinity.PodAntiAffinity == nil {
			pod.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{}
		}
		if pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
			pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = []corev1.PodAffinityTerm{}
		}
		pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					vmv1.VirtualMachineNameLabel: migration.Spec.VmName,
				},
			},
			TopologyKey: "kubernetes.io/hostname",
		})
	}

	// Set the ownerRef for the Pod
	if err := ctrl.SetControllerReference(migration, pod, r.Scheme); err != nil {
		return nil, err
	}

	return pod, nil
}
