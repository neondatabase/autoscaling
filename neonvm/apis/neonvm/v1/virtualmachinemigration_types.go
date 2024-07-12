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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const MigrationPort int32 = 20187

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// VirtualMachineMigrationSpec defines the desired state of VirtualMachineMigration
type VirtualMachineMigrationSpec struct {
	VmName string `json:"vmName"`

	// TODO: not implemented
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// TODO: not implemented
	// +optional
	NodeAffinity *corev1.NodeAffinity `json:"nodeAffinity,omitempty"`

	// +optional
	// +kubebuilder:default:=true
	PreventMigrationToSameHost bool `json:"preventMigrationToSameHost"`

	// TODO: not implemented
	// Set 1 hour as default timeout for migration
	// +optional
	// +kubebuilder:default:=3600
	CompletionTimeout int32 `json:"completionTimeout"`

	// Trigger incremental disk copy migration by default, otherwise full disk copy used in migration
	// +optional
	// +kubebuilder:default:=true
	Incremental bool `json:"incremental"`

	// Use PostCopy migration by default
	// +optional
	// +kubebuilder:default:=false
	AllowPostCopy bool `json:"allowPostCopy"`

	// Use Auto converge by default
	// +optional
	// +kubebuilder:default:=true
	AutoConverge bool `json:"autoConverge"`

	// Set 1 Gbyte/sec as default for migration bandwidth
	// +optional
	// +kubebuilder:default:="1Gi"
	MaxBandwidth resource.Quantity `json:"maxBandwidth"`
}

// VirtualMachineMigrationStatus defines the observed state of VirtualMachineMigration
type VirtualMachineMigrationStatus struct {
	// Represents the observations of a VirtualMachineMigration's current state.
	// VirtualMachineMigration.status.conditions.type are: "Available", "Progressing", and "Degraded"
	// VirtualMachineMigration.status.conditions.status are one of True, False, Unknown.
	// VirtualMachineMigration.status.conditions.reason the Value should be a CamelCase string and producers of specific
	// condition types may define expected values and meanings for this field, and whether the values
	// are considered a guaranteed API.
	// VirtualMachineMigration.status.conditions.Message is a human readable message indicating details about the transition.
	// For further information see: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// The phase of a VM is a simple, high-level summary of where the VM is in its lifecycle.
	// +optional
	Phase VmmPhase `json:"phase,omitempty"`
	// +optional
	SourcePodName string `json:"sourcePodName,omitempty"`
	// +optional
	TargetPodName string `json:"targetPodName,omitempty"`
	// +optional
	SourcePodIP string `json:"sourcePodIP,omitempty"`
	// +optional
	TargetPodIP string `json:"targetPodIP,omitempty"`
	// +optional
	SourceNode string `json:"sourceNode,omitempty"`
	// +optional
	TargetNode string `json:"targetNode,omitempty"`
	// +optional
	Info MigrationInfo `json:"info,omitempty"`
}

type MigrationInfo struct {
	// +optional
	Status string `json:"status,omitempty"`
	// +optional
	TotalTimeMs int64 `json:"totalTimeMs,omitempty"`
	// +optional
	SetupTimeMs int64 `json:"setupTimeMs,omitempty"`
	// +optional
	DowntimeMs int64 `json:"downtimeMs,omitempty"`
	// +optional
	Ram MigrationInfoRam `json:"ram,omitempty"`
	// +optional
	Compression MigrationInfoCompression `json:"compression,omitempty"`
}

type MigrationInfoRam struct {
	// +optional
	Transferred int64 `json:"transferred,omitempty"`
	// +optional
	Remaining int64 `json:"remaining,omitempty"`
	// +optional
	Total int64 `json:"total,omitempty"`
}

type MigrationInfoCompression struct {
	// +optional
	CompressedSize int64 `json:"compressedSize,omitempty"`
	// +optional
	CompressionRate int64 `json:"compressionRate,omitempty"`
}

type VmmPhase string

const (
	// VmmPending means the migration has been accepted by the system, but target vm-runner pod
	// has not been started. This includes time before being bound to a node, as well as time spent
	// pulling images onto the host.
	VmmPending VmmPhase = "Pending"
	// VmmRunning means the target vm-runner pod has been bound to a node and have been started.
	VmmRunning VmmPhase = "Running"
	// VmmSucceeded means that migration finisged with success
	VmmSucceeded VmmPhase = "Succeeded"
	// VmmFailed means that migration failed
	VmmFailed VmmPhase = "Failed"
)

//+genclient
//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:singular=neonvmm

// VirtualMachineMigration is the Schema for the virtualmachinemigrations API
// +kubebuilder:printcolumn:name="VM",type=string,JSONPath=`.spec.vmName`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.status.sourcePodName`
// +kubebuilder:printcolumn:name="SourceIP",type=string,priority=1,JSONPath=`.status.sourcePodIP`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.status.targetPodName`
// +kubebuilder:printcolumn:name="TargetIP",type=string,priority=1,JSONPath=`.status.targetPodIP`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type VirtualMachineMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualMachineMigrationSpec   `json:"spec,omitempty"`
	Status VirtualMachineMigrationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VirtualMachineMigrationList contains a list of VirtualMachineMigration
type VirtualMachineMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualMachineMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualMachineMigration{}, &VirtualMachineMigrationList{}) //nolint:exhaustruct // just being used to provide the types
}
