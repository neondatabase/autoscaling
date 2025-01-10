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
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"reflect"
	sysruntime "runtime"
	"strconv"
	"time"

	nadapiv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/samber/lo"
	"golang.org/x/crypto/ssh"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/client-go/tools/record"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/neonvm/controllers/buildtag"
	"github.com/neondatabase/autoscaling/pkg/neonvm/ipam"
	"github.com/neondatabase/autoscaling/pkg/util/patch"
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

const (
	minSupportedRunnerVersion api.RunnerProtoVersion = api.RunnerProtoV1
	maxSupportedRunnerVersion api.RunnerProtoVersion = api.RunnerProtoV1
)

// VMReconciler reconciles a VirtualMachine object
type VMReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Config   *ReconcilerConfig

	Metrics ReconcilerMetrics `exhaustruct:"optional"`
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when controller-gen (used by 'make generate') is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachines/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
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
func (r *VMReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var vm vmv1.VirtualMachine
	if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
		// Error reading the object - requeue the request.
		if notfound := client.IgnoreNotFound(err); notfound == nil {
			log.Info("virtualmachine resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Unable to fetch VirtualMachine")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if vm.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(&vm, virtualmachineFinalizer) {
			log.Info("Adding Finalizer for VirtualMachine")
			if ok := controllerutil.AddFinalizer(&vm, virtualmachineFinalizer); !ok {
				log.Info("Failed to add finalizer from VirtualMachine")
				return ctrl.Result{Requeue: true}, nil
			}
			if err := r.tryUpdateVM(ctx, &vm); err != nil {
				log.Error(err, "Failed to update status about adding finalizer to VirtualMachine")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(&vm, virtualmachineFinalizer) {
			// our finalizer is present, so lets handle any external dependency
			log.Info("Performing Finalizer Operations for VirtualMachine before delete it")
			r.doFinalizerOperationsForVirtualMachine(ctx, &vm)

			// remove our finalizer from the list and update it.
			log.Info("Removing Finalizer for VirtualMachine after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(&vm, virtualmachineFinalizer); !ok {
				log.Info("Failed to remove finalizer from VirtualMachine")
				return ctrl.Result{Requeue: true}, nil
			}
			if err := r.tryUpdateVM(ctx, &vm); err != nil {
				log.Error(err, "Failed to update status about removing finalizer from VirtualMachine")
				return ctrl.Result{}, err
			}
		}
		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	// examine cpuScalingMode and set it to the default value if it is not set
	if vm.Spec.CpuScalingMode == nil {
		log.Info("Setting default CPU scaling mode", "default", r.Config.DefaultCPUScalingMode)
		vm.Spec.CpuScalingMode = lo.ToPtr(r.Config.DefaultCPUScalingMode)
		if err := r.tryUpdateVM(ctx, &vm); err != nil {
			log.Error(err, "Failed to set default CPU scaling mode")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	statusBefore := vm.Status.DeepCopy()
	if err := r.doReconcile(ctx, &vm); err != nil {
		r.Recorder.Eventf(&vm, corev1.EventTypeWarning, "Failed",
			"Failed to reconcile (%s): %s", vm.Name, err)
		return ctrl.Result{}, err
	}

	// If the status changed, try to update the object
	if !DeepEqual(statusBefore, vm.Status) {
		if err := r.Status().Update(ctx, &vm); err != nil {
			log.Error(err, "Failed to update VirtualMachine status after reconcile loop",
				"virtualmachine", vm.Name)
			return ctrl.Result{}, err
		}
	}

	// Only quickly requeue if we're scaling or migrating. Otherwise, we aren't expecting any
	// changes from QEMU, and it's wasteful to repeatedly check.
	requeueAfter := time.Second
	if vm.Status.Phase == vmv1.VmPending || vm.Status.Phase == vmv1.VmRunning {
		requeueAfter = 15 * time.Second
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// doFinalizerOperationsForVirtualMachine will perform the required operations before delete the CR.
func (r *VMReconciler) doFinalizerOperationsForVirtualMachine(ctx context.Context, vm *vmv1.VirtualMachine) {
	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These ones, such as the Pod created on this reconcile,
	// are defined as depended of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	log := log.FromContext(ctx)

	// The following implementation will raise an event
	r.Recorder.Event(vm, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			vm.Name,
			vm.Namespace))

	// Release overlay IP address
	if vm.Spec.ExtraNetwork != nil {
		// Create IPAM object
		nadName, err := nadIpamName()
		if err != nil {
			// ignore error
			log.Error(err, "ignored error")
			return
		}
		nadNamespace, err := nadIpamNamespace()
		if err != nil {
			// ignore error
			log.Error(err, "ignored error")
			return
		}
		ipam, err := ipam.New(ctx, nadName, nadNamespace)
		if err != nil {
			// ignore error
			log.Error(err, "ignored error")
			return
		}
		defer ipam.Close()
		ip, err := ipam.ReleaseIP(ctx, vm.Name, vm.Namespace)
		if err != nil {
			// ignore error
			log.Error(err, "fail to release IP, error ignored")
			return
		}
		message := fmt.Sprintf("Released IP %s", ip.String())
		log.Info(message)
		r.Recorder.Event(vm, "Normal", "OverlayNet", message)
	}
}

func getRunnerVersion(pod *corev1.Pod) (api.RunnerProtoVersion, error) {
	val, ok := pod.Labels[vmv1.RunnerPodVersionLabel]
	if !ok {
		return api.RunnerProtoVersion(0), nil
	}

	uintVal, err := strconv.ParseUint(val, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to parse label value as integer: %w", err)
	}

	return api.RunnerProtoVersion(uintVal), nil
}

func runnerVersionIsSupported(version api.RunnerProtoVersion) bool {
	return version >= minSupportedRunnerVersion && version <= maxSupportedRunnerVersion
}

func (r *VMReconciler) updateVMStatusCPU(
	ctx context.Context,
	vm *vmv1.VirtualMachine,
	vmRunner *corev1.Pod,
	activeCPUs uint32,
	cgroupUsage *api.VCPUCgroup,
) {
	log := log.FromContext(ctx)

	// We expect:
	// - vm.Status.CPUs = cgroupUsage.VCPUs
	// - vm.Status.CPUs.RoundUp() == activeCPUs
	// Otherwise, we update the status.
	var currentCPUUsage vmv1.MilliCPU
	if cgroupUsage != nil {
		if cgroupUsage.VCPUs.RoundedUp() != activeCPUs {
			// This is not expected but it's fine. We only report the
			// mismatch here and will resolve it in the next reconcile
			// iteration loops by comparing these values to spec CPU use
			// and moving to the scaling phase.
			log.Error(nil, "Mismatch in the number of VM's plugged CPUs and runner pod's cgroup vCPUs",
				"VirtualMachine", vm.Name,
				"Runner Pod", vmRunner.Name,
				"plugged CPUs", activeCPUs,
				"cgroup vCPUs", cgroupUsage.VCPUs)
		}
		currentCPUUsage = min(cgroupUsage.VCPUs, vmv1.MilliCPU(1000*activeCPUs))
	} else {
		currentCPUUsage = vmv1.MilliCPU(1000 * activeCPUs)
	}
	if vm.Status.CPUs == nil || *vm.Status.CPUs != currentCPUUsage {
		vm.Status.CPUs = &currentCPUUsage
		r.Recorder.Event(vm, "Normal", "CpuInfo",
			fmt.Sprintf("VirtualMachine %s uses %v cpu cores",
				vm.Name,
				vm.Status.CPUs))
	}
}

func (r *VMReconciler) updateVMStatusMemory(
	vm *vmv1.VirtualMachine,
	qmpMemorySize *resource.Quantity,
) {
	if vm.Status.MemorySize == nil || !qmpMemorySize.Equal(*vm.Status.MemorySize) {
		vm.Status.MemorySize = qmpMemorySize
		r.Recorder.Event(vm, "Normal", "MemoryInfo",
			fmt.Sprintf("VirtualMachine %s uses %v memory",
				vm.Name,
				vm.Status.MemorySize))
	}
}

func (r *VMReconciler) acquireOverlayIP(ctx context.Context, vm *vmv1.VirtualMachine) error {
	if vm.Spec.ExtraNetwork == nil || !vm.Spec.ExtraNetwork.Enable || len(vm.Status.ExtraNetIP) != 0 {
		// If the VM has extra network disabled or already has an IP, do nothing.
		return nil
	}

	log := log.FromContext(ctx)

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
	ip, err := ipam.AcquireIP(ctx, vm.Name, vm.Namespace)
	if err != nil {
		log.Error(err, "fail to acquire IP")
		return err
	}
	message := fmt.Sprintf("Acquired IP %s for overlay network interface", ip.String())
	log.Info(message)
	vm.Status.ExtraNetIP = ip.IP.String()
	vm.Status.ExtraNetMask = fmt.Sprintf("%d.%d.%d.%d", ip.Mask[0], ip.Mask[1], ip.Mask[2], ip.Mask[3])
	r.Recorder.Event(vm, "Normal", "OverlayNet", message)
	return nil
}

func (r *VMReconciler) doReconcile(ctx context.Context, vm *vmv1.VirtualMachine) error {
	log := log.FromContext(ctx)

	// Let's check and just set the condition status as Unknown when no status are available
	if len(vm.Status.Conditions) == 0 {
		// set Unknown condition status for AvailableVirtualMachine
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{Type: typeAvailableVirtualMachine, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
	}

	// NB: .Spec.EnableSSH guaranteed non-nil because the k8s API server sets the default for us.
	enableSSH := *vm.Spec.EnableSSH

	// Generate ssh secret name
	if enableSSH && len(vm.Status.SSHSecretName) == 0 {
		vm.Status.SSHSecretName = fmt.Sprintf("ssh-neonvm-%s", vm.Name)
	}

	// Forwards compatibility: Set virtio-mem memory provider for VMs that don't have it in the
	// status.
	// We need to do this so that when we switch to no longer setting memoryProvider, we won't
	// regress and try to set it to dimm slots.
	// See #1060 for the relevant tracking issue.
	if vm.Status.PodName != "" && vm.Status.MemoryProvider == nil {
		newMemProvider := vmv1.MemoryProviderVirtioMem
		log.Error(nil, "Setting default MemoryProvider for VM", "MemoryProvider", newMemProvider)
		vm.Status.MemoryProvider = lo.ToPtr(newMemProvider)
	}

	switch vm.Status.Phase {

	case "":
		if err := r.acquireOverlayIP(ctx, vm); err != nil {
			return err
		}
		// VirtualMachine just created, change Phase to "Pending"
		vm.Status.Phase = vmv1.VmPending
	case vmv1.VmPending:
		// Generate runner pod name and set desired memory provider.
		// Together with Status.MemoryProvider set for PodName != "" above,
		// It is now guaranteed to have Status.MemoryProvider != nil
		if len(vm.Status.PodName) == 0 {
			vm.Status.PodName = names.SimpleNameGenerator.GenerateName(fmt.Sprintf("%s-", vm.Name))
			if vm.Status.MemoryProvider == nil {
				vm.Status.MemoryProvider = lo.ToPtr(pickMemoryProvider(r.Config, vm))
			}
			// Update the .Status on API Server to avoid creating multiple pods for a single VM
			// See https://github.com/neondatabase/autoscaling/issues/794 for the context
			if err := r.Status().Update(ctx, vm); err != nil {
				return fmt.Errorf("Failed to update VirtualMachine status: %w", err)
			}
		}

		memoryProvider := *vm.Status.MemoryProvider

		// Check if the runner pod already exists, if not create a new one
		vmRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: vm.Status.PodName, Namespace: vm.Namespace}, vmRunner)
		if err != nil && apierrors.IsNotFound(err) {
			var sshSecret *corev1.Secret
			if enableSSH {
				// Check if the ssh secret already exists, if not create a new one
				sshSecret = &corev1.Secret{}
				err := r.Get(ctx, types.NamespacedName{
					Name:      vm.Status.SSHSecretName,
					Namespace: vm.Namespace,
				}, sshSecret)
				if err != nil && apierrors.IsNotFound(err) {
					// Define a new ssh secret
					sshSecret, err = r.sshSecretForVirtualMachine(vm)
					if err != nil {
						log.Error(err, "Failed to define new SSH Secret for VirtualMachine")
						return err
					}

					log.Info("Creating a new SSH Secret", "Secret.Namespace", sshSecret.Namespace, "Secret.Name", sshSecret.Name)
					if err = r.Create(ctx, sshSecret); err != nil {
						log.Error(err, "Failed to create new SSH secret", "Secret.Namespace", sshSecret.Namespace, "Secret.Name", sshSecret.Name)
						return err
					}
					log.Info("SSH Secret was created", "Secret.Namespace", sshSecret.Namespace, "Secret.Name", sshSecret.Name)
				} else if err != nil {
					log.Error(err, "Failed to get SSH Secret")
					return err
				}
			}

			// Define a new pod
			pod, err := r.podForVirtualMachine(vm, memoryProvider, sshSecret)
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

			msg := fmt.Sprintf("VirtualMachine %s created, Pod %s", vm.Name, pod.Name)
			if sshSecret != nil {
				msg = fmt.Sprintf("%s, SSH Secret %s", msg, sshSecret.Name)
			}
			r.Recorder.Event(vm, "Normal", "Created", msg)
			if !vm.HasRestarted() {
				d := pod.CreationTimestamp.Time.Sub(vm.CreationTimestamp.Time)
				r.Metrics.vmCreationToRunnerCreationTime.Observe(d.Seconds())
			}
		} else if err != nil {
			log.Error(err, "Failed to get vm-runner Pod")
			return err
		}

		// Update the metadata (including "usage" annotation) before anything else, so that it
		// will be correctly set even if the rest of the reconcile operation fails.
		if err := updatePodMetadataIfNecessary(ctx, r.Client, vm, vmRunner); err != nil {
			log.Error(err, "Failed to sync pod labels and annotations", "VirtualMachine", vm.Name)
		}

		// runner pod found, check phase
		switch runnerStatus(vmRunner) {
		case runnerRunning:
			vm.Status.PodIP = vmRunner.Status.PodIP
			vm.Status.Phase = vmv1.VmRunning
			meta.SetStatusCondition(&vm.Status.Conditions,
				metav1.Condition{
					Type:    typeAvailableVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) created successfully", vm.Status.PodName, vm.Name),
				})
			{
				// Calculating VM startup latency metrics
				now := time.Now()
				d := now.Sub(vmRunner.CreationTimestamp.Time)
				r.Metrics.runnerCreationToVMRunningTime.Observe(d.Seconds())
				if !vm.HasRestarted() {
					d := now.Sub(vm.CreationTimestamp.Time)
					r.Metrics.vmCreationToVMRunningTime.Observe(d.Seconds())
					log.Info("VM creation to VM running time", "duration(sec)", d.Seconds())
				}
			}
		case runnerSucceeded:
			vm.Status.Phase = vmv1.VmSucceeded
			meta.SetStatusCondition(&vm.Status.Conditions,
				metav1.Condition{
					Type:    typeAvailableVirtualMachine,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) succeeded", vm.Status.PodName, vm.Name),
				})
		case runnerFailed:
			vm.Status.Phase = vmv1.VmFailed
			meta.SetStatusCondition(&vm.Status.Conditions,
				metav1.Condition{
					Type:    typeDegradedVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) failed", vm.Status.PodName, vm.Name),
				})
		default:
			// do nothing
		}
	case vmv1.VmRunning:
		// Check if the runner pod exists
		vmRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: vm.Status.PodName, Namespace: vm.Namespace}, vmRunner)
		if err != nil && apierrors.IsNotFound(err) {
			// lost runner pod for running VirtualMachine ?
			r.Recorder.Event(vm, "Warning", "NotFound",
				fmt.Sprintf("runner pod %s not found",
					vm.Status.PodName))
			vm.Status.Phase = vmv1.VmFailed
			meta.SetStatusCondition(&vm.Status.Conditions,
				metav1.Condition{
					Type:    typeDegradedVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) not found", vm.Status.PodName, vm.Name),
				})
		} else if err != nil {
			log.Error(err, "Failed to get runner Pod")
			return err
		}

		// Update the metadata (including "usage" annotation) before anything else, so that it
		// will be correctly set even if the rest of the reconcile operation fails.
		if err := updatePodMetadataIfNecessary(ctx, r.Client, vm, vmRunner); err != nil {
			log.Error(err, "Failed to sync pod labels and annotations", "VirtualMachine", vm.Name)
		}

		// runner pod found, check/update phase now
		switch runnerStatus(vmRunner) {
		case runnerRunning:
			// update status by IP of runner pod
			vm.Status.PodIP = vmRunner.Status.PodIP
			// update phase
			vm.Status.Phase = vmv1.VmRunning
			// update Node name where runner working
			vm.Status.Node = vmRunner.Spec.NodeName

			runnerVersion, err := getRunnerVersion(vmRunner)
			if err != nil {
				log.Error(err, "Failed to get runner version of VM runner pod", "VirtualMachine", vm.Name)
				return err
			}
			if !runnerVersionIsSupported(runnerVersion) {
				err := fmt.Errorf("runner version %v is not supported", runnerVersion)
				log.Error(err, "VM runner pod has unsupported version", "VirtualMachine", vm.Name)
				return err
			}

			// get cgroups CPU details from runner pod
			cgroupUsage, err := getRunnerCPULimits(ctx, vm)
			if err != nil {
				log.Error(err, "Failed to get CPU details from runner", "VirtualMachine", vm.Name)
				return err
			}
			var pluggedCPU uint32

			if vm.Spec.CpuScalingMode == nil { // should not happen
				err := fmt.Errorf("CPU scaling mode is not set")
				log.Error(err, "Unknown CPU scaling mode", "VirtualMachine", vm.Name)
				return err
			}

			switch *vm.Spec.CpuScalingMode {
			case vmv1.CpuScalingModeSysfs:
				pluggedCPU = cgroupUsage.VCPUs.RoundedUp()
			case vmv1.CpuScalingModeQMP:
				cpuSlotsPlugged, _, err := QmpGetCpus(QmpAddr(vm))
				if err != nil {
					log.Error(err, "Failed to get CPU details from VirtualMachine", "VirtualMachine", vm.Name)
					return err
				}
				pluggedCPU = uint32(len(cpuSlotsPlugged))
			default:
				err := fmt.Errorf("unsupported CPU scaling mode: %s", *vm.Spec.CpuScalingMode)
				log.Error(err, "Unknown CPU scaling mode", "VirtualMachine", vm.Name, "CPU scaling mode", *vm.Spec.CpuScalingMode)
				return err
			}

			// update status by CPUs used in the VM
			r.updateVMStatusCPU(ctx, vm, vmRunner, pluggedCPU, cgroupUsage)

			// get Memory details from hypervisor and update VM status
			memorySize, err := QmpGetMemorySize(QmpAddr(vm))
			if err != nil {
				log.Error(err, "Failed to get Memory details from VirtualMachine", "VirtualMachine", vm.Name)
				return err
			}
			// update status by memory sizes used in the VM
			r.updateVMStatusMemory(vm, memorySize)

			// check if need hotplug/unplug CPU or memory
			// compare guest spec and count of plugged

			specUseCPU := vm.Spec.Guest.CPUs.Use
			scaleCgroupCPU := specUseCPU != cgroupUsage.VCPUs
			scaleQemuCPU := specUseCPU.RoundedUp() != pluggedCPU
			if scaleCgroupCPU || scaleQemuCPU {
				log.Info("VM goes into scaling mode, CPU count needs to be changed",
					"CPUs on runner pod cgroup", cgroupUsage.VCPUs,
					"CPUs on board", pluggedCPU,
					"CPUs in spec", vm.Spec.Guest.CPUs.Use)
				vm.Status.Phase = vmv1.VmScaling
			}

			memorySizeFromSpec := resource.NewQuantity(int64(vm.Spec.Guest.MemorySlots.Use)*vm.Spec.Guest.MemorySlotSize.Value(), resource.BinarySI)
			if !memorySize.Equal(*memorySizeFromSpec) {
				log.Info("VM goes into scale mode, need to resize Memory",
					"Memory on board", memorySize,
					"Memory in spec", memorySizeFromSpec)
				vm.Status.Phase = vmv1.VmScaling
			}

		case runnerSucceeded:
			vm.Status.Phase = vmv1.VmSucceeded
			meta.SetStatusCondition(&vm.Status.Conditions,
				metav1.Condition{
					Type:    typeAvailableVirtualMachine,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) succeeded", vm.Status.PodName, vm.Name),
				})
		case runnerFailed:
			vm.Status.Phase = vmv1.VmFailed
			meta.SetStatusCondition(&vm.Status.Conditions,
				metav1.Condition{
					Type:    typeDegradedVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) failed", vm.Status.PodName, vm.Name),
				})
		default:
			// do nothing
		}

	case vmv1.VmScaling:
		// Check that runner pod is still ok
		vmRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: vm.Status.PodName, Namespace: vm.Namespace}, vmRunner)
		if err != nil && apierrors.IsNotFound(err) {
			// lost runner pod for running VirtualMachine ?
			r.Recorder.Event(vm, "Warning", "NotFound",
				fmt.Sprintf("runner pod %s not found",
					vm.Status.PodName))
			vm.Status.Phase = vmv1.VmFailed
			meta.SetStatusCondition(&vm.Status.Conditions,
				metav1.Condition{
					Type:    typeDegradedVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) not found", vm.Status.PodName, vm.Name),
				})
		} else if err != nil {
			log.Error(err, "Failed to get runner Pod")
			return err
		}

		// Update the metadata (including "usage" annotation) before anything else, so that it
		// will be correctly set even if the rest of the reconcile operation fails.
		if err := updatePodMetadataIfNecessary(ctx, r.Client, vm, vmRunner); err != nil {
			log.Error(err, "Failed to sync pod labels and annotations", "VirtualMachine", vm.Name)
		}

		// runner pod found, check that it's still up:
		switch runnerStatus(vmRunner) {
		case runnerSucceeded:
			vm.Status.Phase = vmv1.VmSucceeded
			meta.SetStatusCondition(&vm.Status.Conditions,
				metav1.Condition{
					Type:    typeAvailableVirtualMachine,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) succeeded", vm.Status.PodName, vm.Name),
				})
			return nil
		case runnerFailed:
			vm.Status.Phase = vmv1.VmFailed
			meta.SetStatusCondition(&vm.Status.Conditions,
				metav1.Condition{
					Type:    typeDegradedVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) failed", vm.Status.PodName, vm.Name),
				})
			return nil
		default:
			// do nothing
		}

		runnerVersion, err := getRunnerVersion(vmRunner)
		if err != nil {
			log.Error(err, "Failed to get runner version of VM runner pod", "VirtualMachine", vm.Name)
			return err
		}
		if !runnerVersionIsSupported(runnerVersion) {
			err := fmt.Errorf("runner version %v is not supported", runnerVersion)
			log.Error(err, "VM runner pod has unsupported version", "VirtualMachine", vm.Name)
			return err
		}

		cpuScaled, err := r.handleCPUScaling(ctx, vm, vmRunner)
		if err != nil {
			log.Error(err, "failed to handle CPU scaling")
			return err
		}
		ramScaled := false

		// do hotplug/unplug Memory
		switch *vm.Status.MemoryProvider {
		case vmv1.MemoryProviderVirtioMem:
			ramScaled, err = r.doVirtioMemScaling(vm)
			if err != nil {
				return err
			}
		case vmv1.MemoryProviderDIMMSlots:
			ramScaled, err = r.doDIMMSlotsScaling(ctx, vm)
			if err != nil {
				return err
			}
		default:
			panic(fmt.Errorf("unexpected vm.status.memoryProvider %q", *vm.Status.MemoryProvider))
		}

		// set VM phase to running if everything scaled
		if cpuScaled && ramScaled {
			vm.Status.Phase = vmv1.VmRunning
		}

	case vmv1.VmSucceeded, vmv1.VmFailed:
		// Always delete runner pod. Otherwise, we could end up with one container succeeded/failed
		// but the other one still running (meaning that the pod still ends up Running).
		vmRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: vm.Status.PodName, Namespace: vm.Namespace}, vmRunner)
		if err == nil {
			// delete current runner
			if err := r.deleteRunnerPodIfEnabled(ctx, vm, vmRunner); err != nil {
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}

		// By default, we cleanup the VM, even if previous pod still exists. This behavior is for the case
		// where the pod is stuck deleting, and we want to progress without waiting for it.
		//
		// However, this opens up a possibility for cascading failures where the pods would be constantly
		// recreated, and then stuck deleting. That's why we have AtMostOnePod.
		if !r.Config.AtMostOnePod || apierrors.IsNotFound(err) {
			// NB: Cleanup() leaves status .Phase and .RestartCount (+ some others) but unsets other fields.
			vm.Cleanup()

			var shouldRestart bool
			switch vm.Spec.RestartPolicy {
			case vmv1.RestartPolicyAlways:
				shouldRestart = true
			case vmv1.RestartPolicyOnFailure:
				shouldRestart = vm.Status.Phase == vmv1.VmFailed
			case vmv1.RestartPolicyNever:
				shouldRestart = false
			}

			if shouldRestart {
				log.Info("Restarting VM runner pod", "VM.Phase", vm.Status.Phase, "RestartPolicy", vm.Spec.RestartPolicy)
				vm.Status.Phase = vmv1.VmPending // reset to trigger restart
				vm.Status.RestartCount += 1      // increment restart count
				r.Metrics.vmRestartCounts.Inc()
			}

			// TODO for RestartPolicyNever: implement TTL or do nothing
		}
	default:
		// do nothing
	}

	// Propagate TargetRevision to CurrentRevision. This is done only if the VM is fully
	// reconciled and running.
	if vm.Status.Phase == vmv1.VmRunning {
		propagateRevision(vm)
	}

	return nil
}

func propagateRevision(vm *vmv1.VirtualMachine) {
	if vm.Spec.TargetRevision == nil {
		return
	}
	if vm.Status.CurrentRevision != nil &&
		vm.Status.CurrentRevision.Revision == vm.Spec.TargetRevision.Revision {
		return
	}
	rev := vm.Spec.TargetRevision.WithTime(time.Now())
	vm.Status.CurrentRevision = &rev
}

func pickMemoryProvider(config *ReconcilerConfig, vm *vmv1.VirtualMachine) vmv1.MemoryProvider {
	if p := vm.Spec.Guest.MemoryProvider; p != nil {
		return *p
	}
	if p := vm.Status.MemoryProvider; p != nil {
		return *p
	}

	// Not all configurations are valid for virtio-mem. Only switch to the default as long as it
	// won't be invalid:
	if err := vm.Spec.Guest.ValidateForMemoryProvider(config.DefaultMemoryProvider); err != nil {
		return vmv1.MemoryProviderDIMMSlots
	}

	return config.DefaultMemoryProvider
}

func (r *VMReconciler) doVirtioMemScaling(vm *vmv1.VirtualMachine) (done bool, _ error) {
	targetSlotCount := int(vm.Spec.Guest.MemorySlots.Use - vm.Spec.Guest.MemorySlots.Min)

	targetVirtioMemSize := int64(targetSlotCount) * vm.Spec.Guest.MemorySlotSize.Value()
	previousTarget, err := QmpSetVirtioMem(vm, targetVirtioMemSize)
	if err != nil {
		return false, err
	}

	goalTotalSize := resource.NewQuantity(
		int64(vm.Spec.Guest.MemorySlots.Use)*vm.Spec.Guest.MemorySlotSize.Value(),
		resource.BinarySI,
	)

	if previousTarget != targetVirtioMemSize {
		// We changed the requested size. Make an event for it.
		reason := "ScaleUp"
		if targetVirtioMemSize < previousTarget {
			reason = "ScaleDown"
		}
		r.Recorder.Eventf(vm, "Normal", reason, "Set virtio-mem size for %v total memory", goalTotalSize)
	}

	// Maybe we're already using the amount we want?
	// Update the status to reflect the current size - and if it matches goalTotalSize, ram
	// scaling is done.
	currentTotalSize, err := QmpGetMemorySize(QmpAddr(vm))
	if err != nil {
		return false, err
	}

	done = currentTotalSize.Value() == goalTotalSize.Value()
	r.updateVMStatusMemory(vm, currentTotalSize)
	return done, nil
}

func (r *VMReconciler) doDIMMSlotsScaling(ctx context.Context, vm *vmv1.VirtualMachine) (done bool, _ error) {
	log := log.FromContext(ctx)

	memSlotsMin := vm.Spec.Guest.MemorySlots.Min
	targetSlotCount := int(vm.Spec.Guest.MemorySlots.Use - memSlotsMin)

	realSlots, err := QmpSetMemorySlots(ctx, vm, targetSlotCount, r.Recorder)
	if realSlots < 0 {
		return false, err
	}

	if realSlots != int(targetSlotCount) {
		log.Info("Couldn't achieve desired memory slot count, will modify .spec.guest.memorySlots.use instead", "details", err)
		// firstly re-fetch VM
		if err := r.Get(ctx, types.NamespacedName{Name: vm.Name, Namespace: vm.Namespace}, vm); err != nil {
			log.Error(err, "Unable to re-fetch VirtualMachine")
			return false, err
		}
		memorySlotsUseInSpec := vm.Spec.Guest.MemorySlots.Use
		memoryPluggedSlots := memSlotsMin + int32(realSlots)
		vm.Spec.Guest.MemorySlots.Use = memoryPluggedSlots
		if err := r.tryUpdateVM(ctx, vm); err != nil {
			log.Error(err, "Failed to update .spec.guest.memorySlots.use",
				"old value", memorySlotsUseInSpec,
				"new value", memoryPluggedSlots)
			return false, err
		}
	} else {
		done = true
	}
	// get Memory details from hypervisor and update VM status
	memorySize, err := QmpGetMemorySize(QmpAddr(vm))
	if err != nil {
		log.Error(err, "Failed to get Memory details from VirtualMachine", "VirtualMachine", vm.Name)
		return false, err
	}
	// update status by memory sizes used in the VM
	r.updateVMStatusMemory(vm, memorySize)
	return done, nil
}

type runnerStatusKind string

const (
	runnerPending   runnerStatusKind = "Pending"
	runnerRunning   runnerStatusKind = "Running"
	runnerFailed    runnerStatusKind = "Failed"
	runnerSucceeded runnerStatusKind = "Succeeded"
)

// runnerStatus returns a description of the status of the VM inside the runner pod.
//
// This is *similar* to the value of pod.Status.Phase, but we'd like to retain our own abstraction
// to have more control over the semantics.
func runnerStatus(pod *corev1.Pod) runnerStatusKind {
	// Add 5 seconds to account for clock skew and k8s lagging behind.
	deadline := metav1.NewTime(metav1.Now().Add(-5 * time.Second))

	// If the pod is being deleted, we consider it failed. The deletion might be stalled
	// because the node is shutting down, or the pod is stuck pulling an image.
	if pod.DeletionTimestamp != nil && pod.DeletionTimestamp.Before(&deadline) {
		return runnerFailed
	}
	switch pod.Status.Phase {
	case "", corev1.PodPending:
		return runnerPending
	case corev1.PodSucceeded:
		return runnerSucceeded
	case corev1.PodFailed:
		return runnerFailed
	case corev1.PodRunning:
		return isRunnerPodReady(pod)
	default:
		panic(fmt.Errorf("unknown pod phase: %q", pod.Status.Phase))
	}
}

// isRunnerPodReady returns whether the runner pod is ready respecting the readiness probe of its containers.
func isRunnerPodReady(pod *corev1.Pod) runnerStatusKind {
	if pod.Status.ContainerStatuses == nil {
		return runnerPending
	}
	for _, c := range pod.Status.ContainerStatuses {
		// we only care about the neonvm-runner container
		if c.Name == "neonvm-runner" && !c.Ready {
			return runnerPending
		}
	}
	return runnerRunning
}

// deleteRunnerPodIfEnabled deletes the runner pod if buildtag.NeverDeleteRunnerPods is false, and
// then emits an event and log line about what it did, whether it actually deleted the runner pod.
func (r *VMReconciler) deleteRunnerPodIfEnabled(
	ctx context.Context,
	vm *vmv1.VirtualMachine,
	runner *corev1.Pod,
) error {
	log := log.FromContext(ctx)
	var msg, eventReason string
	if buildtag.NeverDeleteRunnerPods {
		msg = fmt.Sprintf("VM runner pod deletion was skipped due to '%s' build tag", buildtag.TagnameNeverDeleteRunnerPods)
		eventReason = "DeleteSkipped"
	} else {
		// delete current runner
		if err := r.Delete(ctx, runner); err != nil {
			return err
		}
		msg = "VM runner pod was deleted"
		eventReason = "Deleted"
	}
	log.Info(msg, "Pod.Namespace", runner.Namespace, "Pod.Name", runner.Name)
	r.Recorder.Event(vm, "Normal", eventReason, fmt.Sprintf("%s: %s", msg, runner.Name))
	return nil
}

// updates the values of the runner pod's labels and annotations so that they are exactly equal to
// the set of labels/annotations we expect - minus some that are ignored.
//
// The reason we also need to delete unrecognized labels/annotations is so that if a
// label/annotation on the VM itself is deleted, we can accurately reflect that in the pod.
func updatePodMetadataIfNecessary(ctx context.Context, c client.Client, vm *vmv1.VirtualMachine, runnerPod *corev1.Pod) error {
	log := log.FromContext(ctx)

	var patches []patch.Operation

	metaSpecs := []struct {
		metaField   string
		expected    map[string]string
		actual      map[string]string
		ignoreExtra map[string]bool // use bool here so `if ignoreExtra[key] { ... }` works
	}{
		{
			metaField: "labels",
			expected:  labelsForVirtualMachine(vm, nil), // don't include runner version
			actual:    runnerPod.Labels,
			ignoreExtra: map[string]bool{
				// Don't override the runner pod version - we need to keep it around without
				// changing it; otherwise it's not useful!
				vmv1.RunnerPodVersionLabel: true,
			},
		},
		{
			metaField: "annotations",
			expected:  annotationsForVirtualMachine(vm),
			actual:    runnerPod.Annotations,
			ignoreExtra: map[string]bool{
				"k8s.v1.cni.cncf.io/networks":        true,
				"k8s.v1.cni.cncf.io/network-status":  true,
				"k8s.v1.cni.cncf.io/networks-status": true,
			},
		},
	}

	var removedMessageParts []string

	for _, spec := range metaSpecs {
		// Add/update the entries we're expecting to be there
		for k, e := range spec.expected {
			if a, ok := spec.actual[k]; !ok || e != a {
				patches = append(patches, patch.Operation{
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
					Op:    patch.OpAdd,
					Path:  fmt.Sprintf("/metadata/%s/%s", spec.metaField, patch.PathEscape(k)),
					Value: e,
				})
			}
		}

		// Remove the entries we aren't expecting to be there
		var removed []string
		for k := range spec.actual {
			if _, expected := spec.expected[k]; !expected && !spec.ignoreExtra[k] {
				removed = append(removed, k)
				patches = append(patches, patch.Operation{
					Op:   patch.OpRemove,
					Path: fmt.Sprintf("/metadata/%s/%s", spec.metaField, patch.PathEscape(k)),
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
	cpu := spec.Guest.CPUs.Use

	memorySlots := spec.Guest.MemorySlots.Use

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

func extractVirtualMachineResourcesJSON(spec vmv1.VirtualMachineSpec) string {
	resourcesJSON, err := json.Marshal(spec.Resources())
	if err != nil {
		panic(fmt.Errorf("error marshalling JSON: %w", err))
	}

	return string(resourcesJSON)
}

// podForVirtualMachine returns a VirtualMachine Pod object
func (r *VMReconciler) podForVirtualMachine(
	vm *vmv1.VirtualMachine,
	memoryProvider vmv1.MemoryProvider,
	sshSecret *corev1.Secret,
) (*corev1.Pod, error) {
	pod, err := podSpec(vm, memoryProvider, sshSecret, r.Config, true)
	if err != nil {
		return nil, err
	}

	// Set the ownerRef for the Pod
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(vm, pod, r.Scheme); err != nil {
		return nil, err
	}

	return pod, nil
}

func (r *VMReconciler) sshSecretForVirtualMachine(vm *vmv1.VirtualMachine) (*corev1.Secret, error) {
	secret, err := sshSecretSpec(vm)
	if err != nil {
		return nil, err
	}

	// Set the ownerRef for the Secret
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(vm, secret, r.Scheme); err != nil {
		return nil, err
	}
	return secret, nil
}

func sshSecretSpec(vm *vmv1.VirtualMachine) (*corev1.Secret, error) {
	// using ed25519 signatures it takes ~16us to finish
	publicKey, privateKey, err := sshKeygen()
	if err != nil {
		return nil, err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vm.Status.SSHSecretName,
			Namespace: vm.Namespace,
		},
		Immutable: lo.ToPtr(true),
		Type:      corev1.SecretTypeSSHAuth,
		Data: map[string][]byte{
			"ssh-publickey":  publicKey,
			"ssh-privatekey": privateKey,
		},
	}

	return secret, nil
}

// labelsForVirtualMachine returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForVirtualMachine(vm *vmv1.VirtualMachine, runnerVersion *api.RunnerProtoVersion) map[string]string {
	l := make(map[string]string, len(vm.Labels)+3)
	for k, v := range vm.Labels {
		l[k] = v
	}

	l["app.kubernetes.io/name"] = "NeonVM"
	l[vmv1.VirtualMachineNameLabel] = vm.Name
	if runnerVersion != nil {
		l[vmv1.RunnerPodVersionLabel] = fmt.Sprintf("%d", *runnerVersion)
	}
	return l
}

func annotationsForVirtualMachine(vm *vmv1.VirtualMachine) map[string]string {
	// use bool here so `if ignored[key] { ... }` works
	ignored := map[string]bool{
		"kubectl.kubernetes.io/last-applied-configuration": true,
	}

	a := make(map[string]string, len(vm.Annotations)+2)
	for k, v := range vm.Annotations {
		if !ignored[k] {
			a[k] = v
		}
	}

	a["kubectl.kubernetes.io/default-container"] = "neonvm-runner"
	a[vmv1.VirtualMachineUsageAnnotation] = extractVirtualMachineUsageJSON(vm.Spec)
	a[vmv1.VirtualMachineResourcesAnnotation] = extractVirtualMachineResourcesJSON(vm.Spec)
	return a
}

func affinityForVirtualMachine(vm *vmv1.VirtualMachine) *corev1.Affinity {
	a := vm.Spec.Affinity
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
						Values:   []string{sysruntime.GOARCH},
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

// imageForVirtualMachine gets the Operand image which is managed by this controller
// from the VM_RUNNER_IMAGE environment variable defined in the config/manager/manager.yaml
func imageForVmRunner() (string, error) {
	imageEnvVar := "VM_RUNNER_IMAGE"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("unable to find %s environment variable with the image", imageEnvVar)
	}
	return image, nil
}

// podSpec returns a VirtualMachine Pod object
// withReadinessProbe - if true, adds a readiness probe to the container
// we don't need readiness probe for the VM runner pod if it is a target pod
// for migration because VM is not started until migration is complete
func podSpec(
	vm *vmv1.VirtualMachine,
	memoryProvider vmv1.MemoryProvider,
	sshSecret *corev1.Secret,
	config *ReconcilerConfig,
	withReadinessProbe bool,
) (*corev1.Pod, error) {
	runnerVersion := api.RunnerProtoV1
	labels := labelsForVirtualMachine(vm, &runnerVersion)
	annotations := annotationsForVirtualMachine(vm)
	affinity := affinityForVirtualMachine(vm)

	// Get the Operand image
	image, err := imageForVmRunner()
	if err != nil {
		return nil, err
	}

	vmSpecJson, err := json.Marshal(vm.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal VM Spec: %w", err)
	}

	vmStatusJson, err := json.Marshal(vm.Status)
	if err != nil {
		return nil, fmt.Errorf("marshal VM Status: %w", err)
	}

	// We have to add tolerations explicitly here.
	// Otherwise, if the k8s node becomes unavailable, the default
	// tolerations will be added, which are 300s (5m) long, which is
	// not acceptable for us.
	tolerations := append([]corev1.Toleration{}, vm.Spec.Tolerations...)
	tolerations = append(tolerations,
		corev1.Toleration{
			Key:               "node.kubernetes.io/not-ready",
			TolerationSeconds: lo.ToPtr(int64(30)),
			Effect:            "NoExecute",
		},
		corev1.Toleration{
			Key:               "node.kubernetes.io/unreachable",
			TolerationSeconds: lo.ToPtr(int64(30)),
			Effect:            "NoExecute",
		},
	)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        vm.Status.PodName,
			Namespace:   vm.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			EnableServiceLinks:            vm.Spec.ServiceLinks,
			AutomountServiceAccountToken:  lo.ToPtr(false),
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: vm.Spec.TerminationGracePeriodSeconds,
			NodeSelector:                  vm.Spec.NodeSelector,
			ImagePullSecrets:              vm.Spec.ImagePullSecrets,
			Tolerations:                   tolerations,
			ServiceAccountName:            vm.Spec.ServiceAccountName,
			SchedulerName:                 vm.Spec.SchedulerName,
			Affinity:                      affinity,
			InitContainers: []corev1.Container{
				{
					Image:           vm.Spec.Guest.RootDisk.Image,
					Name:            "init",
					ImagePullPolicy: vm.Spec.Guest.RootDisk.ImagePullPolicy,
					VolumeMounts: []corev1.VolumeMount{{
						Name:      "virtualmachineimages",
						MountPath: "/vm/images",
					}},
					Command: []string{
						"sh", "-c",
						"mv /disk.qcow2 /vm/images/rootdisk.qcow2 && " +
							/* uid=36(qemu) gid=34(kvm) groups=34(kvm) */
							"chown 36:34 /vm/images/rootdisk.qcow2 && " +
							"sysctl -w net.ipv4.ip_forward=1",
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: lo.ToPtr(true),
					},
				},
			},
			// generate containers as an inline function so the context isn't isolated
			Containers: func() []corev1.Container {
				runner := corev1.Container{
					Image:           image,
					Name:            "neonvm-runner",
					ImagePullPolicy: corev1.PullIfNotPresent,
					// Ensure restrictive context for the container
					// More info: https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted
					SecurityContext: &corev1.SecurityContext{
						Privileged: lo.ToPtr(false),
						Capabilities: &corev1.Capabilities{
							Add: []corev1.Capability{
								"NET_ADMIN",
								"SYS_ADMIN",
								"SYS_RESOURCE",
							},
						},
					},
					Ports: []corev1.ContainerPort{{
						ContainerPort: vm.Spec.QMP,
						Name:          "qmp",
					}, {
						ContainerPort: vm.Spec.QMPManual,
						Name:          "qmp-manual",
					}},
					Command: func() []string {
						cmd := []string{"runner"}
						if config.DisableRunnerCgroup {
							cmd = append(cmd, "-skip-cgroup-management")
						}
						cmd = append(
							cmd,
							"-qemu-disk-cache-settings", config.QEMUDiskCacheSettings,
							"-memory-provider", string(memoryProvider),
						)
						if memoryProvider == vmv1.MemoryProviderVirtioMem {
							cmd = append(cmd, "-memhp-auto-movable-ratio", config.MemhpAutoMovableRatio)
						}
						// put these last, so that the earlier args are easier to see (because these
						// can get quite large)
						cmd = append(
							cmd,
							"-vmspec", base64.StdEncoding.EncodeToString(vmSpecJson),
							"-vmstatus", base64.StdEncoding.EncodeToString(vmStatusJson),
						)
						// NB: We don't need to check if the value is nil because the default value
						// was set in Reconcile
						cmd = append(cmd, "-cpu-scaling-mode", string(*vm.Spec.CpuScalingMode))
						return cmd
					}(),
					Env: []corev1.EnvVar{{
						Name: "K8S_POD_NAME",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "metadata.name",
							},
						},
					}},
					VolumeMounts: func() []corev1.VolumeMount {
						images := corev1.VolumeMount{
							Name:      "virtualmachineimages",
							MountPath: "/vm/images",
						}
						cgroups := corev1.VolumeMount{
							Name:      "sysfscgroup",
							MountPath: "/sys/fs/cgroup",
							// MountPropagationNone means that the volume in a container will
							// not receive new mounts from the host or other containers, and filesystems
							// mounted inside the container won't be propagated to the host or other
							// containers.
							// Note that this mode corresponds to "private" in Linux terminology.
							MountPropagation: lo.ToPtr(corev1.MountPropagationNone),
						}

						if config.DisableRunnerCgroup {
							return []corev1.VolumeMount{images}
						} else {
							// the /sys/fs/cgroup mount is only necessary if neonvm-runner has to
							// do is own cpu limiting
							return []corev1.VolumeMount{images, cgroups}
						}
					}(),
					Resources: vm.Spec.PodResources,
				}

				return []corev1.Container{runner}
			}(),
			Volumes: func() []corev1.Volume {
				images := corev1.Volume{
					Name: "virtualmachineimages",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				}
				cgroup := corev1.Volume{
					Name: "sysfscgroup",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/sys/fs/cgroup",
							Type: lo.ToPtr(corev1.HostPathDirectory),
						},
					},
				}
				if config.DisableRunnerCgroup {
					return []corev1.Volume{images}
				} else {
					return []corev1.Volume{images, cgroup}
				}
			}(),
		},
	}

	if withReadinessProbe {
		pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/ready",
					Port:   intstr.FromInt32(vm.Spec.RunnerPort),
					Scheme: corev1.URISchemeHTTP,
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			FailureThreshold:    3,
		}
	}

	if sshSecret != nil {
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{
				Name:      "ssh-privatekey",
				MountPath: "/mnt/ssh",
			},
			corev1.VolumeMount{
				Name:      "ssh-publickey",
				MountPath: "/vm/ssh",
			},
		)
		pod.Spec.Volumes = append(pod.Spec.Volumes,
			corev1.Volume{
				Name: "ssh-privatekey",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: sshSecret.Name,
						Items: []corev1.KeyToPath{
							{
								Key:  "ssh-privatekey",
								Path: "id_ed25519",
								Mode: lo.ToPtr[int32](0o600),
							},
						},
					},
				},
			},
			corev1.Volume{
				Name: "ssh-publickey",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: sshSecret.Name,
						Items: []corev1.KeyToPath{
							{
								Key:  "ssh-publickey",
								Path: "authorized_keys",
								Mode: lo.ToPtr[int32](0o644),
							},
						},
					},
				},
			},
		)
	}

	// If a custom neonvm-runner image is requested, use that instead:
	if vm.Spec.RunnerImage != nil {
		pod.Spec.Containers[0].Image = *vm.Spec.RunnerImage
	}

	// If a custom kernel is used, add that image:
	if vm.Spec.Guest.KernelImage != nil {
		pod.Spec.Containers[0].Args = append(pod.Spec.Containers[0].Args, "-kernelpath=/vm/images/vmlinuz")
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
			Image:           *vm.Spec.Guest.KernelImage,
			Name:            "init-kernel",
			ImagePullPolicy: vm.Spec.Guest.RootDisk.ImagePullPolicy,
			Args:            []string{"cp", "/vmlinuz", "/vm/images/vmlinuz"},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "virtualmachineimages",
				MountPath: "/vm/images",
			}},
			SecurityContext: &corev1.SecurityContext{
				// uid=36(qemu) gid=34(kvm) groups=34(kvm)
				RunAsUser:  lo.ToPtr[int64](36),
				RunAsGroup: lo.ToPtr[int64](34),
			},
		})
	}

	if vm.Spec.Guest.AppendKernelCmdline != nil {
		pod.Spec.Containers[0].Args = append(pod.Spec.Containers[0].Args, fmt.Sprintf("-appendKernelCmdline=%s", *vm.Spec.Guest.AppendKernelCmdline))
	}

	// Add any InitContainers that were specified by the spec
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, vm.Spec.ExtraInitContainers...)

	// allow access to /dev/kvm and /dev/vhost-net devices by generic-device-plugin for kubelet
	if pod.Spec.Containers[0].Resources.Limits == nil {
		pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{}
	}
	pod.Spec.Containers[0].Resources.Limits["neonvm/vhost-net"] = resource.MustParse("1")
	// NB: EnableAcceleration guaranteed non-nil because the k8s API server sets the default for us.
	if *vm.Spec.EnableAcceleration {
		pod.Spec.Containers[0].Resources.Limits["neonvm/kvm"] = resource.MustParse("1")
	}

	for _, port := range vm.Spec.Guest.Ports {
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

	if settings := vm.Spec.Guest.Settings; settings != nil {
		if swapSize := settings.Swap; swapSize != nil {
			diskName := "swapdisk"
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
				Name:      diskName,
				MountPath: fmt.Sprintf("/vm/mounts/%s", diskName),
			})
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: diskName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						SizeLimit: swapSize,
					},
				},
			})
		}
	}

	for _, disk := range vm.Spec.Disks {

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

	// use multus network to add extra network interface
	if vm.Spec.ExtraNetwork != nil && vm.Spec.ExtraNetwork.Enable {
		var nadNetwork string
		if len(vm.Spec.ExtraNetwork.MultusNetwork) > 0 { // network specified in spec
			nadNetwork = vm.Spec.ExtraNetwork.MultusNetwork
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
		pod.ObjectMeta.Annotations[nadapiv1.NetworkAttachmentAnnot] = fmt.Sprintf("%s@%s", nadNetwork, vm.Spec.ExtraNetwork.Interface)
	}

	return pod, nil
}

// SetupWithManager sets up the controller with the Manager.
// Note that the Runner Pod will be also watched in order to ensure its
// desirable state on the cluster
func (r *VMReconciler) SetupWithManager(mgr ctrl.Manager) (ReconcilerWithMetrics, error) {
	cntrlName := "virtualmachine"
	reconciler := WithMetrics(
		withCatchPanic(r),
		r.Metrics,
		cntrlName,
		r.Config.FailurePendingPeriod,
		r.Config.FailingRefreshInterval,
	)
	err := ctrl.NewControllerManagedBy(mgr).
		For(&vmv1.VirtualMachine{}).
		Owns(&corev1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.Config.MaxConcurrentReconciles}).
		Named(cntrlName).
		Complete(reconciler)
	return reconciler, err
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
func (r *VMReconciler) tryUpdateVM(ctx context.Context, vm *vmv1.VirtualMachine) error {
	return r.Update(ctx, vm)
}

// return Network Attachment Definition name with IPAM settings
func nadIpamName() (string, error) {
	return getEnvVarValue("NAD_IPAM_NAME")
}

// return Network Attachment Definition namespace with IPAM settings
func nadIpamNamespace() (string, error) {
	return getEnvVarValue("NAD_IPAM_NAMESPACE")
}

// return Network Attachment Definition name for second interface in Runner
func nadRunnerName() (string, error) {
	return getEnvVarValue("NAD_RUNNER_NAME")
}

// return Network Attachment Definition namespace for second interface in Runner
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

// sshKeygen generates a pair of public and private keys using the ed25519
// algorithm. It returns the generated public key and private key as byte
// slices. If an error occurs during key generation or encoding, it returns nil
// for both keys and the error.
func sshKeygen() (publicKeyBytes []byte, privateKeyBytes []byte, err error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	publicKeyBytes, err = encodePublicKey(publicKey)
	if err != nil {
		return nil, nil, err
	}

	privateKeyBytes, err = encodePrivateKey(privateKey)
	if err != nil {
		return nil, nil, err
	}

	return
}

func encodePrivateKey(privateKey ed25519.PrivateKey) ([]byte, error) {
	privBlock, err := ssh.MarshalPrivateKey(privateKey, "")
	if err != nil {
		return nil, err
	}
	privatePEM := pem.EncodeToMemory(privBlock)

	return privatePEM, nil
}

func encodePublicKey(publicKey ed25519.PublicKey) ([]byte, error) {
	sshPublicKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		return nil, err
	}

	pubKeyBytes := ssh.MarshalAuthorizedKey(sshPublicKey)
	return pubKeyBytes, nil
}
