/*
Copyright 2022.

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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/bufbuild/connect-go"
	goipamapiv1 "github.com/cicdteam/go-ipam/api/v1"
	"github.com/cicdteam/go-ipam/api/v1/apiv1connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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
	"github.com/neondatabase/autoscaling/pkg/api"
)

const (
	virtualmachineFinalizer = "vm.neon.tech/finalizer"
	ipamServerVariableName  = "IPAM_SERVER"
)

// Definitions to manage status conditions
const (
	// typeAvailableVirtualMachine represents the status of the Deployment reconciliation
	typeAvailableVirtualMachine = "Available"
	// typeDegradedVirtualMachine represents the status used when the custom resource is deleted and the finalizer operations are must to occur.
	typeDegradedVirtualMachine = "Degraded"
)

// VirtualMachineReconciler reconciles a VirtualMachine object
type VirtualMachineReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachines/finalizers,verbs=update
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
func (r *VirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the VirtualMachine instance
	// The purpose is check if the Custom Resource for the Kind VirtualMachine
	// is applied on the cluster if not we return nil to stop the reconciliation

	//virtualmachine := &vmv1.VirtualMachine{}
	var virtualmachine vmv1.VirtualMachine
	err := r.Get(ctx, req.NamespacedName, &virtualmachine)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			//log.V(2).Info("virtualmachine resource not found. Ignoring since object must be deleted")
			log.Info("virtualmachine resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get virtualmachine")
		return ctrl.Result{}, err
	}

	// Let's add a finalizer. Then, we can define some operations which should
	// occurs before the custom resource to be deleted.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers
	if !controllerutil.ContainsFinalizer(&virtualmachine, virtualmachineFinalizer) {
		log.Info("Adding Finalizer for VirtualMachine")
		controllerutil.AddFinalizer(&virtualmachine, virtualmachineFinalizer)

		if err = r.Update(ctx, &virtualmachine); err != nil {
			log.Error(err, "Failed to update custom resource to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Check if the VirtualMachine instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isVirtualMachineMarkedToBeDeleted := virtualmachine.GetDeletionTimestamp() != nil
	if isVirtualMachineMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(&virtualmachine, virtualmachineFinalizer) {
			log.Info("Performing Finalizer Operations for VirtualMachine before delete CR")

			// Let's add here an status "Downgrade" to define that this resource begin its process to be terminated.
			meta.SetStatusCondition(&virtualmachine.Status.Conditions, metav1.Condition{Type: typeDegradedVirtualMachine,
				Status: metav1.ConditionUnknown, Reason: "Finalizing",
				Message: fmt.Sprintf("Performing finalizer operations for the custom resource: %s ", virtualmachine.Name)})

			if err := r.Status().Update(ctx, &virtualmachine); err != nil {
				log.Error(err, "Failed to update VirtualMachine status")
				return ctrl.Result{}, err
			}

			// Perform all operations required before remove the finalizer and allow
			// the Kubernetes API to remove the custom custom resource.
			r.doFinalizerOperationsForVirtualMachine(ctx, &virtualmachine)

			// TODO(user): If you add operations to the doFinalizerOperationsForVirtualMachine method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

			// Re-fetch the virtualmachine Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, &virtualmachine); err != nil {
				log.Error(err, "Failed to re-fetch virtualmachine")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(&virtualmachine.Status.Conditions, metav1.Condition{Type: typeDegradedVirtualMachine,
				Status: metav1.ConditionTrue, Reason: "Finalizing",
				Message: fmt.Sprintf("Finalizer operations for custom resource %s name were successfully accomplished", virtualmachine.Name)})

			/*

				if err := r.Status().Update(ctx, &virtualmachine); err != nil {
					log.Error(err, "Failed to update VirtualMachine status")
					return ctrl.Result{}, err
				}

			*/

			log.Info("Removing Finalizer for VirtualMachine after successfully perform the operations")
			controllerutil.RemoveFinalizer(&virtualmachine, virtualmachineFinalizer)

			if err := r.Update(ctx, &virtualmachine); err != nil {
				log.Error(err, "Failed to remove finalizer for VirtualMachine")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Re-fetch the virtualmachine Custom Resource before managing VM lifecycle
	if err := r.Get(ctx, req.NamespacedName, &virtualmachine); err != nil {
		log.Error(err, "Failed to re-fetch virtualmachine")
		return ctrl.Result{}, err
	}

	statusBefore := virtualmachine.Status.DeepCopy()
	if err := r.doReconcile(ctx, &virtualmachine); err != nil {
		r.Recorder.Eventf(&virtualmachine, corev1.EventTypeWarning, "Failed",
			"Failed to reconcile (%s): %s", virtualmachine.Name, err)
		return ctrl.Result{}, err
	}

	// update status after reconcile loop (try 10 times)
	statusNow := statusBefore.DeepCopy()
	statusNew := virtualmachine.Status.DeepCopy()
	try := 1
	for try < 10 {
		if !DeepEqual(statusNow, statusNew) {
			// update VirtualMachine status
			// log.Info("DEBUG", "StatusNow", statusNow, "StatusNew", statusNew, "attempt", try)
			if err := r.Status().Update(ctx, &virtualmachine); err != nil {
				if apierrors.IsConflict(err) {
					try++
					time.Sleep(time.Second)
					// re-get statusNow from current state
					statusNow = virtualmachine.Status.DeepCopy()
					continue
				}
				log.Error(err, "Failed to update VirtualMachine status after reconcile loop",
					"virtualmachine", virtualmachine.Name)
				return ctrl.Result{}, err
			}
		}
		// status updated (before and now are equal)
		break
	}
	if try >= 10 {
		return ctrl.Result{}, fmt.Errorf("unable update .specextraNetwork for virtualmachine %s in %d attempts", virtualmachine.Name, try)
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// finalizeVirtualMachine will perform the required operations before delete the CR.
func (r *VirtualMachineReconciler) doFinalizerOperationsForVirtualMachine(ctx context.Context, virtualmachine *vmv1.VirtualMachine) {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as depended of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	log := log.FromContext(ctx)

	// release IP for ExtraNetwork interface
	if virtualmachine.Spec.ExtraNetwork != nil {
		ipamService := os.Getenv(ipamServerVariableName)
		if len(ipamService) == 0 {
			log.Error(fmt.Errorf("IPAM service not found, environment variable %s is empty", ipamServerVariableName), "ipam service not defined")
		} else if len(virtualmachine.Status.ExtraNetIP) != 0 {
			log.Info("Going to release IP address used for ExtraNetwork",
				"virtualmachine", virtualmachine.Name,
				"extraNetIP", virtualmachine.Status.ExtraNetIP)
			err := releaseIP(ctx, ipamService, virtualmachine.Status.ExtraNetIP)
			if err != nil {
				log.Error(err, "Failed to release IP address for ExtraNetwork",
					"virtualmachine", virtualmachine.Name,
					"extraNetIP", virtualmachine.Status.ExtraNetIP)
			}
		}
	}

	// The following implementation will raise an event
	r.Recorder.Event(virtualmachine, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			virtualmachine.Name,
			virtualmachine.Namespace))
}

func runnerSupportsCgroup(pod *corev1.Pod) bool {
	val, ok := pod.Labels[vmv1.RunnerPodVersionLabel]
	if !ok {
		return false
	}

	uintVal, err := strconv.ParseUint(val, 10, 32)
	if err != nil {
		return false
	}

	return api.RunnerProtoVersion(uintVal).SupportsCgroupFractionalCPU()
}

func (r *VirtualMachineReconciler) doReconcile(ctx context.Context, virtualmachine *vmv1.VirtualMachine) error {
	log := log.FromContext(ctx)

	// Let's check and just set the condition status as Unknown when no status are available
	if virtualmachine.Status.Conditions == nil || len(virtualmachine.Status.Conditions) == 0 {
		// set Unknown condition status for AvailableVirtualMachine
		meta.SetStatusCondition(&virtualmachine.Status.Conditions, metav1.Condition{Type: typeAvailableVirtualMachine, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
	}

	// Generate runner pod name
	if len(virtualmachine.Status.PodName) == 0 {
		virtualmachine.Status.PodName = names.SimpleNameGenerator.GenerateName(fmt.Sprintf("%s-", virtualmachine.Name))
	}

	switch virtualmachine.Status.Phase {

	case "":
		// VirtualMachine just created, change Phase to "Pending"
		virtualmachine.Status.Phase = vmv1.VmPending
	case vmv1.VmPending:
		// acquire IP for ExtraNetwork interface
		if virtualmachine.Spec.ExtraNetwork != nil {
			ipamService := os.Getenv(ipamServerVariableName)
			if len(ipamService) == 0 {
				return fmt.Errorf("IPAM service not found, environment variable %s is empty", ipamServerVariableName)
			}
			if len(virtualmachine.Status.ExtraNetIP) == 0 {
				log.Info("Trying acquire IP address and mask for ExtraNetwork")
				ip, mask, err := acquireIP(ctx, ipamService)
				if err != nil {
					log.Error(err, "Failed to acquire IP address for ExtraNetwork")
					return err
				}
				log.Info("Acquired IP address and mask", "ip", ip.String(), "mask", mask.String())
				virtualmachine.Status.ExtraNetIP = ip.String()
				virtualmachine.Status.ExtraNetMask = fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])

				r.Recorder.Event(virtualmachine, "Normal", "Acquired",
					fmt.Sprintf("Acquired IP %s for VirtualMachine %s",
						virtualmachine.Status.ExtraNetIP,
						virtualmachine.Name))
			}
		}

		// Check if the runner pod already exists, if not create a new one
		vmRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: virtualmachine.Status.PodName, Namespace: virtualmachine.Namespace}, vmRunner)
		if err != nil && apierrors.IsNotFound(err) {
			// Define a new pod
			pod, err := r.podForVirtualMachine(virtualmachine)
			if err != nil {
				log.Error(err, "Failed to define new Pod resource for VirtualMachine")
				return err
			}

			log.Info("Creating a new Pod", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
			if err = r.Create(ctx, pod); err != nil {
				log.Error(err, "Failed to create new Pod", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
				return err
			}

			r.Recorder.Event(virtualmachine, "Normal", "Created",
				fmt.Sprintf("Created VirtualMachine %s",
					virtualmachine.Name))
		} else if err != nil {
			log.Error(err, "Failed to get vm-runner Pod")
			return err
		}
		// runner pod found, check phase
		switch vmRunner.Status.Phase {
		case corev1.PodRunning:
			virtualmachine.Status.PodIP = vmRunner.Status.PodIP
			virtualmachine.Status.Phase = vmv1.VmRunning
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) created successfully", virtualmachine.Status.PodName, virtualmachine.Name)})
		case corev1.PodSucceeded:
			virtualmachine.Status.Phase = vmv1.VmSucceeded
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachine,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) succeeded", virtualmachine.Status.PodName, virtualmachine.Name)})
		case corev1.PodFailed:
			virtualmachine.Status.Phase = vmv1.VmFailed
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeDegradedVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) failed", virtualmachine.Status.PodName, virtualmachine.Name)})
		case corev1.PodUnknown:
			virtualmachine.Status.Phase = vmv1.VmPending
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachine,
					Status:  metav1.ConditionUnknown,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) in Unknown phase", virtualmachine.Status.PodName, virtualmachine.Name)})
		default:
			// do nothing
		}
	case vmv1.VmRunning:
		// Check if the runner pod exists
		vmRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: virtualmachine.Status.PodName, Namespace: virtualmachine.Namespace}, vmRunner)
		if err != nil && apierrors.IsNotFound(err) {
			// lost runner pod for running VirtualMachine ?
			r.Recorder.Event(virtualmachine, "Warning", "NotFound",
				fmt.Sprintf("runner pod %s not fodund",
					virtualmachine.Status.PodName))
			virtualmachine.Status.Phase = vmv1.VmFailed
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeDegradedVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) not found", virtualmachine.Status.PodName, virtualmachine.Name)})
		} else if err != nil {
			log.Error(err, "Failed to get runner Pod")
			return err
		}
		// runner pod found, check/update phase now
		switch vmRunner.Status.Phase {
		case corev1.PodRunning:
			// update status by IP of runner pod
			virtualmachine.Status.PodIP = vmRunner.Status.PodIP
			// update phase
			virtualmachine.Status.Phase = vmv1.VmRunning
			// update Node name where runner working
			virtualmachine.Status.Node = vmRunner.Spec.NodeName

			// do hotplug/unplug CPU if .spec.guest.cpus.use defined
			if virtualmachine.Spec.Guest.CPUs.Use != nil {
				// firstly get current state from QEMU
				cpusPlugged, _, err := QmpGetCpus(virtualmachine)
				if err != nil {
					log.Error(err, "Failed to get CPU details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
					return err
				}
				// compare guest spec and count of plugged
				if virtualmachine.Spec.Guest.CPUs.Use.Value() > int64(len(cpusPlugged)) {
					// going to plug one CPU
					if err := QmpPlugCpu(virtualmachine); err != nil {
						return err
					}
				} else if virtualmachine.Spec.Guest.CPUs.Use.Value() < int64(len(cpusPlugged)) {
					// going to unplug one CPU
					if err := QmpUnplugCpu(virtualmachine); err != nil {
						return err
					}
				}
			}

			// get CPU details from QEMU and update status
			cpuSlotsPlugged, _, err := QmpGetCpus(virtualmachine)
			if err != nil {
				log.Error(err, "Failed to get CPU details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
				return err
			}
			specCPU := virtualmachine.Spec.Guest.CPUs.Use
			pluggedCPU := int64(len(cpuSlotsPlugged))
			var cgroupUsage api.VCPUCgroup
			supportsCgroup := runnerSupportsCgroup(vmRunner)
			if supportsCgroup {
				cgroupUsage, err = getRunnerCgroup(ctx, virtualmachine)
				if err != nil {
					log.Error(err, "Failed to get CPU details from runner", "VirtualMachine", virtualmachine.Name)
					return err
				}
			}

			// update cgroup when necessary
			// if we're done scaling (plugged all CPU) then apply cgroup
			// else just use all
			var targetCPUUsage resource.Quantity
			if specCPU != nil && specCPU.Value() == pluggedCPU {
				targetCPUUsage = *specCPU
			} else {
				targetCPUUsage = *resource.NewQuantity(pluggedCPU, resource.BinarySI)
			}

			if supportsCgroup && targetCPUUsage.Cmp(cgroupUsage.VCPUs) != 0 {
				if err := notifyRunner(ctx, virtualmachine, targetCPUUsage); err != nil {
					return err
				}
			}

			if virtualmachine.Status.CPUs == nil || virtualmachine.Status.CPUs.Cmp(*virtualmachine.Spec.Guest.CPUs.Use) != 0 {
				// update status by count of CPU cores used in VM
				virtualmachine.Status.CPUs = virtualmachine.Spec.Guest.CPUs.Use
				// record event about cpus used in VM
				r.Recorder.Event(virtualmachine, "Normal", "CpuInfo",
					fmt.Sprintf("VirtualMachine %s uses %v cpu cores",
						virtualmachine.Name,
						&virtualmachine.Status.CPUs))
			}

			// do hotplug/unplug Memory if .spec.guest.memorySlots.use defined
			if virtualmachine.Spec.Guest.MemorySlots.Use != nil {
				// firstly get current state from QEMU
				memoryDevices, err := QmpQueryMemoryDevices(virtualmachine)
				if err != nil {
					log.Error(err, "Failed to get Memory details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
					return err
				}
				// compare guest spec and count of plugged
				if *virtualmachine.Spec.Guest.MemorySlots.Use > *virtualmachine.Spec.Guest.MemorySlots.Min+int32(len(memoryDevices)) {
					// going to plug one Memory Slot
					if err := QmpPlugMemory(virtualmachine); err != nil {
						return err
					}
				} else if *virtualmachine.Spec.Guest.MemorySlots.Use < *virtualmachine.Spec.Guest.MemorySlots.Min+int32(len(memoryDevices)) {
					// going to unplug one Memory Slot
					if err := QmpUnplugMemory(virtualmachine); err != nil {
						return err
					}
				}
			}

			// get Memory details from hypervisor and update VM status
			memorySize, err := QmpGetMemorySize(virtualmachine)
			if err != nil {
				log.Error(err, "Failed to get Memory details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
				return err
			}
			// update status by Memory sizes used in VM
			if virtualmachine.Status.MemorySize == nil {
				virtualmachine.Status.MemorySize = memorySize
				r.Recorder.Event(virtualmachine, "Normal", "MemoryInfo",
					fmt.Sprintf("VirtualMachine %s uses %v memory",
						virtualmachine.Name,
						virtualmachine.Status.MemorySize))
			} else if !memorySize.Equal(*virtualmachine.Status.MemorySize) {
				virtualmachine.Status.MemorySize = memorySize
				r.Recorder.Event(virtualmachine, "Normal", "MemoryInfo",
					fmt.Sprintf("VirtualMachine %s uses %v memory",
						virtualmachine.Name,
						virtualmachine.Status.MemorySize))
			}

			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) created successfully", virtualmachine.Status.PodName, virtualmachine.Name)})
		case corev1.PodSucceeded:
			virtualmachine.Status.Phase = vmv1.VmSucceeded
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachine,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) succeeded", virtualmachine.Status.PodName, virtualmachine.Name)})
		case corev1.PodFailed:
			virtualmachine.Status.Phase = vmv1.VmFailed
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeDegradedVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) failed", virtualmachine.Status.PodName, virtualmachine.Name)})
		case corev1.PodUnknown:
			virtualmachine.Status.Phase = vmv1.VmPending
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachine,
					Status:  metav1.ConditionUnknown,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) in Unknown phase", virtualmachine.Status.PodName, virtualmachine.Name)})
		default:
			// do nothing
		}

	case vmv1.VmSucceeded, vmv1.VmFailed:
		// TODO: implement RestartPolicy
		switch virtualmachine.Spec.RestartPolicy {
		case vmv1.RestartPolicyAlways:
			// TODO: restart VirtualMachine
		case vmv1.RestartPolicyOnFailure:
			// TODO: restart VirtualMachine
		case vmv1.RestartPolicyNever:
			// TODO: implement TTL or do nothing
		default:
			// do nothing
		}

	default:
		// do nothing
	}

	return nil
}

// podForVirtualMachine returns a VirtualMachine Pod object
func (r *VirtualMachineReconciler) podForVirtualMachine(
	virtualmachine *vmv1.VirtualMachine) (*corev1.Pod, error) {

	pod, err := podSpec(virtualmachine)
	if err != nil {
		return nil, err
	}

	// Set the ownerRef for the Pod
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(virtualmachine, pod, r.Scheme); err != nil {
		return nil, err
	}

	return pod, nil
}

// labelsForVirtualMachine returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForVirtualMachine(virtualmachine *vmv1.VirtualMachine) map[string]string {
	l := virtualmachine.Labels
	if l == nil {
		l = map[string]string{}
	}
	l["app.kubernetes.io/name"] = "NeonVM"
	l[vmv1.VirtualMachineNameLabel] = virtualmachine.Name
	l[vmv1.RunnerPodVersionLabel] = fmt.Sprintf("%d", api.RunnerProtoV1)
	return l
}

func affinityForVirtualMachine(virtualmachine *vmv1.VirtualMachine) *corev1.Affinity {
	a := virtualmachine.Spec.Affinity
	if a == nil {
		a = &corev1.Affinity{}
	}
	if a.NodeAffinity == nil {
		a.NodeAffinity = &corev1.NodeAffinity{}
	}
	if a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}

	// if NodeSelectorTerms list is empty - add default values (arch==amd84 or os==linux)
	if len(a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {
		a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = append(
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms,
			corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{
						Key:      "kubernetes.io/arch",
						Operator: "In",
						Values:   []string{"amd64"},
					},
					{
						Key:      "kubernetes.io/os",
						Operator: "In",
						Values:   []string{"linux"},
					},
				},
			})
	}
	return a
}

func notifyRunner(ctx context.Context, vm *vmv1.VirtualMachine, r resource.Quantity) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/cpu_change", vm.Status.PodIP, vm.Spec.RunnerPort)

	update := api.VCPUChange{VCPUs: r}

	data, err := json.Marshal(update)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}

func getRunnerCgroup(ctx context.Context, vm *vmv1.VirtualMachine) (api.VCPUCgroup, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result := api.VCPUCgroup{}

	url := fmt.Sprintf("http://%s:%d/cpu_current", vm.Status.PodIP, vm.Spec.RunnerPort)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return result, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return result, err
	}

	if resp.StatusCode != 200 {
		return result, fmt.Errorf("unexpected status %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		return result, err
	}

	err = json.Unmarshal(body, &result)
	if err != nil {
		return result, err
	}

	return result, nil
}

// imageForVirtualMachine gets the Operand image which is managed by this controller
// from the VM_RUNNER_IMAGE environment variable defined in the config/manager/manager.yaml
func imageForVmRunner() (string, error) {
	var imageEnvVar = "VM_RUNNER_IMAGE"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("unable to find %s environment variable with the image", imageEnvVar)
	}
	return image, nil
}

func podSpec(virtualmachine *vmv1.VirtualMachine) (*corev1.Pod, error) {
	labels := labelsForVirtualMachine(virtualmachine)
	affinity := affinityForVirtualMachine(virtualmachine)

	// Get the Operand image
	image, err := imageForVmRunner()
	if err != nil {
		return nil, err
	}

	vmSpecJson, err := json.Marshal(virtualmachine.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal VM Spec: %s", err)
	}

	vmStatusJson, err := json.Marshal(virtualmachine.Status)
	if err != nil {
		return nil, fmt.Errorf("marshal VM Status: %s", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        virtualmachine.Status.PodName,
			Namespace:   virtualmachine.Namespace,
			Labels:      labels,
			Annotations: virtualmachine.Annotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicy(virtualmachine.Spec.RestartPolicy),
			TerminationGracePeriodSeconds: virtualmachine.Spec.TerminationGracePeriodSeconds,
			NodeSelector:                  virtualmachine.Spec.NodeSelector,
			ImagePullSecrets:              virtualmachine.Spec.ImagePullSecrets,
			Tolerations:                   virtualmachine.Spec.Tolerations,
			SchedulerName:                 virtualmachine.Spec.SchedulerName,
			Affinity:                      affinity,
			InitContainers: []corev1.Container{{
				Image:           virtualmachine.Spec.Guest.RootDisk.Image,
				Name:            "init-rootdisk",
				ImagePullPolicy: virtualmachine.Spec.Guest.RootDisk.ImagePullPolicy,
				Args:            []string{"cp", "/disk.qcow2", "/vm/images/rootdisk.qcow2"},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "virtualmachineimages",
					MountPath: "/vm/images",
				}},
			}},
			Containers: []corev1.Container{{
				Image:           image,
				Name:            "runner",
				ImagePullPolicy: corev1.PullIfNotPresent,
				// Ensure restrictive context for the container
				// More info: https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted
				SecurityContext: &corev1.SecurityContext{
					Privileged: &[]bool{true}[0],
				},
				Ports: []corev1.ContainerPort{{
					ContainerPort: virtualmachine.Spec.QMP,
					Name:          "qmp",
				}},
				Command: []string{
					"runner",
					"-vmspec", base64.StdEncoding.EncodeToString(vmSpecJson),
					"-vmstatus", base64.StdEncoding.EncodeToString(vmStatusJson),
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "virtualmachineimages",
					MountPath: "/vm/images",
				}},
				Resources: virtualmachine.Spec.PodResources,
			}},
			Volumes: []corev1.Volume{{
				Name: "virtualmachineimages",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			}},
		},
	}

	for _, port := range virtualmachine.Spec.Guest.Ports {
		cPort := corev1.ContainerPort{
			ContainerPort: int32(port.Port),
		}
		if len(port.Name) != 0 {
			cPort.Name = port.Name
		}
		if len(port.Protocol) != 0 {
			cPort.Protocol = corev1.Protocol(port.Protocol)
		}
		pod.Spec.Containers[0].Ports = append(pod.Spec.Containers[0].Ports, cPort)
	}

	for _, disk := range virtualmachine.Spec.Disks {

		mnt := corev1.VolumeMount{
			Name:      disk.Name,
			MountPath: fmt.Sprintf("/vm/mounts%s", disk.MountPath),
		}
		if disk.ReadOnly != nil {
			mnt.ReadOnly = *disk.ReadOnly
		}

		switch {
		case disk.ConfigMap != nil:
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, mnt)
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: disk.Name,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: disk.ConfigMap.Name,
						},
						Items: disk.ConfigMap.Items,
					},
				},
			})
		case disk.Secret != nil:
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, mnt)
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: disk.Name,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: disk.Secret.SecretName,
						Items:      disk.Secret.Items,
					},
				},
			})
		case disk.EmptyDisk != nil:
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, mnt)
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: disk.Name,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						SizeLimit: &disk.EmptyDisk.Size,
					},
				},
			})
		default:
			// do nothing
		}
	}

	// use multus network tp add extra network interface
	if virtualmachine.Spec.ExtraNetwork != nil {
		if pod.ObjectMeta.Annotations == nil {
			pod.ObjectMeta.Annotations = map[string]string{}
		}
		pod.ObjectMeta.Annotations["k8s.v1.cni.cncf.io/networks"] = fmt.Sprintf("%s@%s", virtualmachine.Spec.ExtraNetwork.MultusNetwork, virtualmachine.Spec.ExtraNetwork.Interface)
	}

	return pod, nil
}

// SetupWithManager sets up the controller with the Manager.
// Note that the Runner Pod will be also watched in order to ensure its
// desirable state on the cluster
func (r *VirtualMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vmv1.VirtualMachine{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

func waitForGrpc(ctx context.Context, addr string) {
	log := log.FromContext(ctx)

	dialAddr := strings.TrimPrefix(addr, "http://")
	dialTimeout := 5 * time.Second
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	}
	log.Info("check grpc connection", "address", dialAddr)
	for {
		dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
		defer dialCancel()
		check, err := grpc.DialContext(dialCtx, dialAddr, dialOpts...)
		if err != nil {
			if err == context.DeadlineExceeded {
				log.Info("timeout: failed to connect to grpc service", "address", dialAddr)
			} else {
				log.Info("failed to connect to grpc service", "address", dialAddr, "error", err)
			}
		} else {
			log.Info("grpc connected", "grpc state", check.GetState())
			break
		}
	}
}

func acquireIP(ctx context.Context, ipam string) (net.IP, net.IPMask, error) {
	log := log.FromContext(ctx)

	waitForGrpc(ctx, ipam)

	c := apiv1connect.NewIpamServiceClient(
		http.DefaultClient,
		ipam,
		connect.WithGRPC(),
	)

	// get prefixes from IPAM service
	prefixes, err := c.ListPrefixes(ctx, connect.NewRequest(&goipamapiv1.ListPrefixesRequest{}))
	if err != nil {
		return net.IP{}, net.IPMask{}, err
	}
	p := prefixes.Msg.Prefixes
	if len(p) == 0 {
		return net.IP{}, net.IPMask{}, errors.New("IPAM prefix not found")
	}
	if len(p) > 1 {
		return net.IP{}, net.IPMask{}, fmt.Errorf("too many IPAM prefixes found (%d)", len(p))
	}

	result, err := c.AcquireIP(ctx, connect.NewRequest(&goipamapiv1.AcquireIPRequest{PrefixCidr: p[0].Cidr}))
	if err != nil {
		return net.IP{}, net.IPMask{}, err
	}
	log.Info("ip acquired", "ip", result.Msg.Ip.Ip)

	// parse overlay cidr for IPMask
	_, ipv4Net, err := net.ParseCIDR(p[0].Cidr)
	if err != nil {
		return net.IP{}, net.IPMask{}, err
	}
	ip := net.ParseIP(result.Msg.Ip.Ip)
	mask := ipv4Net.Mask

	return ip, mask, nil
}

func releaseIP(ctx context.Context, ipam string, ip string) error {
	log := log.FromContext(ctx)

	waitForGrpc(ctx, ipam)

	c := apiv1connect.NewIpamServiceClient(
		http.DefaultClient,
		ipam,
		connect.WithGRPC(),
	)

	// get prefixes from IPAM service
	prefixes, err := c.ListPrefixes(ctx, connect.NewRequest(&goipamapiv1.ListPrefixesRequest{}))
	if err != nil {
		return err
	}
	p := prefixes.Msg.Prefixes
	if len(p) == 0 {
		return errors.New("IPAM prefix not found")
	}
	if len(p) > 1 {
		return fmt.Errorf("too many IPAM prefixes found (%d)", len(p))
	}

	result, err := c.ReleaseIP(ctx, connect.NewRequest(&goipamapiv1.ReleaseIPRequest{PrefixCidr: p[0].Cidr, Ip: ip}))
	if err != nil {
		return err
	}
	log.Info("ip released", "ip", result.Msg.Ip.Ip)

	return nil
}

func DeepEqual(v1, v2 interface{}) bool {
	if reflect.DeepEqual(v1, v2) {
		return true
	}
	var x1 interface{}
	bytesA, _ := json.Marshal(v1)
	_ = json.Unmarshal(bytesA, &x1)
	var x2 interface{}
	bytesB, _ := json.Marshal(v2)
	_ = json.Unmarshal(bytesB, &x2)

	return reflect.DeepEqual(x1, x2)
}
