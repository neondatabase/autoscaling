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

package v1

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VirtualMachineNameLabel is the label assigned to each NeonVM Pod, providing the name of the
// VirtualMachine object for the VM running in it
//
// This label can be used both to find which VM is running in a Pod (by getting the value of the
// label) or to find which Pod a VM is running in (by searching for Pods with the label equal to the
// VM's name).
const VirtualMachineNameLabel string = "vm.neon.tech/name"

// Label that determines the version of runner pod. May be missing on older runners
const RunnerPodVersionLabel string = "vm.neon.tech/runner-version"

// VirtualMachineUsageAnnotation is the annotation added to each runner Pod, mirroring information
// about the resource allocations of the VM running in the pod.
//
// The value of this annotation is always a JSON-encoded VirtualMachineUsage object.
const VirtualMachineUsageAnnotation string = "vm.neon.tech/usage"

// VirtualMachineUsage provides information about a VM's current usage. This is the type of the
// JSON-encoded data in the VirtualMachineUsageAnnotation attached to each runner pod.
type VirtualMachineUsage struct {
	CPU    *resource.Quantity `json:"cpu"`
	Memory *resource.Quantity `json:"memory"`
}

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// VirtualMachineSpec defines the desired state of VirtualMachine
type VirtualMachineSpec struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default:=20183
	// +optional
	QMP int32 `json:"qmp,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default:=25183
	// +optional
	RunnerPort int32 `json:"runnerPort,omitempty"`

	// +kubebuilder:default:=5
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds"`

	NodeSelector       map[string]string           `json:"nodeSelector,omitempty"`
	Affinity           *corev1.Affinity            `json:"affinity,omitempty"`
	Tolerations        []corev1.Toleration         `json:"tolerations,omitempty"`
	SchedulerName      string                      `json:"schedulerName,omitempty"`
	ServiceAccountName string                      `json:"serviceAccountName,omitempty"`
	PodResources       corev1.ResourceRequirements `json:"podResources,omitempty"`

	// +kubebuilder:default:=Always
	// +optional
	RestartPolicy RestartPolicy `json:"restartPolicy"`

	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	Guest Guest `json:"guest"`

	ExtraInitContainers []corev1.Container `json:"extraInitContainers,omitempty"`

	// List of disk that can be mounted by virtual machine.
	// +optional
	Disks []Disk `json:"disks,omitempty"`

	// Extra network interface attached to network provided by Mutlus CNI.
	// +optional
	ExtraNetwork *ExtraNetwork `json:"extraNetwork,omitempty"`

	// +optional
	ServiceLinks *bool `json:"service_links,omitempty"`

	// Use KVM acceleation
	// +kubebuilder:default:=true
	// +optional
	EnableAcceleration *bool `json:"enableAcceleration,omitempty"`
}

// +kubebuilder:validation:Enum=Always;OnFailure;Never
type RestartPolicy string

const (
	RestartPolicyAlways    RestartPolicy = "Always"
	RestartPolicyOnFailure RestartPolicy = "OnFailure"
	RestartPolicyNever     RestartPolicy = "Never"
)

type Guest struct {
	// +optional
	CPUs CPUs `json:"cpus"`
	// +optional
	// +kubebuilder:default:="1Gi"
	MemorySlotSize resource.Quantity `json:"memorySlotSize"`
	// +optional
	MemorySlots MemorySlots `json:"memorySlots"`
	// +optional
	RootDisk RootDisk `json:"rootDisk"`
	// Docker image Entrypoint array replacement.
	// +optional
	Command []string `json:"command,omitempty"`
	// Arguments to the entrypoint.
	// The docker image's cmd is used if this is not provided.
	// +optional
	Args []string `json:"args,omitempty"`
	// List of environment variables to set in the vmstart process.
	// +optional
	Env []EnvVar `json:"env,omitempty" patchStrategy:"merge" patchMergeKey:"name"`
	// List of ports to expose from the container.
	// Cannot be updated.
	// +optional
	Ports []Port `json:"ports,omitempty"`

	// Additional settings for the VM.
	// Cannot be updated.
	// +optional
	Settings *GuestSettings `json:"settings,omitempty"`
}

type GuestSettings struct {
	// Individual lines to add to a sysctl.conf file. See sysctl.conf(5) for more
	// +optional
	Sysctl []string `json:"sysctl,omitempty"`
}

type CPUs struct {
	// +optional
	// +kubebuilder:default:=1
	Min *MilliCPU `json:"min"`
	// +optional
	Max *MilliCPU `json:"max,omitempty"`
	// +optional
	Use *MilliCPU `json:"use,omitempty"`
}

// MilliCPU is a special type to represent vCPUs * 1000
// e.g. 2 vCPU is 2000, 0.25 is 250
//
// +kubebuilder:validation:XIntOrString
// +kubebuilder:validation:Pattern=^[0-9]+((\.[0-9]*)?|m)
type MilliCPU uint32 // note: pattern is more restrictive than resource.Quantity, because we're just using it for CPU

// RoundedUp returns the smallest integer number of CPUs greater than or equal to the effective
// value of m.
func (m MilliCPU) RoundedUp() uint32 {
	r := uint32(m) / 1000
	if m%1000 != 0 {
		r += 1
	}
	return r
}

// MilliCPUFromResourceQuantity converts resource.Quantity into MilliCPU
func MilliCPUFromResourceQuantity(r resource.Quantity) MilliCPU {
	return MilliCPU(r.MilliValue())
}

// ToResourceQuantity converts a MilliCPU to resource.Quantity
// this is useful for formatting/serialization
func (m MilliCPU) ToResourceQuantity() *resource.Quantity {
	return resource.NewMilliQuantity(int64(m), resource.BinarySI)
}

// AsFloat64 converts the MilliCPU value into a float64 of CPU
//
// This should be preferred over calling m.ToResourceQuantity().AsApproximateFloat64(), because
// going through the resource.Quantity can produce less accurate floats.
func (m MilliCPU) AsFloat64() float64 {
	return float64(m) / 1000
}

// this is used to parse scheduler config and communication between components
// we used resource.Quantity as underlying transport format for MilliCPU
func (m *MilliCPU) UnmarshalJSON(data []byte) error {
	var quantity resource.Quantity
	err := json.Unmarshal(data, &quantity)
	if err != nil {
		return err
	}

	*m = MilliCPUFromResourceQuantity(quantity)
	return nil
}

func (m MilliCPU) MarshalJSON() ([]byte, error) {
	// Mashal as an integer if we can, for backwards-compatibility with components that wouldn't be
	// expecting a string here.
	if m%1000 == 0 {
		return json.Marshal(uint32(m / 1000))
	}

	return json.Marshal(m.ToResourceQuantity())
}

func (m MilliCPU) Format(state fmt.State, verb rune) {
	switch {
	case verb == 'v' && state.Flag('#'):
		state.Write([]byte(fmt.Sprintf("%v", uint32(m))))
	default:
		state.Write([]byte(fmt.Sprintf("%v", m.AsFloat64())))
	}
}

type MemorySlots struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	// +kubebuilder:validation:ExclusiveMaximum=false
	// +optional
	// +kubebuilder:default:=1
	Min *int32 `json:"min"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	// +kubebuilder:validation:ExclusiveMaximum=false
	// +optional
	Max *int32 `json:"max,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	// +kubebuilder:validation:ExclusiveMaximum=false
	// +optional
	Use *int32 `json:"use,omitempty"`
}

type RootDisk struct {
	Image string `json:"image"`
	// +optional
	Size resource.Quantity `json:"size,omitempty"`
	// +optional
	// +kubebuilder:default:="IfNotPresent"
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy"`
	// +optional
	Execute []string `json:"execute,omitempty"`
}

type EnvVar struct {
	// Name of the environment variable. Must be a C_IDENTIFIER.
	Name string `json:"name"`
	// +optional
	// +kubebuilder:default:=""
	Value string `json:"value,omitempty"`
}

type Port struct {
	// If specified, this must be an IANA_SVC_NAME and unique within the pod. Each
	// named port in a pod must have a unique name. Name for the port that can be
	// referred to by services.
	Name string `json:"name,omitempty"`
	// Number of port to expose on the pod's IP address.
	// This must be a valid port number, 0 < x < 65536.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int `json:"port"`
	// Protocol for port. Must be UDP or TCP.
	// Defaults to "TCP".
	// +kubebuilder:default:=TCP
	Protocol Protocol `json:"protocol,omitempty"`
}

type Protocol string

const (
	// ProtocolTCP is the TCP protocol.
	ProtocolTCP Protocol = "TCP"
	// ProtocolUDP is the UDP protocol.
	ProtocolUDP Protocol = "UDP"
)

type Disk struct {
	//Disk's name.
	// Must be a DNS_LABEL and unique within the virtual machine.
	Name string `json:"name"`
	// Mounted read-only if true, read-write otherwise (false or unspecified).
	// Defaults to false.
	// +optional
	// +kubebuilder:default:=false
	ReadOnly *bool `json:"readOnly,omitempty"`
	// Path within the virtual machine at which the disk should be mounted.  Must
	// not contain ':'.
	MountPath string `json:"mountPath"`
	// DiskSource represents the location and type of the mounted disk.
	DiskSource `json:",inline"`
}

type DiskSource struct {
	// EmptyDisk represents a temporary empty qcow2 disk that shares a vm's lifetime.
	EmptyDisk *EmptyDiskSource `json:"emptyDisk,omitempty"`
	// configMap represents a configMap that should populate this disk
	// +optional
	ConfigMap *corev1.ConfigMapVolumeSource `json:"configMap,omitempty"`
	// Secret represents a secret that should populate this disk.
	// +optional
	Secret *corev1.SecretVolumeSource `json:"secret,omitempty"`
	// TmpfsDisk represents a tmpfs.
	// +optional
	Tmpfs *TmpfsDiskSource `json:"tmpfs,omitempty"`
}

type EmptyDiskSource struct {
	Size resource.Quantity `json:"size"`
	// Discard enables the "discard" mount option for the filesystem
	Discard bool `json:"discard,omitempty"`
}

type TmpfsDiskSource struct {
	Size resource.Quantity `json:"size"`
}

type ExtraNetwork struct {
	// Enable extra network interface
	// +kubebuilder:default:=false
	// +optional
	Enable bool `json:"enable"`
	// Interface name.
	// +kubebuilder:default:=net1
	// +optional
	Interface string `json:"interface"`
	// Multus Network name specified in network-attachments-definition.
	// +optional
	MultusNetwork string `json:"multusNetwork,omitempty"`
}

// VirtualMachineStatus defines the observed state of VirtualMachine
type VirtualMachineStatus struct {
	// Represents the observations of a VirtualMachine's current state.
	// VirtualMachine.status.conditions.type are: "Available", "Progressing", and "Degraded"
	// VirtualMachine.status.conditions.status are one of True, False, Unknown.
	// VirtualMachine.status.conditions.reason the value should be a CamelCase string and producers of specific
	// condition types may define expected values and meanings for this field, and whether the values
	// are considered a guaranteed API.
	// VirtualMachine.status.conditions.Message is a human readable message indicating details about the transition.
	// For further information see: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// The phase of a VM is a simple, high-level summary of where the VM is in its lifecycle.
	// +optional
	Phase VmPhase `json:"phase,omitempty"`
	// +optional
	PodName string `json:"podName,omitempty"`
	// +optional
	PodIP string `json:"podIP,omitempty"`
	// +optional
	ExtraNetIP string `json:"extraNetIP,omitempty"`
	// +optional
	ExtraNetMask string `json:"extraNetMask,omitempty"`
	// +optional
	Node string `json:"node,omitempty"`
	// +optional
	CPUs *MilliCPU `json:"cpus,omitempty"`
	// +optional
	MemorySize *resource.Quantity `json:"memorySize,omitempty"`
}

type VmPhase string

const (
	// VmPending means the VM has been accepted by the system, but vm-runner pod
	// has not been started. This includes time before being bound to a node, as well as time spent
	// pulling images onto the host.
	VmPending VmPhase = "Pending"
	// VmRunning means the vm-runner pod has been bound to a node and have been started.
	VmRunning VmPhase = "Running"
	// VmSucceeded means that all containers in the vm-runner pod have voluntarily terminated
	// with a container exit code of 0, and the system is not going to restart any of these containers.
	VmSucceeded VmPhase = "Succeeded"
	// VmFailed means that all containers in the vm-runner pod have terminated, and at least one container has
	// terminated in a failure (exited with a non-zero exit code or was stopped by the system).
	VmFailed VmPhase = "Failed"
	// VmPreMigrating means that VM in preparation to start migration
	VmPreMigrating VmPhase = "PreMigrating"
	// VmMigrating means that VM in migration to another node
	VmMigrating VmPhase = "Migrating"
	// VmScaling means that devices are plugging/unplugging to/from the VM
	VmScaling VmPhase = "Scaling"
)

// IsAlive returns whether the guest in the VM is expected to be running
func (p VmPhase) IsAlive() bool {
	switch p {
	case VmRunning, VmPreMigrating, VmMigrating, VmScaling:
		return true
	default:
		return false
	}
}

//+genclient
//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:singular=neonvm

// VirtualMachine is the Schema for the virtualmachines API
// +kubebuilder:printcolumn:name="Cpus",type=string,JSONPath=`.status.cpus`
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.status.memorySize`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.podName`
// +kubebuilder:printcolumn:name="ExtraIP",type=string,JSONPath=`.status.extraNetIP`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Node",type=string,priority=1,JSONPath=`.status.node`
// +kubebuilder:printcolumn:name="Image",type=string,priority=1,JSONPath=`.spec.guest.rootDisk.image`
type VirtualMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualMachineSpec   `json:"spec,omitempty"`
	Status VirtualMachineStatus `json:"status,omitempty"`
}

func (vm *VirtualMachine) Cleanup() {
	vm.Status.PodName = ""
	vm.Status.PodIP = ""
	vm.Status.Node = ""
	vm.Status.CPUs = nil
	vm.Status.MemorySize = nil
	vm.Status.Phase = ""
}

//+kubebuilder:object:root=true

// VirtualMachineList contains a list of VirtualMachine
type VirtualMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualMachine{}, &VirtualMachineList{})
}
