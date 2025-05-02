package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IPPoolSpec defines the desired state of IPPool
type IPPoolSpec struct {
	// Range is a RFC 4632/4291-style string that represents an IP address and prefix length in CIDR notation
	Range string `json:"range"`
	// Allocations is the set of allocated IPs for the given range. Its` indices are a direct mapping to the
	// IP with the same index/offset for the pool's range.
	Allocations map[string]IPAllocation `json:"allocations"`

	// QuarantinedOffsets is the set of offsets that are quarantined from allocation.
	// This is required to allow time for the FDB to get cleared out.
	QuarantinedOffsets []string `json:"quarantinedOffsets,omitempty"`

	// QuarantinedOffsetsPending is the set of offsets that are pending quarantine.
	// We first collect IPs in this list, so that every IP is quarantined for the full period.
	QuarantinedOffsetsPending []string `json:"quarantinedOffsetsPending,omitempty"`

	// QuarantineStartTime is the time that the quarantine period started.
	QuarantineStartTime metav1.Time `json:"quarantineStartTime,omitempty"`
}

// IPAllocation represents metadata about the owner of a specific IP.
//
// When VirtualMachine owns the IP, the OwnerID is the VM's NamespacedName.
type IPAllocation struct {
	OwnerID string `json:"id"`
}

//+genclient
//+kubebuilder:object:root=true
//+kubebuilder:resource:singular=ippool

// IPPool is the Schema for the ippools API
type IPPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IPPoolSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// IPPoolList contains a list of IPPool
type IPPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IPPool{}, &IPPoolList{}) //nolint:exhaustruct // just being used to provide the types
}
