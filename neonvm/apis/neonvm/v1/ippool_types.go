package v1

import (
	"fmt"
	"math/big"
	"net"
	"net/netip"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Unit represents an empty struct for use in sets/maps
type Unit struct{}

// IPPoolSpec defines the desired state of IPPool
type IPPoolSpec struct {
	// Range is a RFC 4632/4291-style string that represents an IP address and prefix length in CIDR notation
	Range string `json:"range"`

	// Managed is the set of IPs that are managed by the IPAM manager.
	Managed map[string]Unit `json:"managed"`

	// Deprecated: This field is deprecated and will be removed in a future version.
	// Allocations is the set of allocated IPs for the given range. Its` indices are a direct mapping to the
	// IP with the same index/offset for the pool's range.
	// +kubebuilder:validation:Optional
	Allocations map[string]IPAllocation `json:"allocations"`
}

// Deprecated: This type is deprecated and will be removed in a future version.
// IPAllocation represents metadata about the VM owner of a specific IP.
type IPAllocation struct {
	VMID string `json:"id"`
}

func (a *IPPoolSpec) Normalize() error {
	if len(a.Managed) > 0 {
		// Pool is already initialized
		return nil
	}

	a.Managed = make(map[string]Unit)

	ip, _, err := net.ParseCIDR(a.Range)
	if err != nil {
		return fmt.Errorf("invalid range: %w", err)
	}

	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("invalid range: %s", a.Range)
	}
	baseInt := new(big.Int).SetBytes(ip4)
	for offset := range a.Allocations {
		offsetInt, ok := new(big.Int).SetString(offset, 10)
		if !ok {
			return fmt.Errorf("invalid offset: %s", offset)
		}

		ip, ok := netip.AddrFromSlice(baseInt.Add(baseInt, offsetInt).Bytes())
		if !ok {
			return fmt.Errorf("invalid ip: %s", ip)
		}

		a.Managed[ip.String()] = Unit{}
	}

	return nil
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
