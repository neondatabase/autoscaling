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
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nadapiv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/neonvm/pkg/ipam"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

const (
	virtualmachineFinalizer = "vm.neon.tech/finalizer"
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
//+kubebuilder:rbac:groups=vm.neon.tech,resources=ippools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=vm.neon.tech,resources=ippools/finalizers,verbs=update
//+kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch

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

	var virtualmachine vmv1.VirtualMachine
	if err := r.Get(ctx, req.NamespacedName, &virtualmachine); err != nil {
		// Error reading the object - requeue the request.
		if notfound := client.IgnoreNotFound(err); notfound == nil {
			log.Info("virtualmachine resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Unable to fetch VirtualMachine")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if virtualmachine.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(&virtualmachine, virtualmachineFinalizer) {
			log.Info("Adding Finalizer for VirtualMachine")
			if ok := controllerutil.AddFinalizer(&virtualmachine, virtualmachineFinalizer); !ok {
				log.Info("Failed to add finalizer from VirtualMachine")
				return ctrl.Result{Requeue: true}, nil
			}
			if err := r.tryUpdateVM(ctx, &virtualmachine); err != nil {
				log.Error(err, "Failed to update status about adding finalizer to VirtualMachine")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(&virtualmachine, virtualmachineFinalizer) {
			// our finalizer is present, so lets handle any external dependency
			log.Info("Performing Finalizer Operations for VirtualMachine before delete it")
			if err := r.doFinalizerOperationsForVirtualMachine(ctx, &virtualmachine); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			log.Info("Removing Finalizer for VirtualMachine after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(&virtualmachine, virtualmachineFinalizer); !ok {
				log.Info("Failed to remove finalizer from VirtualMachine")
				return ctrl.Result{Requeue: true}, nil
			}
			if err := r.tryUpdateVM(ctx, &virtualmachine); err != nil {
				log.Error(err, "Failed to update status about removing finalizer from VirtualMachine")
				return ctrl.Result{}, err
			}
		}
		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
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
		return ctrl.Result{}, fmt.Errorf("unable update .status for virtualmachine %s in %d attempts", virtualmachine.Name, try)
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// finalizeVirtualMachine will perform the required operations before delete the CR.
func (r *VirtualMachineReconciler) doFinalizerOperationsForVirtualMachine(ctx context.Context, virtualmachine *vmv1.VirtualMachine) error {
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

	// The following implementation will raise an event
	r.Recorder.Event(virtualmachine, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			virtualmachine.Name,
			virtualmachine.Namespace))

	// Release overlay IP address
	if virtualmachine.Spec.ExtraNetwork != nil {
		// Create IPAM object
		nadName, err := nadIpamName()
		if err != nil {
			// ignore error
			log.Error(err, "ignored error")
			return nil
		}
		nadNamespace, err := nadIpamNamespace()
		if err != nil {
			// ignore error
			log.Error(err, "ignored error")
			return nil
		}
		ipam, err := ipam.New(ctx, nadName, nadNamespace)
		if err != nil {
			// ignore error
			log.Error(err, "ignored error")
			return nil
		}
		defer ipam.Close()
		ip, err := ipam.ReleaseIP(ctx, virtualmachine.Name, virtualmachine.Namespace)
		if err != nil {
			// ignore error
			log.Error(err, "fail to release IP, error ignored")
			return nil
		}
		message := fmt.Sprintf("Released IP %s", ip.String())
		log.Info(message)
		r.Recorder.Event(virtualmachine, "Normal", "OverlayNet", message)
	}

	return nil
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
		// Acquire overlay IP address
		if virtualmachine.Spec.ExtraNetwork != nil &&
			virtualmachine.Spec.ExtraNetwork.Enable &&
			len(virtualmachine.Status.ExtraNetIP) == 0 {
			// Create IPAM object
			nadName, err := nadIpamName()
			if err != nil {
				return err
			}
			nadNamespace, err := nadIpamNamespace()
			if err != nil {
				return err
			}
			ipam, err := ipam.New(ctx, nadName, nadNamespace)
			if err != nil {
				log.Error(err, "failed to create IPAM")
				return err
			}
			defer ipam.Close()
			ip, err := ipam.AcquireIP(ctx, virtualmachine.Name, virtualmachine.Namespace)
			if err != nil {
				log.Error(err, "fail to acquire IP")
				return err
			}
			virtualmachine.Status.ExtraNetIP = ip.IP.String()
			virtualmachine.Status.ExtraNetMask = fmt.Sprintf("%d.%d.%d.%d", ip.Mask[0], ip.Mask[1], ip.Mask[2], ip.Mask[3])
			message := fmt.Sprintf("Acquired IP %s for overlay network interface", ip.String())
			log.Info(message)
			r.Recorder.Event(virtualmachine, "Normal", "OverlayNet", message)
		}
		// VirtualMachine just created, change Phase to "Pending"
		virtualmachine.Status.Phase = vmv1.VmPending
	case vmv1.VmPending:
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
			log.Info("Runner Pod was created", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)

			r.Recorder.Event(virtualmachine, "Normal", "Created",
				fmt.Sprintf("VirtualMachine %s created, pod %s",
					virtualmachine.Name, pod.Name))
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
				fmt.Sprintf("runner pod %s not found",
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

		// Update the metadata (including "usage" annotation) before anything else, so that it
		// will be correctly set even if the rest of the reconcile operation fails.
		if err := updatePodMetadataIfNecessary(ctx, r.Client, virtualmachine, vmRunner); err != nil {
			log.Error(err, "Failed to sync pod labels and annotations", "VirtualMachine", virtualmachine.Name)
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

			// get CPU details from QEMU and update status
			cpuSlotsPlugged, _, err := QmpGetCpus(virtualmachine)
			if err != nil {
				log.Error(err, "Failed to get CPU details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
				return err
			}
			specCPU := virtualmachine.Spec.Guest.CPUs.Use
			pluggedCPU := uint32(len(cpuSlotsPlugged))
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
			var targetCPUUsage vmv1.MilliCPU
			if specCPU.RoundedUp() == pluggedCPU {
				targetCPUUsage = *specCPU
			} else {
				targetCPUUsage = vmv1.MilliCPU(1000 * pluggedCPU)
			}

			if supportsCgroup && targetCPUUsage != cgroupUsage.VCPUs {
				if err := notifyRunner(ctx, virtualmachine, targetCPUUsage); err != nil {
					return err
				}
			}

			if virtualmachine.Status.CPUs == nil || *virtualmachine.Status.CPUs != targetCPUUsage {
				virtualmachine.Status.CPUs = &targetCPUUsage
				// record event about cpus used in VM
				r.Recorder.Event(virtualmachine, "Normal", "CpuInfo",
					fmt.Sprintf("VirtualMachine %s uses %v cpu cores",
						virtualmachine.Name,
						virtualmachine.Status.CPUs))
			}

			// get Memory details from hypervisor and update VM status
			memorySize, err := QmpGetMemorySize(virtualmachine)
			if err != nil {
				log.Error(err, "Failed to get Memory details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
				return err
			}
			// update status by Memory sizes used in VM
			if virtualmachine.Status.MemorySize == nil || !memorySize.Equal(*virtualmachine.Status.MemorySize) {
				virtualmachine.Status.MemorySize = memorySize
				r.Recorder.Event(virtualmachine, "Normal", "MemoryInfo",
					fmt.Sprintf("VirtualMachine %s uses %v memory",
						virtualmachine.Name,
						virtualmachine.Status.MemorySize))
			}

			// check if need hotplug/unplug CPU or memory
			// compare guest spec and count of plugged
			if virtualmachine.Spec.Guest.CPUs.Use.RoundedUp() != pluggedCPU {
				log.Info("VM goes into scaling mode, CPU count needs to be changed",
					"CPUs on board", pluggedCPU,
					"CPUs in spec", virtualmachine.Spec.Guest.CPUs.Use.RoundedUp())
				virtualmachine.Status.Phase = vmv1.VmScaling
			}
			memorySizeFromSpec := resource.NewQuantity(int64(*virtualmachine.Spec.Guest.MemorySlots.Use)*virtualmachine.Spec.Guest.MemorySlotSize.Value(), resource.BinarySI)
			if !memorySize.Equal(*memorySizeFromSpec) {
				log.Info("VM goes into scale mode, need to resize Memory",
					"Memory on board", memorySize,
					"Memory in spec", memorySizeFromSpec)
				virtualmachine.Status.Phase = vmv1.VmScaling
			}

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

	case vmv1.VmScaling:
		cpuScaled := false
		ramScaled := false

		// do hotplug/unplug CPU
		// firstly get current state from QEMU
		cpuSlotsPlugged, _, err := QmpGetCpus(virtualmachine)
		if err != nil {
			log.Error(err, "Failed to get CPU details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
			return err
		}
		specCPU := virtualmachine.Spec.Guest.CPUs.Use.RoundedUp()
		pluggedCPU := uint32(len(cpuSlotsPlugged))
		// compare guest spec and count of plugged
		if specCPU > pluggedCPU {
			// going to plug one CPU
			log.Info("Plug one more CPU into VM")
			if err := QmpPlugCpu(virtualmachine); err != nil {
				return err
			}
			r.Recorder.Event(virtualmachine, "Normal", "ScaleUp",
				fmt.Sprintf("One more CPU was plugged into VM %s",
					virtualmachine.Name))
		} else if specCPU < pluggedCPU {
			// going to unplug one CPU
			log.Info("Unplug one CPU from VM")
			if err := QmpUnplugCpu(virtualmachine); err != nil {
				return err
			}
			r.Recorder.Event(virtualmachine, "Normal", "ScaleDown",
				fmt.Sprintf("One CPU was unplugged from VM %s",
					virtualmachine.Name))
		} else {
			// seems already plugged correctly
			cpuScaled = true
		}

		// do hotplug/unplug Memory
		// firstly get current state from QEMU
		memoryDevices, err := QmpQueryMemoryDevices(virtualmachine)
		memoryPluggedSlots := *virtualmachine.Spec.Guest.MemorySlots.Min + int32(len(memoryDevices))
		if err != nil {
			log.Error(err, "Failed to get Memory details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
			return err
		}
		// compare guest spec and count of plugged
		if *virtualmachine.Spec.Guest.MemorySlots.Use > memoryPluggedSlots {
			// going to plug one Memory Slot
			log.Info("Plug one more Memory module into VM")
			if err := QmpPlugMemory(virtualmachine); err != nil {
				return err
			}
			r.Recorder.Event(virtualmachine, "Normal", "ScaleUp",
				fmt.Sprintf("One more DIMM was plugged into VM %s",
					virtualmachine.Name))
		} else if *virtualmachine.Spec.Guest.MemorySlots.Use < memoryPluggedSlots {
			// going to unplug one Memory Slot
			log.Info("Unplug one Memory module from VM")
			if err := QmpUnplugMemory(virtualmachine); err != nil {
				// special case !
				// error means VM hadn't memory devices available for unplug
				// need set .memorySlots.Use back to real value
				log.Info("All memory devices busy, unable to unplug any, will modify .spec.guest.memorySlots.use instead", "details", err)
				// firstly re-fetch VM
				if err := r.Get(ctx, types.NamespacedName{Name: virtualmachine.Name, Namespace: virtualmachine.Namespace}, virtualmachine); err != nil {
					log.Error(err, "Unable to re-fetch VirtualMachine")
					return err
				}
				memorySlotsUseInSpec := *virtualmachine.Spec.Guest.MemorySlots.Use
				virtualmachine.Spec.Guest.MemorySlots.Use = &memoryPluggedSlots
				if err := r.tryUpdateVM(ctx, virtualmachine); err != nil {
					log.Error(err,
						"Failed to update .spec.guest.memorySlots.use",
						"old value", memorySlotsUseInSpec,
						"new value", memoryPluggedSlots)
					return err
				}
				r.Recorder.Event(virtualmachine, "Warning", "ScaleDown",
					fmt.Sprintf("Unable unplug DIMM from VM %s, all memory devices are busy",
						virtualmachine.Name))
			} else {
				r.Recorder.Event(virtualmachine, "Normal", "ScaleDown",
					fmt.Sprintf("One DIMM was unplugged from VM %s",
						virtualmachine.Name))
			}
		} else {
			// seems already plugged correctly
			ramScaled = true
		}

		// set VM phase to running if everything scaled
		if cpuScaled && ramScaled {
			virtualmachine.Status.Phase = vmv1.VmRunning
		}

	case vmv1.VmSucceeded, vmv1.VmFailed:
		switch virtualmachine.Spec.RestartPolicy {
		case vmv1.RestartPolicyAlways:
			log.Info("Restarting VM runner pod", "VM.Phase", virtualmachine.Status.Phase, "RestartPolicy", virtualmachine.Spec.RestartPolicy)
			// get runner to delete
			vmRunner := &corev1.Pod{}
			err := r.Get(ctx, types.NamespacedName{Name: virtualmachine.Status.PodName, Namespace: virtualmachine.Namespace}, vmRunner)
			if err == nil {
				// delete current runner
				if err = r.Delete(ctx, vmRunner); err != nil {
					return err
				}
				r.Recorder.Event(virtualmachine, "Normal", "Deleted",
					fmt.Sprintf("VM runner pod was deleted: %s", vmRunner.Name))
				log.Info("VM runner pod was deleted", "Pod.Namespace", vmRunner.Namespace, "Pod.Name", vmRunner.Name)
			} else if !apierrors.IsNotFound(err) {
				return err
			}
			// do cleanup
			virtualmachine.Cleanup()
		case vmv1.RestartPolicyOnFailure:
			log.Info("Restarting VM runner pod", "VM.Phase", virtualmachine.Status.Phase, "RestartPolicy", virtualmachine.Spec.RestartPolicy)
			// get runner to delete
			found := true
			vmRunner := &corev1.Pod{}
			err := r.Get(ctx, types.NamespacedName{Name: virtualmachine.Status.PodName, Namespace: virtualmachine.Namespace}, vmRunner)
			if apierrors.IsNotFound(err) {
				found = false
			} else if err != nil {
				return err
			}
			// delete runner only when VM failed
			if found && virtualmachine.Status.Phase == vmv1.VmFailed {
				// delete current runner
				if err = r.Delete(ctx, vmRunner); err != nil {
					return err
				}
				r.Recorder.Event(virtualmachine, "Normal", "Deleted",
					fmt.Sprintf("VM runner pod was deleted: %s", vmRunner.Name))
				log.Info("VM runner pod was deleted", "Pod.Namespace", vmRunner.Namespace, "Pod.Name", vmRunner.Name)
			}
			// do cleanup when VM failed OR pod not found (deleted manually?) when VM succeeded
			if virtualmachine.Status.Phase == vmv1.VmFailed || (!found && virtualmachine.Status.Phase == vmv1.VmSucceeded) {
				virtualmachine.Cleanup()
			}
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

// updates the values of the runner pod's labels and annotations so that they are exactly equal to
// the set of labels/annotations we expect - minus some that are ignored.
//
// The reason we also need to delete unrecongized labels/annotations is so that if a
// label/annotation on the VM itself is deleted, we can accurately reflect that in the pod.
func updatePodMetadataIfNecessary(ctx context.Context, c client.Client, vm *vmv1.VirtualMachine, runnerPod *corev1.Pod) error {
	log := log.FromContext(ctx)

	var patches []util.JSONPatch

	metaSpecs := []struct {
		metaField   string
		expected    map[string]string
		actual      map[string]string
		ignoreExtra map[string]bool // use bool here so `if ignoreExtra[key] { ... }` works
	}{
		{
			metaField:   "labels",
			expected:    labelsForVirtualMachine(vm),
			actual:      runnerPod.Labels,
			ignoreExtra: map[string]bool{},
		},
		{
			metaField: "annotations",
			expected:  annotationsForVirtualMachine(vm),
			actual:    runnerPod.Annotations,
			ignoreExtra: map[string]bool{
				"kubectl.kubernetes.io/default-container": true,
				"k8s.v1.cni.cncf.io/networks":             true,
				"k8s.v1.cni.cncf.io/network-status":       true,
				"k8s.v1.cni.cncf.io/networks-status":      true,
			},
		},
	}

	var removedMessageParts []string

	for _, spec := range metaSpecs {
		// Add/update the entries we're expecting to be there
		for k, e := range spec.expected {
			if a, ok := spec.actual[k]; !ok || e != a {
				patches = append(patches, util.JSONPatch{
					// From RFC 6902 (JSON patch):
					//
					// > The "add" operation performs one of the following functions, depending upon
					// > what the target location references:
					// >
					// > [ ... ]
					// >
					// > * If the target location specifies an object member that does not already
					// >   exist, a new member is added to the object.
					// > * If the target location specifies an object member that does exist, that
					// >   member's value is replaced.
					//
					// So: if the value is missing we'll add it. And if it's different, we'll replace it.
					Op:    util.PatchAdd,
					Path:  fmt.Sprintf("/metadata/%s/%s", spec.metaField, util.PatchPathEscape(k)),
					Value: e,
				})
			}
		}

		// Remove the entries we aren't expecting to be there
		var removed []string
		for k := range spec.actual {
			if _, expected := spec.expected[k]; !expected && !spec.ignoreExtra[k] {
				removed = append(removed, k)
				patches = append(patches, util.JSONPatch{
					Op:   util.PatchRemove,
					Path: fmt.Sprintf("/metadata/%s/%s", spec.metaField, util.PatchPathEscape(k)),
				})
			}
		}

		if len(removed) != 0 {
			// note: formatting with %q for a []string will print the array normally, but escape the
			// strings inside. For example:
			//
			//   fmt.Printf("%q\n", []string{"foo", "bar", "escaped\nstring"})
			//
			// outputs:
			//
			//   ["foo" "bar" "escaped\nstring"]
			//
			// So the "message part" might look like `labels ["foo" "test-label"]`
			removedMessageParts = append(removedMessageParts, fmt.Sprintf("%s %q", spec.metaField, removed))
		}
	}

	if len(patches) == 0 {
		return nil
	}

	patchData, err := json.Marshal(patches)
	if err != nil {
		panic(fmt.Errorf("error marshalling JSON patch: %w", err))
	}

	if len(removedMessageParts) != 0 {
		var msg string

		if len(removedMessageParts) == 1 {
			msg = fmt.Sprintf("removing runner pod %s", removedMessageParts[0])
		} else /* len = 2 */ {
			msg = fmt.Sprintf("removing runner pod %s and %s", removedMessageParts[0], removedMessageParts[1])
		}

		// We want to log something when labels/annotations are removed, because the ignoreExtra
		// values above might be incomplete, and it'd be hard to debug without an logs for the
		// change.
		log.Info(msg, "VirtualMachine", vm.Name, "Pod", runnerPod.Name)
	}

	// NOTE: We don't need to update the data in runnerPod ourselves because c.Patch will update it
	// with what we get back from the k8s API after the patch completes.
	return c.Patch(ctx, runnerPod, client.RawPatch(types.JSONPatchType, patchData))
}

func extractVirtualMachineUsageJSON(spec vmv1.VirtualMachineSpec) string {
	cpu := *spec.Guest.CPUs.Use

	memorySlots := *spec.Guest.MemorySlots.Use

	usage := vmv1.VirtualMachineUsage{
		CPU:    cpu.ToResourceQuantity(),
		Memory: resource.NewQuantity(spec.Guest.MemorySlotSize.Value()*int64(memorySlots), resource.BinarySI),
	}

	usageJSON, err := json.Marshal(usage)
	if err != nil {
		panic(fmt.Errorf("error marshalling JSON: %w", err))
	}

	return string(usageJSON)
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
	l := make(map[string]string, len(virtualmachine.Labels)+3)
	for k, v := range virtualmachine.Labels {
		l[k] = v
	}

	l["app.kubernetes.io/name"] = "NeonVM"
	l[vmv1.VirtualMachineNameLabel] = virtualmachine.Name
	l[vmv1.RunnerPodVersionLabel] = fmt.Sprintf("%d", api.RunnerProtoV1)
	return l
}

func annotationsForVirtualMachine(virtualmachine *vmv1.VirtualMachine) map[string]string {
	// use bool here so `if ignored[key] { ... }` works
	ignored := map[string]bool{
		"kubectl.kubernetes.io/last-applied-configuration": true,
	}

	a := make(map[string]string, len(virtualmachine.Annotations)+1)
	for k, v := range virtualmachine.Annotations {
		if !ignored[k] {
			a[k] = v
		}
	}

	a[vmv1.VirtualMachineUsageAnnotation] = extractVirtualMachineUsageJSON(virtualmachine.Spec)
	return a
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

	// if NodeSelectorTerms list is empty - add default values (arch==amd64 or os==linux)
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

func notifyRunner(ctx context.Context, vm *vmv1.VirtualMachine, cpu vmv1.MilliCPU) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/cpu_change", vm.Status.PodIP, vm.Spec.RunnerPort)

	update := api.VCPUChange{VCPUs: cpu}

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
	annotations := annotationsForVirtualMachine(virtualmachine)
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
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			EnableServiceLinks:            virtualmachine.Spec.ServiceLinks,
			AutomountServiceAccountToken:  &[]bool{false}[0],
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: virtualmachine.Spec.TerminationGracePeriodSeconds,
			NodeSelector:                  virtualmachine.Spec.NodeSelector,
			ImagePullSecrets:              virtualmachine.Spec.ImagePullSecrets,
			Tolerations:                   virtualmachine.Spec.Tolerations,
			SchedulerName:                 virtualmachine.Spec.SchedulerName,
			Affinity:                      affinity,
			InitContainers: []corev1.Container{
				{
					Image:           virtualmachine.Spec.Guest.RootDisk.Image,
					Name:            "init-rootdisk",
					ImagePullPolicy: virtualmachine.Spec.Guest.RootDisk.ImagePullPolicy,
					Args:            []string{"cp", "/disk.qcow2", "/vm/images/rootdisk.qcow2"},
					VolumeMounts: []corev1.VolumeMount{{
						Name:      "virtualmachineimages",
						MountPath: "/vm/images",
					}},
					SecurityContext: &corev1.SecurityContext{
						// uid=36(qemu) gid=34(kvm) groups=34(kvm)
						RunAsUser:  &[]int64{36}[0],
						RunAsGroup: &[]int64{34}[0],
					},
				},
				{
					Image:   image,
					Name:    "sysctl",
					Command: []string{"sysctl", "-w", "net.ipv4.ip_forward=1"},
					SecurityContext: &corev1.SecurityContext{
						Privileged: &[]bool{true}[0],
					},
				},
			},
			Containers: []corev1.Container{{
				Image:           image,
				Name:            "neonvm-runner",
				ImagePullPolicy: corev1.PullIfNotPresent,
				// Ensure restrictive context for the container
				// More info: https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted
				SecurityContext: &corev1.SecurityContext{
					Privileged: &[]bool{false}[0],
					Capabilities: &corev1.Capabilities{
						Add: []corev1.Capability{
							"NET_ADMIN",
							"SYS_ADMIN",
							"SYS_RESOURCE",
						},
					},
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
				Env: []corev1.EnvVar{{
					Name: "K8S_POD_NAME",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.name",
						},
					},
				}},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "virtualmachineimages",
						MountPath: "/vm/images",
					},
					{
						Name:      "sysfscgroup",
						MountPath: "/sys/fs/cgroup",
						// MountPropagationNone means that the volume in a container will
						// not receive new mounts from the host or other containers, and filesystems
						// mounted inside the container won't be propagated to the host or other
						// containers.
						// Note that this mode corresponds to "private" in Linux terminology.
						MountPropagation: &[]corev1.MountPropagationMode{corev1.MountPropagationNone}[0],
					},
				},
				Resources: virtualmachine.Spec.PodResources,
			}},
			Volumes: []corev1.Volume{
				{
					Name: "virtualmachineimages",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "sysfscgroup",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/sys/fs/cgroup",
							Type: &[]corev1.HostPathType{corev1.HostPathDirectory}[0],
						},
					},
				},
			},
		},
	}

	// allow access to /dev/kvm and /dev/vhost-net devices by generic-device-plugin for kubelet
	if pod.Spec.Containers[0].Resources.Limits == nil {
		pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{}
	}
	pod.Spec.Containers[0].Resources.Limits["neonvm/vhost-net"] = resource.MustParse("1")
	if virtualmachine.Spec.EnableAcceleration {
		pod.Spec.Containers[0].Resources.Limits["neonvm/kvm"] = resource.MustParse("1")
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

	if pod.ObjectMeta.Annotations == nil {
		pod.ObjectMeta.Annotations = map[string]string{}
	}
	pod.ObjectMeta.Annotations["kubectl.kubernetes.io/default-container"] = "neonvm-runner"

	// use multus network to add extra network interface
	if virtualmachine.Spec.ExtraNetwork != nil && virtualmachine.Spec.ExtraNetwork.Enable {
		var nadNetwork string
		if len(virtualmachine.Spec.ExtraNetwork.MultusNetwork) > 0 { // network specified in spec
			nadNetwork = virtualmachine.Spec.ExtraNetwork.MultusNetwork
		} else { // get network from env variables
			nadName, err := nadRunnerName()
			if err != nil {
				return nil, err
			}
			nadNamespace, err := nadRunnerNamespace()
			if err != nil {
				return nil, err
			}
			nadNetwork = fmt.Sprintf("%s/%s", nadNamespace, nadName)
		}
		pod.ObjectMeta.Annotations[nadapiv1.NetworkAttachmentAnnot] = fmt.Sprintf("%s@%s", nadNetwork, virtualmachine.Spec.ExtraNetwork.Interface)
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
		WithOptions(controller.Options{MaxConcurrentReconciles: 8}).
		Complete(r)
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

// TODO: reimplement to r.Patch()
func (r *VirtualMachineReconciler) tryUpdateVM(ctx context.Context, virtualmachine *vmv1.VirtualMachine) error {
	return r.Update(ctx, virtualmachine)
}

// return Netwrok Attachment Definition name with IPAM settings
func nadIpamName() (string, error) {
	return getEnvVarValue("NAD_IPAM_NAME")
}

// return Netwrok Attachment Definition namespace with IPAM settings
func nadIpamNamespace() (string, error) {
	return getEnvVarValue("NAD_IPAM_NAMESPACE")
}

// return Netwrok Attachment Definition name for second interface in Runner
func nadRunnerName() (string, error) {
	return getEnvVarValue("NAD_RUNNER_NAME")
}

// return Netwrok Attachment Definition namespace for second interface in Runner
func nadRunnerNamespace() (string, error) {
	return getEnvVarValue("NAD_RUNNER_NAMESPACE")
}

// return env variable value
func getEnvVarValue(envVarName string) (string, error) {
	value, found := os.LookupEnv(envVarName)
	if !found {
		return "", fmt.Errorf("unable to find %s environment variable", envVarName)
	}
	return value, nil
}
