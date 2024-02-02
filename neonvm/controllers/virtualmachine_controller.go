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
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"time"

	nadapiv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
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
	"github.com/neondatabase/autoscaling/neonvm/controllers/buildtag"
	"github.com/neondatabase/autoscaling/neonvm/pkg/ipam"
	"github.com/neondatabase/autoscaling/pkg/api"
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

// VirtualMachineReconciler reconciles a VirtualMachine object
type VirtualMachineReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Config   *ReconcilerConfig

	Metrics ReconcilerMetrics `exhaustruct:"optional"`
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=vm.neon.tech,resources=virtualmachines/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=list
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
			r.doFinalizerOperationsForVirtualMachine(ctx, &virtualmachine)

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

// doFinalizerOperationsForVirtualMachine will perform the required operations before delete the CR.
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
		ip, err := ipam.ReleaseIP(ctx, virtualmachine.Name, virtualmachine.Namespace)
		if err != nil {
			// ignore error
			log.Error(err, "fail to release IP, error ignored")
			return
		}
		message := fmt.Sprintf("Released IP %s", ip.String())
		log.Info(message)
		r.Recorder.Event(virtualmachine, "Normal", "OverlayNet", message)
	}
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

func (r *VirtualMachineReconciler) updateVMStatusCPU(
	ctx context.Context,
	virtualmachine *vmv1.VirtualMachine,
	vmRunner *corev1.Pod,
	qmpPluggedCPUs uint32,
	cgroupUsage *api.VCPUCgroup,
) {
	log := log.FromContext(ctx)

	// We expect:
	// - vm.Status.CPUs = cgroupUsage.VCPUs
	// - vm.Status.CPUs.RoundUp() == qmpPluggedCPUs
	// Otherwise, we update the status.
	var currentCPUUsage vmv1.MilliCPU
	if cgroupUsage != nil {
		if cgroupUsage.VCPUs.RoundedUp() != qmpPluggedCPUs {
			// This is not expected but it's fine. We only report the
			// mismatch here and will resolve it in the next reconcile
			// iteration loops by comparing these values to spec CPU use
			// and moving to the scaling phase.
			log.Error(nil, "Mismatch in the number of VM's plugged CPUs and runner pod's cgroup vCPUs",
				"VirtualMachine", virtualmachine.Name,
				"Runner Pod", vmRunner.Name,
				"plugged CPUs", qmpPluggedCPUs,
				"cgroup vCPUs", cgroupUsage.VCPUs)
		}
		currentCPUUsage = min(cgroupUsage.VCPUs, vmv1.MilliCPU(1000*qmpPluggedCPUs))
	} else {
		currentCPUUsage = vmv1.MilliCPU(1000 * qmpPluggedCPUs)
	}
	if virtualmachine.Status.CPUs == nil || *virtualmachine.Status.CPUs != currentCPUUsage {
		virtualmachine.Status.CPUs = &currentCPUUsage
		r.Recorder.Event(virtualmachine, "Normal", "CpuInfo",
			fmt.Sprintf("VirtualMachine %s uses %v cpu cores",
				virtualmachine.Name,
				virtualmachine.Status.CPUs))
	}
}

func (r *VirtualMachineReconciler) updateVMStatusMemory(
	virtualmachine *vmv1.VirtualMachine,
	qmpMemorySize *resource.Quantity,
) {
	if virtualmachine.Status.MemorySize == nil || !qmpMemorySize.Equal(*virtualmachine.Status.MemorySize) {
		virtualmachine.Status.MemorySize = qmpMemorySize
		r.Recorder.Event(virtualmachine, "Normal", "MemoryInfo",
			fmt.Sprintf("VirtualMachine %s uses %v memory",
				virtualmachine.Name,
				virtualmachine.Status.MemorySize))
	}
}

func (r *VirtualMachineReconciler) doReconcile(ctx context.Context, virtualmachine *vmv1.VirtualMachine) error {
	log := log.FromContext(ctx)

	// Let's check and just set the condition status as Unknown when no status are available
	if virtualmachine.Status.Conditions == nil || len(virtualmachine.Status.Conditions) == 0 {
		// set Unknown condition status for AvailableVirtualMachine
		meta.SetStatusCondition(&virtualmachine.Status.Conditions, metav1.Condition{Type: typeAvailableVirtualMachine, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
	}

	// NB: .Spec.EnableSSH guaranteed non-nil because the k8s API server sets the default for us.
	enableSSH := *virtualmachine.Spec.EnableSSH

	// Generate ssh secret name
	if enableSSH && len(virtualmachine.Status.SSHSecretName) == 0 {
		virtualmachine.Status.SSHSecretName = fmt.Sprintf("ssh-neonvm-%s", virtualmachine.Name)
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
			message := fmt.Sprintf("Acquired IP %s for overlay network interface", ip.String())
			log.Info(message)
			virtualmachine.Status.ExtraNetIP = ip.IP.String()
			virtualmachine.Status.ExtraNetMask = fmt.Sprintf("%d.%d.%d.%d", ip.Mask[0], ip.Mask[1], ip.Mask[2], ip.Mask[3])
			r.Recorder.Event(virtualmachine, "Normal", "OverlayNet", message)
		}
		// VirtualMachine just created, change Phase to "Pending"
		virtualmachine.Status.Phase = vmv1.VmPending
		virtualmachine.Status.RestartCount = &[]int32{0}[0] // set value to pointer to 0
	case vmv1.VmPending:
		// Check if the runner pod already exists, if not create a new one
		vmRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: virtualmachine.Status.PodName, Namespace: virtualmachine.Namespace}, vmRunner)
		if err != nil && apierrors.IsNotFound(err) {
			var sshSecret *corev1.Secret
			if enableSSH {
				// Check if the ssh secret already exists, if not create a new one
				sshSecret = &corev1.Secret{}
				err := r.Get(ctx, types.NamespacedName{
					Name:      virtualmachine.Status.SSHSecretName,
					Namespace: virtualmachine.Namespace,
				}, sshSecret)
				if err != nil && apierrors.IsNotFound(err) {
					// Define a new ssh secret
					sshSecret, err = r.sshSecretForVirtualMachine(virtualmachine)
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
			pod, err := r.podForVirtualMachine(virtualmachine, sshSecret)
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

			msg := fmt.Sprintf("VirtualMachine %s created, Pod %s", virtualmachine.Name, pod.Name)
			if sshSecret != nil {
				msg = fmt.Sprintf("%s, SSH Secret %s", msg, sshSecret.Name)
			}
			r.Recorder.Event(virtualmachine, "Normal", "Created", msg)
		} else if err != nil {
			log.Error(err, "Failed to get vm-runner Pod")
			return err
		}
		// runner pod found, check phase
		switch runnerStatus(vmRunner) {
		case runnerRunning:
			virtualmachine.Status.PodIP = vmRunner.Status.PodIP
			virtualmachine.Status.Phase = vmv1.VmRunning
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) created successfully", virtualmachine.Status.PodName, virtualmachine.Name)})
		case runnerSucceeded:
			virtualmachine.Status.Phase = vmv1.VmSucceeded
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachine,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) succeeded", virtualmachine.Status.PodName, virtualmachine.Name)})
		case runnerFailed:
			virtualmachine.Status.Phase = vmv1.VmFailed
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeDegradedVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) failed", virtualmachine.Status.PodName, virtualmachine.Name)})
		case runnerUnknown:
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
		switch runnerStatus(vmRunner) {
		case runnerRunning:
			// update status by IP of runner pod
			virtualmachine.Status.PodIP = vmRunner.Status.PodIP
			// update phase
			virtualmachine.Status.Phase = vmv1.VmRunning
			// update Node name where runner working
			virtualmachine.Status.Node = vmRunner.Spec.NodeName

			// get CPU details from QEMU
			cpuSlotsPlugged, _, err := QmpGetCpus(QmpAddr(virtualmachine))
			if err != nil {
				log.Error(err, "Failed to get CPU details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
				return err
			}
			pluggedCPU := uint32(len(cpuSlotsPlugged))

			// get cgroups CPU details from runner pod
			var cgroupUsage *api.VCPUCgroup
			supportsCgroup := runnerSupportsCgroup(vmRunner)
			if supportsCgroup {
				cgroupUsage, err = getRunnerCgroup(ctx, virtualmachine)
				if err != nil {
					log.Error(err, "Failed to get CPU details from runner", "VirtualMachine", virtualmachine.Name)
					return err
				}
			}

			// update status by CPUs used in the VM
			r.updateVMStatusCPU(ctx, virtualmachine, vmRunner, pluggedCPU, cgroupUsage)

			// get Memory details from hypervisor and update VM status
			memorySize, err := QmpGetMemorySize(QmpAddr(virtualmachine))
			if err != nil {
				log.Error(err, "Failed to get Memory details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
				return err
			}
			// update status by memory sizes used in the VM
			r.updateVMStatusMemory(virtualmachine, memorySize)

			// check if need hotplug/unplug CPU or memory
			// compare guest spec and count of plugged

			specUseCPU := virtualmachine.Spec.Guest.CPUs.Use
			scaleCgroupCPU := supportsCgroup && *specUseCPU != cgroupUsage.VCPUs
			scaleQemuCPU := specUseCPU.RoundedUp() != pluggedCPU
			if scaleCgroupCPU || scaleQemuCPU {
				if !supportsCgroup {
					log.Info("VM goes into scaling mode, CPU count needs to be changed",
						"CPUs on board", pluggedCPU,
						"CPUs in spec", virtualmachine.Spec.Guest.CPUs.Use)
				} else {
					log.Info("VM goes into scaling mode, CPU count needs to be changed",
						"CPUs on runner pod cgroup", cgroupUsage.VCPUs,
						"CPUs on board", pluggedCPU,
						"CPUs in spec", virtualmachine.Spec.Guest.CPUs.Use)
				}
				virtualmachine.Status.Phase = vmv1.VmScaling
			}

			memorySizeFromSpec := resource.NewQuantity(int64(*virtualmachine.Spec.Guest.MemorySlots.Use)*virtualmachine.Spec.Guest.MemorySlotSize.Value(), resource.BinarySI)
			if !memorySize.Equal(*memorySizeFromSpec) {
				log.Info("VM goes into scale mode, need to resize Memory",
					"Memory on board", memorySize,
					"Memory in spec", memorySizeFromSpec)
				virtualmachine.Status.Phase = vmv1.VmScaling
			}

		case runnerSucceeded:
			virtualmachine.Status.Phase = vmv1.VmSucceeded
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachine,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) succeeded", virtualmachine.Status.PodName, virtualmachine.Name)})
		case runnerFailed:
			virtualmachine.Status.Phase = vmv1.VmFailed
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeDegradedVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) failed", virtualmachine.Status.PodName, virtualmachine.Name)})
		case runnerUnknown:
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
		// Check that runner pod is still ok
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

		// runner pod found, check that it's still up:
		switch runnerStatus(vmRunner) {
		case runnerSucceeded:
			virtualmachine.Status.Phase = vmv1.VmSucceeded
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachine,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) succeeded", virtualmachine.Status.PodName, virtualmachine.Name)})
			return nil
		case runnerFailed:
			virtualmachine.Status.Phase = vmv1.VmFailed
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeDegradedVirtualMachine,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) failed", virtualmachine.Status.PodName, virtualmachine.Name)})
			return nil
		case runnerUnknown:
			virtualmachine.Status.Phase = vmv1.VmPending
			meta.SetStatusCondition(&virtualmachine.Status.Conditions,
				metav1.Condition{Type: typeAvailableVirtualMachine,
					Status:  metav1.ConditionUnknown,
					Reason:  "Reconciling",
					Message: fmt.Sprintf("Pod (%s) for VirtualMachine (%s) in Unknown phase", virtualmachine.Status.PodName, virtualmachine.Name)})
			return nil
		default:
			// do nothing
		}

		cpuScaled := false
		ramScaled := false

		// do hotplug/unplug CPU
		// firstly get current state from QEMU
		cpuSlotsPlugged, _, err := QmpGetCpus(QmpAddr(virtualmachine))
		if err != nil {
			log.Error(err, "Failed to get CPU details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
			return err
		}
		specCPU := virtualmachine.Spec.Guest.CPUs.Use
		pluggedCPU := uint32(len(cpuSlotsPlugged))

		var cgroupUsage *api.VCPUCgroup
		supportsCgroup := runnerSupportsCgroup(vmRunner)
		if supportsCgroup {
			cgroupUsage, err = getRunnerCgroup(ctx, virtualmachine)
			if err != nil {
				log.Error(err, "Failed to get CPU details from runner", "VirtualMachine", virtualmachine.Name)
				return err
			}
		}

		// compare guest spec to count of plugged and runner pod cgroups
		if specCPU.RoundedUp() > pluggedCPU {
			// going to plug one CPU
			log.Info("Plug one more CPU into VM")
			if err := QmpPlugCpu(QmpAddr(virtualmachine)); err != nil {
				return err
			}
			r.Recorder.Event(virtualmachine, "Normal", "ScaleUp",
				fmt.Sprintf("One more CPU was plugged into VM %s",
					virtualmachine.Name))
		} else if specCPU.RoundedUp() < pluggedCPU {
			// going to unplug one CPU
			log.Info("Unplug one CPU from VM")
			if err := QmpUnplugCpu(QmpAddr(virtualmachine)); err != nil {
				return err
			}
			r.Recorder.Event(virtualmachine, "Normal", "ScaleDown",
				fmt.Sprintf("One CPU was unplugged from VM %s",
					virtualmachine.Name))
		} else if supportsCgroup && *specCPU != cgroupUsage.VCPUs {
			log.Info("Update runner pod cgroups", "runner", cgroupUsage.VCPUs, "spec", *specCPU)
			if err := setRunnerCgroup(ctx, virtualmachine, *specCPU); err != nil {
				return err
			}
			reason := "ScaleDown"
			if *specCPU > cgroupUsage.VCPUs {
				reason = "ScaleUp"
			}
			r.Recorder.Event(virtualmachine, "Normal", reason,
				fmt.Sprintf("Runner pod cgroups was updated on VM %s",
					virtualmachine.Name))
		} else {
			// seems already plugged correctly
			cpuScaled = true
		}

		// do hotplug/unplug Memory
		memSlotsMin := *virtualmachine.Spec.Guest.MemorySlots.Min
		targetSlotCount := int(*virtualmachine.Spec.Guest.MemorySlots.Use - memSlotsMin)

		realSlots, err := QmpSetMemorySlots(ctx, virtualmachine, targetSlotCount, r.Recorder)
		if realSlots < 0 {
			return err
		}

		if realSlots != int(targetSlotCount) {
			log.Info("Couldn't achieve desired memory slot count, will modify .spec.guest.memorySlots.use instead", "details", err)
			// firstly re-fetch VM
			if err := r.Get(ctx, types.NamespacedName{Name: virtualmachine.Name, Namespace: virtualmachine.Namespace}, virtualmachine); err != nil {
				log.Error(err, "Unable to re-fetch VirtualMachine")
				return err
			}
			memorySlotsUseInSpec := *virtualmachine.Spec.Guest.MemorySlots.Use
			memoryPluggedSlots := memSlotsMin + int32(realSlots)
			*virtualmachine.Spec.Guest.MemorySlots.Use = memoryPluggedSlots
			if err := r.tryUpdateVM(ctx, virtualmachine); err != nil {
				log.Error(err, "Failed to update .spec.guest.memorySlots.use",
					"old value", memorySlotsUseInSpec,
					"new value", memoryPluggedSlots)
				return err
			}
		} else {
			ramScaled = true
		}

		// set VM phase to running if everything scaled
		if cpuScaled && ramScaled {
			// update status by CPUs used in the VM
			r.updateVMStatusCPU(ctx, virtualmachine, vmRunner, pluggedCPU, cgroupUsage)

			// get Memory details from hypervisor and update VM status
			memorySize, err := QmpGetMemorySize(QmpAddr(virtualmachine))
			if err != nil {
				log.Error(err, "Failed to get Memory details from VirtualMachine", "VirtualMachine", virtualmachine.Name)
				return err
			}
			// update status by memory sizes used in the VM
			r.updateVMStatusMemory(virtualmachine, memorySize)

			virtualmachine.Status.Phase = vmv1.VmRunning
		}

	case vmv1.VmSucceeded, vmv1.VmFailed:
		// Always delete runner pod. Otherwise, we could end up with one container succeeded/failed
		// but the other one still running (meaning that the pod still ends up Running).
		vmRunner := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: virtualmachine.Status.PodName, Namespace: virtualmachine.Namespace}, vmRunner)
		if err == nil {
			// delete current runner
			if err := r.deleteRunnerPodIfEnabled(ctx, virtualmachine, vmRunner); err != nil {
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}

		// We must keep the VM status the same until we know the neonvm-runner container has been
		// terminated, otherwise we could end up starting a new runner pod while the VM in the old
		// one is still running.
		//
		// Note that this is required because 'VmSucceeded' and 'VmFailed' are true if *at least
		// one* container inside the runner pod has finished; the VM itself may still be running.
		if apierrors.IsNotFound(err) || runnerContainerStopped(vmRunner) {
			// NB: Cleanup() leaves status .Phase and .RestartCount (+ some others) but unsets other fields.
			virtualmachine.Cleanup()

			var shouldRestart bool
			switch virtualmachine.Spec.RestartPolicy {
			case vmv1.RestartPolicyAlways:
				shouldRestart = true
			case vmv1.RestartPolicyOnFailure:
				shouldRestart = virtualmachine.Status.Phase == vmv1.VmFailed
			case vmv1.RestartPolicyNever:
				shouldRestart = false
			}

			if shouldRestart {
				log.Info("Restarting VM runner pod", "VM.Phase", virtualmachine.Status.Phase, "RestartPolicy", virtualmachine.Spec.RestartPolicy)
				virtualmachine.Status.Phase = vmv1.VmPending // reset to trigger restart
				*virtualmachine.Status.RestartCount += 1     // increment restart count
			}

			// TODO for RestartPolicyNever: implement TTL or do nothing
		}
	default:
		// do nothing
	}

	return nil
}

type runnerStatusKind string

const (
	runnerUnknown   runnerStatusKind = "Unknown"
	runnerPending   runnerStatusKind = "Pending"
	runnerRunning   runnerStatusKind = "Running"
	runnerFailed    runnerStatusKind = "Failed"
	runnerSucceeded runnerStatusKind = "Succeeded"
)

// runnerStatus returns a description of the status of the VM inside the runner pod.
//
// This is *similar* to the value of pod.Status.Phase, but takes into consideration the statuses of
// the individual containers within the pod. This is because Kubernetes sets the pod phase to Failed
// or Succeeded only if *all* pods have exited, whereas we'd like to consider the VM to be Failed or
// Succeeded if *any* pod has exited.
//
// The full set of outputs is:
//
//   - runnerUnknown, if pod.Status.Phase is Unknown
//   - runnerPending, if pod.Status.Phase is "" or Pending
//   - runnerRunning, if pod.Status.Phase is Running, and no containers have exited
//   - runnerFailed, if pod.Status.Phase is Failed, or if any container has failed, or if any
//     container other than neonvm-runner has exited
//   - runnerSucceeded, if pod.Status.Phase is Succeeded, or if neonvm-runner has exited
//     successfully
func runnerStatus(pod *corev1.Pod) runnerStatusKind {
	switch pod.Status.Phase {
	case "", corev1.PodPending:
		return runnerPending
	case corev1.PodSucceeded:
		return runnerSucceeded
	case corev1.PodFailed:
		return runnerFailed
	case corev1.PodUnknown:
		return runnerUnknown

	// See comment above for context on this logic
	case corev1.PodRunning:
		nonRunnerContainerSucceeded := false
		runnerContainerSucceeded := false

		for _, stat := range pod.Status.ContainerStatuses {
			if stat.State.Terminated != nil {
				failed := stat.State.Terminated.ExitCode != 0
				isRunner := stat.Name == "neonvm-runner"

				if failed {
					// return that the "runner" has failed if any container has.
					return runnerFailed
				} else /* succeeded */ {
					if isRunner {
						// neonvm-runner succeeded. We'll return runnerSucceeded if no other
						// container has failed.
						runnerContainerSucceeded = true
					} else {
						// Other container has succeeded. We'll return runnerSucceeded if
						// neonvm-runner has succeeded, but runnerFailed if this exited while
						// neonvm-runner is still going.
						nonRunnerContainerSucceeded = true
					}
				}
			}
		}

		if runnerContainerSucceeded {
			return runnerSucceeded
		} else if nonRunnerContainerSucceeded {
			return runnerFailed
		} else {
			return runnerRunning
		}

	default:
		panic(fmt.Errorf("unknown pod phase: %q", pod.Status.Phase))
	}
}

// runnerContainerStopped returns true iff the neonvm-runner container has exited.
//
// The guarantee is simple: It is only safe to start a new runner pod for a VM if
// runnerContainerStopped returns true (otherwise, we may end up with >1 instance of the same VM).
func runnerContainerStopped(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true
	}

	for _, stat := range pod.Status.ContainerStatuses {
		if stat.Name == "neonvm-runner" {
			return stat.State.Terminated != nil
		}
	}
	return false
}

// deleteRunnerPodIfEnabled deletes the runner pod if buildtag.NeverDeleteRunnerPods is false, and
// then emits an event and log line about what it did, whether it actually deleted the runner pod.
func (r *VirtualMachineReconciler) deleteRunnerPodIfEnabled(
	ctx context.Context,
	virtualmachine *vmv1.VirtualMachine,
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
	r.Recorder.Event(virtualmachine, "Normal", eventReason, fmt.Sprintf("%s: %s", msg, runner.Name))
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
	virtualmachine *vmv1.VirtualMachine,
	sshSecret *corev1.Secret,
) (*corev1.Pod, error) {
	pod, err := podSpec(virtualmachine, sshSecret, r.Config)
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

func (r *VirtualMachineReconciler) sshSecretForVirtualMachine(virtualmachine *vmv1.VirtualMachine) (*corev1.Secret, error) {
	secret, err := sshSecretSpec(virtualmachine)
	if err != nil {
		return nil, err
	}

	// Set the ownerRef for the Secret
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(virtualmachine, secret, r.Scheme); err != nil {
		return nil, err
	}
	return secret, nil
}

func sshSecretSpec(virtualmachine *vmv1.VirtualMachine) (*corev1.Secret, error) {
	// using ed25519 signatures it takes ~16us to finish
	publicKey, privateKey, err := sshKeygen()
	if err != nil {
		return nil, err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      virtualmachine.Status.SSHSecretName,
			Namespace: virtualmachine.Namespace,
		},
		Immutable: &[]bool{true}[0],
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
func labelsForVirtualMachine(virtualmachine *vmv1.VirtualMachine, runnerVersion *api.RunnerProtoVersion) map[string]string {
	l := make(map[string]string, len(virtualmachine.Labels)+3)
	for k, v := range virtualmachine.Labels {
		l[k] = v
	}

	l["app.kubernetes.io/name"] = "NeonVM"
	l[vmv1.VirtualMachineNameLabel] = virtualmachine.Name
	if runnerVersion != nil {
		l[vmv1.RunnerPodVersionLabel] = fmt.Sprintf("%d", *runnerVersion)
	}
	return l
}

func annotationsForVirtualMachine(virtualmachine *vmv1.VirtualMachine) map[string]string {
	// use bool here so `if ignored[key] { ... }` works
	ignored := map[string]bool{
		"kubectl.kubernetes.io/last-applied-configuration": true,
	}

	a := make(map[string]string, len(virtualmachine.Annotations)+2)
	for k, v := range virtualmachine.Annotations {
		if !ignored[k] {
			a[k] = v
		}
	}

	a["kubectl.kubernetes.io/default-container"] = "neonvm-runner"
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

func setRunnerCgroup(ctx context.Context, vm *vmv1.VirtualMachine, cpu vmv1.MilliCPU) error {
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
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}

func getRunnerCgroup(ctx context.Context, vm *vmv1.VirtualMachine) (*api.VCPUCgroup, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/cpu_current", vm.Status.PodIP, vm.Spec.RunnerPort)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		return nil, err
	}

	var result api.VCPUCgroup
	err = json.Unmarshal(body, &result)
	if err != nil {
		return nil, err
	}

	return &result, nil
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

func podSpec(virtualmachine *vmv1.VirtualMachine, sshSecret *corev1.Secret, config *ReconcilerConfig) (*corev1.Pod, error) {
	runnerVersion := api.RunnerProtoV1
	labels := labelsForVirtualMachine(virtualmachine, &runnerVersion)
	annotations := annotationsForVirtualMachine(virtualmachine)
	affinity := affinityForVirtualMachine(virtualmachine)

	// Get the Operand image
	image, err := imageForVmRunner()
	if err != nil {
		return nil, err
	}

	vmSpecJson, err := json.Marshal(virtualmachine.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal VM Spec: %w", err)
	}

	vmStatusJson, err := json.Marshal(virtualmachine.Status)
	if err != nil {
		return nil, fmt.Errorf("marshal VM Status: %w", err)
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
			ServiceAccountName:            virtualmachine.Spec.ServiceAccountName,
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
			// generate containers as an inline function so the context isn't isolated
			Containers: func() []corev1.Container {
				runner := corev1.Container{
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
					}, {
						ContainerPort: virtualmachine.Spec.QMPManual,
						Name:          "qmp-manual",
					}},
					Command: func() []string {
						cmd := []string{"runner"}
						// intentionally add this first, so it's easier to see among the very long
						// args that follow.
						if config.UseContainerMgr {
							cmd = append(cmd, "-skip-cgroup-management")
						}
						cmd = append(
							cmd,
							"-vmspec", base64.StdEncoding.EncodeToString(vmSpecJson),
							"-vmstatus", base64.StdEncoding.EncodeToString(vmStatusJson),
						)
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
							MountPropagation: &[]corev1.MountPropagationMode{corev1.MountPropagationNone}[0],
						}

						if config.UseContainerMgr {
							return []corev1.VolumeMount{images}
						} else {
							// the /sys/fs/cgroup mount is only necessary if neonvm-runner has to
							// do is own cpu limiting
							return []corev1.VolumeMount{images, cgroups}
						}
					}(),
					Resources: virtualmachine.Spec.PodResources,
				}
				containerMgr := corev1.Container{
					Image: image,
					Name:  "neonvm-container-mgr",
					Command: []string{
						"container-mgr",
						"-port", strconv.Itoa(int(virtualmachine.Spec.RunnerPort)),
						"-init-milli-cpu", strconv.Itoa(int(*virtualmachine.Spec.Guest.CPUs.Use)),
					},
					Env: []corev1.EnvVar{
						{
							Name: "K8S_POD_UID",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{
									FieldPath: "metadata.uid",
								},
							},
						},
						{
							Name:  "CRI_ENDPOINT",
							Value: fmt.Sprintf("unix://%s", config.criEndpointSocketPath()),
						},
					},
					LivenessProbe: &corev1.Probe{
						InitialDelaySeconds: 10,
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/healthz",
								Port: intstr.FromInt(int(virtualmachine.Spec.RunnerPort)),
							},
						},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("50m"),
							corev1.ResourceMemory: resource.MustParse("50Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"), // cpu limit > request, because usage is spiky
							corev1.ResourceMemory: resource.MustParse("50Mi"),
						},
					},
					// socket for crictl to connect to
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "containerdsock",
							MountPath: config.criEndpointSocketPath(),
						},
					},
				}

				if config.UseContainerMgr {
					return []corev1.Container{runner, containerMgr}
				} else {
					// Return only the runner if we aren't supposed to use container-mgr
					return []corev1.Container{runner}
				}
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
							Type: &[]corev1.HostPathType{corev1.HostPathDirectory}[0],
						},
					},
				}
				containerdSock := corev1.Volume{
					Name: "containerdsock",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: config.criEndpointSocketPath(),
							Type: &[]corev1.HostPathType{corev1.HostPathSocket}[0],
						},
					},
				}

				if config.UseContainerMgr {
					return []corev1.Volume{images, containerdSock}
				} else {
					return []corev1.Volume{images, cgroup}
				}
			}(),
		},
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
								Mode: &[]int32{0600}[0],
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
								Mode: &[]int32{0644}[0],
							},
						},
					},
				},
			},
		)
	}

	// If a custom neonvm-runner image is requested, use that instead:
	if virtualmachine.Spec.RunnerImage != nil {
		pod.Spec.Containers[0].Image = *virtualmachine.Spec.RunnerImage
		if config.UseContainerMgr {
			pod.Spec.Containers[1].Image = *virtualmachine.Spec.RunnerImage
		}
	}

	// If a custom kernel is used, add that image:
	if virtualmachine.Spec.Guest.KernelImage != nil {
		pod.Spec.Containers[0].Args = append(pod.Spec.Containers[0].Args, "-kernelpath=/vm/images/vmlinuz")
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
			Image:           *virtualmachine.Spec.Guest.KernelImage,
			Name:            "init-kernel",
			ImagePullPolicy: virtualmachine.Spec.Guest.RootDisk.ImagePullPolicy,
			Args:            []string{"cp", "/vmlinuz", "/vm/images/vmlinuz"},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "virtualmachineimages",
				MountPath: "/vm/images",
			}},
			SecurityContext: &corev1.SecurityContext{
				// uid=36(qemu) gid=34(kvm) groups=34(kvm)
				RunAsUser:  &[]int64{36}[0],
				RunAsGroup: &[]int64{34}[0],
			},
		})
	}

	if virtualmachine.Spec.Guest.AppendKernelCmdline != nil {
		pod.Spec.Containers[0].Args = append(pod.Spec.Containers[0].Args, fmt.Sprintf("-appendKernelCmdline=%s", *virtualmachine.Spec.Guest.AppendKernelCmdline))
	}

	// Add any InitContainers that were specified by the spec
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, virtualmachine.Spec.ExtraInitContainers...)

	// allow access to /dev/kvm and /dev/vhost-net devices by generic-device-plugin for kubelet
	if pod.Spec.Containers[0].Resources.Limits == nil {
		pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{}
	}
	pod.Spec.Containers[0].Resources.Limits["neonvm/vhost-net"] = resource.MustParse("1")
	// NB: EnableAcceleration guaranteed non-nil because the k8s API server sets the default for us.
	if *virtualmachine.Spec.EnableAcceleration {
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
	cntrlName := "virtualmachine"
	reconciler := WithMetrics(r, r.Metrics, cntrlName)
	return ctrl.NewControllerManagedBy(mgr).
		For(&vmv1.VirtualMachine{}).
		Owns(&corev1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.Config.MaxConcurrentReconciles}).
		Named(cntrlName).
		Complete(reconciler)
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
