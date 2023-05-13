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
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nadapiv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
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
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
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
	var virtualmachinemigration vmv1.VirtualMachineMigration
	if err := r.Get(ctx, req.NamespacedName, &virtualmachinemigration); err != nil {
		// Error reading the object - requeue the request.
		if notfound := client.IgnoreNotFound(err); notfound == nil {
			log.Info("VM migration resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Unable to fetch VirtualMachineMigration")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if virtualmachinemigration.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(&virtualmachinemigration, virtualmachinemigrationFinalizer) {
			log.Info("Adding Finalizer for VM migration resource")
			if ok := controllerutil.AddFinalizer(&virtualmachinemigration, virtualmachinemigrationFinalizer); !ok {
				log.Info("Failed to add finalizer from VM migration resource")
				return ctrl.Result{Requeue: true}, nil
			}
			if err := r.Update(ctx, &virtualmachinemigration); err != nil {
				log.Error(err, "Failed to update status about adding finalizer to VM migration resource")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(&virtualmachinemigration, virtualmachinemigrationFinalizer) {
			// our finalizer is present, so lets handle any external dependency
			log.Info("Performing Finalizer Operations for VM migration resource before delete it")
			if err := r.doFinalizerOperationsForVirtualMachineMigration(ctx, &virtualmachinemigration); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			log.Info("Removing Finalizer for VM migration resource after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(&virtualmachinemigration, virtualmachinemigrationFinalizer); !ok {
				log.Info("Failed to remove finalizer from VM migration resource")
				return ctrl.Result{Requeue: true}, nil
			}
			if err := r.Update(ctx, &virtualmachinemigration); err != nil {
				log.Error(err, "Failed to update status about removing finalizer from VM migration resource")
				return ctrl.Result{}, err
			}
		}
		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	statusBefore := virtualmachinemigration.Status.DeepCopy()
	if err := r.doReconcile(ctx, &virtualmachinemigration); err != nil {
		r.Recorder.Eventf(&virtualmachinemigration, corev1.EventTypeWarning, "Failed",
			"Failed to reconcile VM migration (%s): %s", virtualmachinemigration.Name, err)
		return ctrl.Result{}, err
	}
	if !DeepEqual(virtualmachinemigration.Status, statusBefore) {
		// update VirtualMachineMigration status
		if err := r.Status().Update(ctx, &virtualmachinemigration); err != nil {
			if apierrors.IsConflict(err) {
				log.Info("Failed to update VM migration status after reconcile loop due conflict, will try again")
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			log.Error(err, "Failed to update VM migration status after reconcile loop")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: time.Second * 5}, nil
}

// finalizeVirtualMachineMigration will perform the required operations before delete the CR.
func (r *VirtualMachineMigrationReconciler) doFinalizerOperationsForVirtualMachineMigration(ctx context.Context, vmm *vmv1.VirtualMachineMigration) error {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as depended of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// TODO:
	// 1) send migration cancellation command
	// 2) wait migration canceled
	// 3) remove target pod (or manage it via ownerRef)

	// The following implementation will raise an event
	r.Recorder.Event(vmm, "Warning", "Deleting",
		fmt.Sprintf("VM migration '%s' is being deleted from the namespace %s",
			vmm.Name,
			vmm.Namespace))
	return nil
}

// SetupWithManager sets up the controller with the Manager.
// Note that the Pods will be also watched in order to ensure its
// desirable state on the cluster
func (r *VirtualMachineMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vmv1.VirtualMachineMigration{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

func (r *VirtualMachineMigrationReconciler) doReconcile(ctx context.Context, virtualmachinemigration *vmv1.VirtualMachineMigration) error {
	log := log.FromContext(ctx)

	// Let's check and just set the condition status as Unknown when no status are available
	if virtualmachinemigration.Status.Conditions == nil || len(virtualmachinemigration.Status.Conditions) == 0 {
		// set Unknown condition status for AvailableVirtualMachine
		meta.SetStatusCondition(&virtualmachinemigration.Status.Conditions, metav1.Condition{Type: typeAvailableVirtualMachineMigration, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
	}

	// VM details
	vm := &vmv1.VirtualMachine{}
	err := r.Get(ctx, types.NamespacedName{Name: virtualmachinemigration.Spec.VmName, Namespace: virtualmachinemigration.Namespace}, vm)
	if err != nil {
		log.Error(err, "Failed to get virtualmachine", "VmName", virtualmachinemigration.Spec.VmName)
		meta.SetStatusCondition(&virtualmachinemigration.Status.Conditions,
			metav1.Condition{Type: typeDegradedVirtualMachineMigration,
				Status:  metav1.ConditionTrue,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("VM (%s) not found", virtualmachinemigration.Spec.VmName)})
		virtualmachinemigration.Status.Phase = vmv1.VmmFailed
		return err
	}

	// target runner pod details - generate name
	if len(virtualmachinemigration.Status.TargetPodName) == 0 {
		virtualmachinemigration.Status.TargetPodName = names.SimpleNameGenerator.GenerateName(fmt.Sprintf("%s-", vm.Name))
		// return to Reconcile loop to save state
		return nil
	}

	switch virtualmachinemigration.Status.Phase {

	case "":
		// Set the ownerRef for the migration
		if err := ctrl.SetControllerReference(vm, virtualmachinemigration, r.Scheme); err != nil {
			return err
		}
		if err := r.Update(ctx, virtualmachinemigration); err != nil {
			log.Info("Failed to update VM migration to add Owner reference", "error", err)
			return nil
		}
		// need change VM status asap to prevent autoscler change CPU/RAM in VM
		// but only if VM in running mode
		if vm.Status.Phase == vmv1.VmRunning {
			vm.Status.Phase = vmv1.VmPreMigrating
			if err := r.Status().Update(ctx, vm); err != nil {
				log.Error(err, "Failed to update VirtualMachine status to 'PreMigrating'")
				return err
			}
			// VirtualMachineMigration just created, change Phase to "Pending"
			virtualmachinemigration.Status.Phase = vmv1.VmmPending
		}
	case vmv1.VmmPending:
		// Check if the target runner pod already exists, if not create a new one using source pod as template
		targetRunner := &corev1.Pod{}
		err = r.Get(ctx, types.NamespacedName{Name: virtualmachinemigration.Status.TargetPodName, Namespace: vm.Namespace}, targetRunner)
		if err != nil && apierrors.IsNotFound(err) {
			// Define a new target pod

			tpod, err := r.targetPodForVirtualMachine(vm, virtualmachinemigration)
			if err != nil {
				log.Error(err, "Failed to define Target Pod for VirtualMachineMigration")
				return err
			}

			log.Info("Creating a Target Pod", "Pod.Namespace", tpod.Namespace, "Pod.Name", tpod.Name)
			if err = r.Create(ctx, tpod); err != nil {
				log.Error(err, "Failed to create Target Pod", "Pod.Namespace", tpod.Namespace, "Pod.Name", tpod.Name)
				return err
			}

			r.Recorder.Event(virtualmachinemigration, "Normal", "Created",
				fmt.Sprintf("VirtualMachine %s ready migrate to target pod %s",
					vm.Name, tpod.Name))
		} else if err != nil {
			log.Error(err, "Failed to get Target Pod")
			return err
		}

		// now inspect target pod status and update migration
		switch targetRunner.Status.Phase {
		case corev1.PodRunning:
			// update migration status
			virtualmachinemigration.Status.SourcePodName = vm.Status.PodName
			virtualmachinemigration.Status.SourcePodIP = vm.Status.PodIP
			virtualmachinemigration.Status.TargetPodIP = targetRunner.Status.PodIP

			// Set the target runner's "usage" annotation before anything else, so that it will be
			// correct even if the rest of the reconcile operation fails
			if err := updateRunnerUsageAnnotation(ctx, r.Client, vm, targetRunner.Name); err != nil {
				log.Error(err, "Failed to set target Pod usage annotation", "VirtualMachineMigration", virtualmachinemigration)
				return err
			}

			readyToMigrateCPU := false
			// do hotplugCPU in targetRunner before migration if .spec.guest.cpus.use defined
			if vm.Spec.Guest.CPUs.Use != nil {
				// firstly get current state from QEMU
				cpusPlugged, _, err := QmpGetCpusFromRunner(virtualmachinemigration.Status.TargetPodIP, vm.Spec.QMP)
				if err != nil {
					log.Error(err, "Failed to get CPU details from Target Runner", "Pod", virtualmachinemigration.Status.TargetPodName)
					return err
				}
				// compare guest spec and count of plugged
				if vm.Spec.Guest.CPUs.Use.RoundedUp() > uint32(len(cpusPlugged)) {
					// going to plug one CPU
					err := QmpPlugCpuToRunner(virtualmachinemigration.Status.TargetPodIP, vm.Spec.QMP)
					if err != nil {
						return err
					} else {
						log.Info("Plugged CPU to Target Pod", "Pod.Name", virtualmachinemigration.Status.TargetPodName)
					}
				} else {
					// seems all CPUs plugged to target runner
					readyToMigrateCPU = true
				}
				if runnerSupportsCgroup(targetRunner) {
					if err := notifyRunner(ctx, vm, *vm.Spec.Guest.CPUs.Use); err != nil {
						return err
					}
				}
			}

			readyToMigrateMemory := false
			// do hotplug Memory in targetRunner if .spec.guest.memorySlots.use defined
			if vm.Spec.Guest.MemorySlots.Use != nil {
				// firstly get current state from QEMU
				memoryDevices, err := QmpQueryMemoryDevicesFromRunner(virtualmachinemigration.Status.TargetPodIP, vm.Spec.QMP)
				if err != nil {
					log.Error(err, "Failed to get Memory details from Target Runner", "Pod", virtualmachinemigration.Status.TargetPodName)
					return err
				}
				// compare guest spec and count of plugged
				if *vm.Spec.Guest.MemorySlots.Use > *vm.Spec.Guest.MemorySlots.Min+int32(len(memoryDevices)) {
					// going to plug one Memory Slot
					err := QmpPlugMemoryToRunner(virtualmachinemigration.Status.TargetPodIP, vm.Spec.QMP, vm.Spec.Guest.MemorySlotSize.Value())
					if err != nil {
						return err
					} else {
						log.Info("Plugged Memory to Target Pod", "Pod.Name", virtualmachinemigration.Status.TargetPodName)
					}
				} else {
					// seems all Memory Slots plugged to target runner
					readyToMigrateMemory = true
				}
			}

			// Migrate only running VMs to target with plugged devices
			if vm.Status.Phase == vmv1.VmPreMigrating && readyToMigrateMemory && readyToMigrateCPU {
				// update VM status
				vm.Status.Phase = vmv1.VmMigrating
				if err := r.Status().Update(ctx, vm); err != nil {
					log.Error(err, "Failed to update VirtualMachine status to 'Migrating'")
					return err
				}
				meta.SetStatusCondition(&virtualmachinemigration.Status.Conditions,
					metav1.Condition{Type: typeAvailableVirtualMachineMigration,
						Status:  metav1.ConditionTrue,
						Reason:  "Reconciling",
						Message: fmt.Sprintf("Target Pod (%s) for VirtualMachine (%s) created successfully", targetRunner.Name, vm.Name)})
				// trigger migration
				if err := QmpStartMigration(vm, virtualmachinemigration); err != nil {
					virtualmachinemigration.Status.Phase = vmv1.VmmFailed
					return err
				}
				r.Recorder.Event(virtualmachinemigration, "Normal", "Started",
					fmt.Sprintf("VirtualMachine (%s) live migration started to target pod (%s)",
						vm.Name, targetRunner.Name))
				// finally update migration phase to Running
				virtualmachinemigration.Status.Phase = vmv1.VmmRunning
			}
		case corev1.PodSucceeded:
			// target runner pod finished without error? but it shouldn't finish
			virtualmachinemigration.Status.Phase = vmv1.VmmFailed
			meta.SetStatusCondition(&virtualmachinemigration.Status.Conditions,
				metav1.Condition{Type: typeDegradedVirtualMachineMigration,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Target Pod (%s) for VirtualMachine (%s) completed", targetRunner.Name, vm.Name)})
		case corev1.PodFailed:
			virtualmachinemigration.Status.Phase = vmv1.VmmFailed
			meta.SetStatusCondition(&virtualmachinemigration.Status.Conditions,
				metav1.Condition{Type: typeDegradedVirtualMachineMigration,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Target Pod (%s) for VirtualMachine (%s) failed", targetRunner.Name, vm.Name)})
		case corev1.PodUnknown:
			virtualmachinemigration.Status.Phase = vmv1.VmmPending
			meta.SetStatusCondition(&virtualmachinemigration.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachineMigration,
					Status:  metav1.ConditionUnknown,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Target Pod (%s) for VirtualMachine (%s) in Unknown phase", targetRunner.Name, vm.Name)})
		default:
			// do nothing
		}

	case vmv1.VmmRunning:
		// retrieve target pod details
		targetRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: virtualmachinemigration.Status.TargetPodName, Namespace: virtualmachinemigration.Namespace}, targetRunner)
		if err != nil && apierrors.IsNotFound(err) {
			// lost target pod for running Migration ?
			virtualmachinemigration.Status.Phase = vmv1.VmmFailed

			r.Recorder.Event(virtualmachinemigration, "Fatal", "NotFound",
				fmt.Sprintf("Target Pod (%s) for VirtualMachine (%s) disappeared",
					virtualmachinemigration.Status.TargetPodName, vm.Name))

			meta.SetStatusCondition(&virtualmachinemigration.Status.Conditions,
				metav1.Condition{Type: typeDegradedVirtualMachineMigration,
					Status: metav1.ConditionTrue,
					Reason: "Reconciling",
					Message: fmt.Sprintf("Target Pod (%s) for VirtualMachine (%s) disappeared",
						virtualmachinemigration.Status.TargetPodName, vm.Name)})
		} else if err != nil {
			log.Error(err, "Failed to get target runner Pod")
			return err
		}
		// retrieve migration statistics and put it in Status
		migrationInfo, err := QmpGetMigrationInfo(vm)
		if err != nil {
			log.Error(err, "Failed to get migration info")
			return err
		}

		// Store/update migration info in VirtualMachineMigration.Status
		virtualmachinemigration.Status.Info.Status = migrationInfo.Status
		virtualmachinemigration.Status.Info.TotalTimeMs = migrationInfo.TotalTimeMs
		virtualmachinemigration.Status.Info.SetupTimeMs = migrationInfo.SetupTimeMs
		virtualmachinemigration.Status.Info.DowntimeMs = migrationInfo.DowntimeMs
		virtualmachinemigration.Status.Info.Ram.Transferred = migrationInfo.Ram.Transferred
		virtualmachinemigration.Status.Info.Ram.Remaining = migrationInfo.Ram.Remaining
		virtualmachinemigration.Status.Info.Ram.Total = migrationInfo.Ram.Total
		virtualmachinemigration.Status.Info.Compression.CompressedSize = migrationInfo.Compression.CompressedSize
		virtualmachinemigration.Status.Info.Compression.CompressionRate = int64(math.Round(migrationInfo.Compression.CompressionRate))

		// check if migration done
		if migrationInfo.Status == "completed" {
			r.Recorder.Event(virtualmachinemigration, "Normal", "Finished",
				fmt.Sprintf("VirtualMachine (%s) live migration finished with success to target pod (%s)",
					vm.Name, targetRunner.Name))

			// Redefine runner Pod for VM
			vm.Status.PodName = virtualmachinemigration.Status.TargetPodName
			vm.Status.PodIP = virtualmachinemigration.Status.TargetPodIP
			vm.Status.Phase = vmv1.VmRunning
			// update VM status
			if err := r.Status().Update(ctx, vm); err != nil {
				log.Error(err, "Failed to update VirtualMachine status")
				return err
			}

			// Redefine ownerRef for the target Pod
			targetRunner.OwnerReferences = []metav1.OwnerReference{}
			if err := ctrl.SetControllerReference(vm, targetRunner, r.Scheme); err != nil {
				return err
			}
			if err := r.Update(ctx, targetRunner); err != nil {
				log.Error(err, "Failed to update ownerRef for target runner pod")
				return err
			}

			// stop hypervisor in source runner
			if err := QmpQuit(virtualmachinemigration.Status.SourcePodIP, vm.Spec.QMP); err != nil {
				log.Info("Failed stop hypervisor in source runner pod, probably hypervisor already stopped", "error", err)
			}

			// finally update migration phase to Succeeded
			virtualmachinemigration.Status.Phase = vmv1.VmmSucceeded
		}
		// check if migration failed
		if migrationInfo.Status == "failed" {
			// oops, migration failed
			virtualmachinemigration.Status.Phase = vmv1.VmmFailed
			// try to stop hypervisor in target runner
			if err := QmpQuit(virtualmachinemigration.Status.TargetPodIP, vm.Spec.QMP); err != nil {
				log.Info("Failed stop hypervisor in target runner pod, probably hypervisor already stopped", "error", err)
			}
			// change VM status to Running
			vm.Status.Phase = vmv1.VmRunning
			// update VM status
			if err := r.Status().Update(ctx, vm); err != nil {
				log.Error(err, "Failed to update VirtualMachine status")
				return err
			}
		}

	case vmv1.VmmSucceeded:
		// do additional VM status checks
		if vm.Status.Phase == vmv1.VmMigrating {
			// migration Succeeded and VM should have status Running
			vm.Status.Phase = vmv1.VmRunning
			if err := r.Status().Update(ctx, vm); err != nil {
				log.Error(err, "Failed to update VirtualMachine status")
				return err
			}
		}
		if len(virtualmachinemigration.Status.SourcePodName) > 0 {
			// try to find and remove source runner Pod
			sourceRunner := &corev1.Pod{}
			err := r.Get(ctx, types.NamespacedName{Name: virtualmachinemigration.Status.SourcePodName, Namespace: virtualmachinemigration.Namespace}, sourceRunner)
			if err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to get source runner Pod")
				return err
			}
			if err := r.Delete(ctx, sourceRunner); err != nil {
				log.Error(err, "Failed to delete source runner Pod")
				return err
			}
			virtualmachinemigration.Status.SourcePodName = ""
			virtualmachinemigration.Status.SourcePodIP = ""
			r.Recorder.Event(virtualmachinemigration, "Normal", "Deleted",
				fmt.Sprintf("Source runner %s for VM %s was deleted",
					virtualmachinemigration.Status.SourcePodName, vm.Name))
		}

	case vmv1.VmmFailed:
		// do additional VM status checks
		if vm.Status.Phase == vmv1.VmMigrating {
			// migration Failed but VM should have status Running after all
			vm.Status.Phase = vmv1.VmRunning
			if err := r.Status().Update(ctx, vm); err != nil {
				log.Error(err, "Failed to update VirtualMachine status")
				return err
			}
		}
	default:
		// do nothing
	}

	return nil
}

// targetPodForVirtualMachine returns a VirtualMachine Pod object
func (r *VirtualMachineMigrationReconciler) targetPodForVirtualMachine(
	virtualmachine *vmv1.VirtualMachine,
	virtualmachinemigration *vmv1.VirtualMachineMigration) (*corev1.Pod, error) {

	pod, err := podSpec(virtualmachine)
	if err != nil {
		return nil, err
	}

	// override pod name
	pod.Name = virtualmachinemigration.Status.TargetPodName

	// add env variable to turn on migration receiver
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{Name: "RECEIVE_MIGRATION", Value: "true"})

	// add podAntiAffinity to schedule target pod to another k8s node
	if virtualmachinemigration.Spec.PreventMigrationToSameHost {
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
					vmv1.VirtualMachineNameLabel: virtualmachinemigration.Spec.VmName,
				},
			},
			TopologyKey: "kubernetes.io/hostname",
		})
	}

	// use multus network to add extra network interface but without IPAM
	if virtualmachine.Spec.ExtraNetwork != nil {
		if virtualmachine.Spec.ExtraNetwork.Enable {
			pod.ObjectMeta.Annotations[nadapiv1.NetworkAttachmentAnnot] = fmt.Sprintf("%s@%s", virtualmachine.Spec.ExtraNetwork.MultusNetworkNoIP, virtualmachine.Spec.ExtraNetwork.Interface)
		}
	}

	// Set the ownerRef for the Pod
	if err := ctrl.SetControllerReference(virtualmachinemigration, pod, r.Scheme); err != nil {
		return nil, err
	}

	return pod, nil
}
