package api

import (
	"fmt"
)

// PodName represents the namespaced name of a pod
//
// Some analogous types already exist elsewhere (e.g., in the kubernetes API packages), but they
// don't provide satisfactory JSON marshal/unmarshalling.
type PodName struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

func (n PodName) String() string {
	return fmt.Sprintf("%s:%s", n.Namespace, n.Name)
}

/////////////////////////////////
// (Autoscaler) Agent Messages //
/////////////////////////////////

// AgentRequest is the type of message sent from an autoscaler-agent to the scheduler plugin
//
// All AgentRequests expect a PluginResponse.
type AgentRequest struct {
	// Pod is the namespaced name of the pod making the request
	Pod PodName `json:"pod"`
	// Resources gives a requested or notified change in resources allocated to the VM.
	//
	// The requested amount MAY be equal to the current amount, in which case it serves as a
	// notification that the VM should no longer be contributing to resource pressure.
	//
	// TODO: allow passing nil here if nothing's changed (i.e., the request would be the same as the
	// previous request)
	Resources Resources `json:"resources"`
	// Metrics provides information about the VM's current load, so that the scheduler may
	// prioritize which pods to migrate
	Metrics Metrics `json:"metrics"`
}

// Resources represents an amount of CPU and memory (via memory slots)
//
// When used in an AgentRequest, it represents the desired total amount of resources. When
// a resource is increasing, the autoscaler-agent "requests" the change to confirm that the
// resources are available. When decreasing, the autoscaler-agent is expected to use Resources to
// "notify" the scheduler -- i.e., the resource amount should have already been decreased. When
// a resource stays at the same amount, the associated AgentRequest serves to indicate that the
// autoscaler-agent is "satisfied" with its current resources, and should no longer contribute to
// any existing resource pressure.
//
// When used a PluginResponse (as a Permit), then the Resources serves to inform the
// autoscaler-agent of the amount it has been permitted to use, subject to node resource limits.
//
// In all cases, each resource type is considered separately from the others.
type Resources struct {
	VCPU uint16 `json:"vCPUs"`
	// Mem gives the number of slots of memory (typically 1G ea.) requested. The slot size is set by
	// the value of the VM's VirtualMachineSpec.Guest.MemorySlotSize.
	Mem uint16 `json:"mem"`
}

// ValidateNonZero checks that neither of the Resources fields are equal to zero, returning an error
// if either is.
func (r Resources) ValidateNonZero() error {
	if r.VCPU == 0 {
		return fmt.Errorf("vCPUs must be non-zero")
	} else if r.Mem == 0 {
		return fmt.Errorf("mem must be non-zero")
	}

	return nil
}

/////////////////////////////////
// (Scheduler) Plugin Messages //
/////////////////////////////////

type PluginResponse struct {
	// Permit provides an upper bound on the resources that the VM is now allowed to consume
	//
	// If the request's Resources were less than or equal its current resources, then the Permit
	// will exactly equal those resources. Otherwise, it may contain resource allocations anywhere
	// between the current and requested resources, inclusive.
	Permit Resources `json:"permit"`

	// Migrate, if present, notifies the autoscaler-agent that its VM will be migrated away,
	// alongside whatever other information may be useful.
	Migrate *MigrateResponse `json:"migrate,omitempty"`

	// ComputeUnit is the minimum unit of resources that the scheduler is expecting to work with
	//
	// For example, if ComputeUnit is Resources{ VCPU: 2, Mem: 4 }, then all VMs are expected to
	// have allocated vCPUs that are a multiple of two, with twice as many memory slots.
	//
	// This value may be different across nodes, and the scheduler expects that all AgentRequests
	// will abide by the most recent ComputeUnit they've received.
	ComputeUnit Resources `json:"resourceUnit"`
}

// MigrateResponse, when provided, is a notification to the autsocaler-agent that it will migrate
//
// After receiving a MigrateResponse, the autoscaler-agent MUST NOT change its resource allocation.
//
// TODO: fill this with more information as required
type MigrateResponse struct{}
