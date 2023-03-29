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
	virtualmachinemigration := &vmv1.VirtualMachineMigration{}
	err := r.Get(ctx, req.NamespacedName, virtualmachinemigration)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("virtualmachinemigration resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get virtualmachinemigration")
		return ctrl.Result{}, err
	}

	// Let's add a finalizer. Then, we can define some operations which should
	// occurs before the custom resource to be deleted.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers
	if !controllerutil.ContainsFinalizer(virtualmachinemigration, virtualmachinemigrationFinalizer) {
		log.Info("Adding Finalizer for VirtualMachineMigration")

		/* for k8s 1.25
		if ok := controllerutil.AddFinalizer(virtualmachinemigration, virtualmachinemigrationFinalizer); !ok {
			log.Error(err, "Failed to add finalizer into the VM migration")
			return ctrl.Result{Requeue: true}, nil
		}
		*/
		controllerutil.AddFinalizer(virtualmachinemigration, virtualmachinemigrationFinalizer)
		if err = r.Update(ctx, virtualmachinemigration); err != nil {
			log.Error(err, "Failed to update VM migration to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Check if the VirtualMachineMigration instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isVirtualMachineMigrationMarkedToBeDeleted := virtualmachinemigration.GetDeletionTimestamp() != nil
	if isVirtualMachineMigrationMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(virtualmachinemigration, virtualmachinemigrationFinalizer) {
			log.Info("Performing Finalizer Operations for VirtualMachineMigration before delete CR")

			// Let's add here an status "Downgrade" to define that this resource begin its process to be terminated.
			meta.SetStatusCondition(&virtualmachinemigration.Status.Conditions, metav1.Condition{Type: typeDegradedVirtualMachineMigration,
				Status: metav1.ConditionUnknown, Reason: "Finalizing",
				Message: fmt.Sprintf("Performing finalizer operations for the VM migration: %s ", virtualmachinemigration.Name)})

			if err := r.Status().Update(ctx, virtualmachinemigration); err != nil {
				log.Error(err, "Failed to update VirtualMachineMigration status")
				return ctrl.Result{}, err
			}

			// Perform all operations required before remove the finalizer and allow
			// the Kubernetes API to remove the custom custom resource.
			r.doFinalizerOperationsForVirtualMachineMigration(virtualmachinemigration)

			// TODO(user): If you add operations to the doFinalizerOperationsForVirtualMachineMigration method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

			// Re-fetch the virtualmachinemigration Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, virtualmachinemigration); err != nil {
				log.Error(err, "Failed to re-fetch virtualmachinemigration")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(&virtualmachinemigration.Status.Conditions, metav1.Condition{Type: typeDegradedVirtualMachineMigration,
				Status: metav1.ConditionTrue, Reason: "Finalizing",
				Message: fmt.Sprintf("Finalizer operations for VM migration %s name were successfully accomplished", virtualmachinemigration.Name)})

			if err := r.Status().Update(ctx, virtualmachinemigration); err != nil {
				log.Error(err, "Failed to update VirtualMachineMigration status")
				return ctrl.Result{}, err
			}

			log.Info("Removing Finalizer for VirtualMachineMigration after successfully perform the operations")
			/* for k8s 1.25
			if ok := controllerutil.RemoveFinalizer(virtualmachinemigration, virtualmachinemigrationFinalizer); !ok {
				log.Error(err, "Failed to remove finalizer for VirtualMachineMigration")
				return ctrl.Result{Requeue: true}, nil
			}
			*/
			controllerutil.RemoveFinalizer(virtualmachinemigration, virtualmachinemigrationFinalizer)
			if err := r.Update(ctx, virtualmachinemigration); err != nil {
				log.Error(err, "Failed to remove finalizer for VirtualMachineMigration")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Re-fetch the virtualmachinemigration Custom Resource before managing VMM lifecycle
	if err := r.Get(ctx, req.NamespacedName, virtualmachinemigration); err != nil {
		log.Error(err, "Failed to re-fetch virtualmachinemigration")
		return ctrl.Result{}, err
	}

	statusBefore := virtualmachinemigration.Status.DeepCopy()
	rerr := r.doReconcile(ctx, virtualmachinemigration)
	if !DeepEqual(virtualmachinemigration.Status, statusBefore) {
		// update VirtualMachineMigration status
		if err := r.Status().Update(ctx, virtualmachinemigration); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			log.Error(err, "Failed to update VirtualMachineMigration status after reconcile loop")
			return ctrl.Result{}, err
		}
	}
	if rerr != nil {
		r.Recorder.Eventf(virtualmachinemigration, corev1.EventTypeWarning, "Failed",
			"Failed to reconcile (%s): %s", virtualmachinemigration.Name, rerr)
		return ctrl.Result{}, rerr
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// finalizeVirtualMachineMigration will perform the required operations before delete the CR.
func (r *VirtualMachineMigrationReconciler) doFinalizerOperationsForVirtualMachineMigration(cr *vmv1.VirtualMachineMigration) {
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
	r.Recorder.Event(cr, "Warning", "Deleting",
		fmt.Sprintf("VM migration '%s' is being deleted from the namespace %s",
			cr.Name,
			cr.Namespace))
}

// SetupWithManager sets up the controller with the Manager.
// Note that the Deployment will be also watched in order to ensure its
// desirable state on the cluster
func (r *VirtualMachineMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vmv1.VirtualMachineMigration{}).
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
	}

	switch virtualmachinemigration.Status.Phase {

	case "":
		// Set the ownerRef for the migration
		if err := ctrl.SetControllerReference(vm, virtualmachinemigration, r.Scheme); err != nil {
			return err
		}
		if err := r.Update(ctx, virtualmachinemigration); err != nil {
			log.Error(err, "Failed to update VM migration to add Owner reference")
			return err
		}
		// VirtualMachineMigration just created, change Phase to "Pending"
		virtualmachinemigration.Status.Phase = vmv1.VmmPending
	case vmv1.VmmPending:
		// Get source runner pods details
		sourceRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: vm.Status.PodName, Namespace: vm.Namespace}, sourceRunner)
		if err != nil && apierrors.IsNotFound(err) {
			log.Error(err, "Failed to get Source Pod")
			return err
		}
		// Check if the target runner pod already exists, if not create a new one using source pod as template
		targetRunner := &corev1.Pod{}
		err = r.Get(ctx, types.NamespacedName{Name: virtualmachinemigration.Status.TargetPodName, Namespace: vm.Namespace}, targetRunner)
		if err != nil && apierrors.IsNotFound(err) {
			// Define a new target pod
			//tpod, err := r.targetPodForVirtualMachine(sourceRunner, virtualmachinemigration)
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
				fmt.Sprintf("Created Target Pod for VirtualMachine %s",
					vm.Name))
			// once more retrieve just created target pod details
			r.Get(ctx, types.NamespacedName{Name: virtualmachinemigration.Status.TargetPodName, Namespace: virtualmachinemigration.Namespace}, targetRunner)
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
				if *vm.Spec.Guest.CPUs.Use > int32(len(cpusPlugged)) {
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
			if vm.Status.Phase == vmv1.VmRunning && readyToMigrateMemory && readyToMigrateCPU {
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

		// TODO: delete this log as it used for debug mostly
		// log.Info("Migration info", "Info", migrationInfo)

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
				log.Error(err, "Failed stop hypervisor in source runner pod")
				return err
			}

			// finally update migration phase to Succeeded
			virtualmachinemigration.Status.Phase = vmv1.VmmSucceeded
		}

	case vmv1.VmmSucceeded:
		// do nothing
	case vmv1.VmmFailed:
		// do nothing
	default:
		// do nothing
	}

	return nil
}

// targetPodForVirtualMachine returns a VirtualMachine Pod object
func (r *VirtualMachineMigrationReconciler) targetPodForVirtualMachine(
	virtualmachine *vmv1.VirtualMachine, virtualmachinemigration *vmv1.VirtualMachineMigration) (*corev1.Pod, error) {

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

	// Set the ownerRef for the Pod
	if err := ctrl.SetControllerReference(virtualmachinemigration, pod, r.Scheme); err != nil {
		return nil, err
	}

	return pod, nil
}
