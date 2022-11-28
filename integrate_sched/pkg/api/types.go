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

// Resources represents either a request for additional resources (given by the total amount),
// a notification that the autoscaler-agent has independently decreased its resource usage, or a
// notification that the autoscaler-agent is satisfied with its current resources.
type Resources struct {
	VCPU uint16 `json:"vCPUs"`
}

/////////////////////////////////
// (Scheduler) Plugin Messages //
/////////////////////////////////

type PluginResponse struct {
	// Permit provides an upper bound on the resources that the VM is now allowed to consume
	//
	// If the request's Resources were less than or equal its current resources, then the
	// ResourcePermit will exactly equal those resources. Otherwise, it may contain resource
	// allocations anywhere between the current and requested resources, inclusive.
	Permit ResourcePermit `json:"permit"`

	// Migrate, if present, signifies an instruction to the autoscaler-agent that it should begin
	// migrating, with the necessary information contained within the response
	Migrate *MigrateResponse `json:"migrate,omitempty"`
}

// ResourcePermit represents the response to a ResourceRequest, informing the autoscaler-agent of
// the amount of resources it has been permitted to use
//
// If the original ResourceRequest was merely notifying the scheduler plugin of an independent
// decrease (or remain the same), then the values in the ResourcePermit will match the request.
//
// If it was requesting an increase in resources, then the ResourcePermit will contain resource
// amounts greater than or equal to the previous values and less than or equal to the requested
// values. In other words, the ResourcePermit may have resource amounts anywhere between the current
// and requested values, inclusive.
type ResourcePermit struct {
	VCPU uint16 `json:"vCPUs"`
}

// MigrateResponse, when provided, is a notification to the autsocaler-agent that it will migrate
//
// After receiving a MigrateResponse, the autoscaler-agent MUST NOT change its resource allocation.
//
// TODO: fill this with more information as required
type MigrateResponse struct{}
