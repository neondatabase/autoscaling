package api

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"k8s.io/apimachinery/pkg/api/resource"
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
		return errors.New("vCPUs must be non-zero")
	} else if r.Mem == 0 {
		return errors.New("mem must be non-zero")
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

///////////////////////////
// VM Informant Messages //
///////////////////////////

// AgentMessage is used for (almost) every message sent from the autoscaler-agent to the VM
// informant, and serves to wrap the type T with a SequenceNumber
//
// The SequenceNumber provides a total ordering of states, even if the ordering of HTTP requests and
// responses are out of order. Fundamentally this is required because we have bidirectional
// communication between the autoscaler-agent and VM informant — without it, we run the risk of racy
// behavior, which could *actually* result in data corruption.
type AgentMessage[T any] struct {
	// Data is the content of the request or response
	Data T `json:"data"`

	// SequenceNumber is a unique-per-instance monotonically increasing number passed in each
	// non-initial message from the autoscaler-agent to the VM informant, both requests and
	// responses.
	SequenceNumber uint64 `json:"sequenceNumber"`
}

// AgentDesc is the first message sent from an autoscaler-agent to a VM informant, describing some
// information about the autoscaler-agent
//
// Each time an autoscaler-agent (re)connects to a VM informant, it sends an AgentDesc to the
// "/register" endpoint.
//
// For more information on the agent<->informant protocol, refer to the top-level ARCHITECTURE.md
type AgentDesc struct {
	// AgentID is a unique UUID for the current instance of the autoscaler-agent
	//
	// This is helpful so that we can distinguish between (incorrect) duplicate calls to /register
	// and (correct) re-registering of an agent.
	AgentID uuid.UUID `json:"agentID"`

	// ServeAddr gives the unique (per instance)
	ServerAddr string `json:"agentServeAddr"`

	// MinProtoVersion is the minimum version of the agent<->informant protocol that the
	// autoscaler-agent supports
	//
	// Protocol versions are always non-zero.
	//
	// AgentDesc must always have MinProtoVersion <= MaxProtoVersion.
	MinProtoVersion uint32 `json:"minProtoVersion"`
	// MaxProtoVersion is the maximum version of the agent<->informant protocol that the
	// autoscaler-agent supports, inclusive.
	//
	// Protocol versions are always non-zero.
	//
	// AgentDesc must always have MinProtoVersion <= MaxProtoVersion.
	MaxProtoVersion uint32 `json:"maxProtoVersion"`
}

type AgentIdentificationMessage = AgentMessage[AgentIdentification]

// AgentIdentification affirms the AgentID of the autoscaler-agent in its initial response to a VM
// informant, on the /id endpoint. This response is always wrapped in an AgentMessage. A type alias
// for this is provided as AgentIdentificationMessage, for convenience.
type AgentIdentification struct {
	// AgentID is the same AgentID as given in the AgentDesc initially provided to the VM informant
	AgentID uuid.UUID `json:"agentID"`
}

// InformantDesc describes the capabilities of a VM informant, in response to an autoscaler-agent's
// request on the /register endpoint
//
// For more information on the agent<->informant protocol, refer to the top-level ARCHITECTURE.md
type InformantDesc struct {
	// ProtoVersion is the version of the agent<->informant protocol that the VM informant has
	// selected
	//
	// If an autoscaler-agent is successfully registered, a well-behaved VM informant MUST respond
	// with a ProtoVersion within the bounds of the agent's declared minimum and maximum protocol
	// versions. If the VM informant does not use a protocol version within those bounds, then it
	// MUST respond with an error status code.
	ProtoVersion uint32 `json:"protoVersion"`

	// MetricsMethod tells the autoscaler-agent how to fetch metrics from the VM
	MetricsMethod InformantMetricsMethod `json:"metricsMethod"`
}

// InformantMetricsMethod collects the options for ways the VM informant can report metrics
//
// At least one method *must* be provided in an InformantDesc, and more than one method gives the
// autoscaler-agent freedom to choose.
//
// We use this type so it's easier to ensure backwards compatibility with previous versions of the
// VM informant — at least during the rollout of new autoscaler-agent or VM informant versions.
type InformantMetricsMethod struct {
	// Prometheus describes prometheus-format metrics, typically not through the informant itself
	Prometheus *MetricsMethodPrometheus `json:"prometheus,omitempty"`
}

// MetricsMethodPrometheus describes VM informant's metrics in the prometheus format, made available
// on a particular port
type MetricsMethodPrometheus struct {
	Port uint16 `json:"port"`
}

// UnregisterAgent is the result of a successful request to a VM informant's /unregister endpoint
type UnregisterAgent struct {
	// WasActive indicates whether the unregistered autoscaler-agent was the one in-use by the VM
	// informant
	WasActive bool `json:"wasActive"`
}

// MoreResources is sent by the VM informant to the autoscaler-agent when the VM is in need of more
// resources of a certain type
type MoreResources struct {
	// Cpu is true if the VM informant is requesting more CPU
	Cpu bool `json:"cpu"`
	// Memory is true if the VM informant is requesting more memory
	Memory bool `json:"memory"`
}

// RawResources signals raw resource amounts, and is primarily used in communications with the VM
// informant because it doesn't know about things like memory slots
type RawResources struct {
	Cpu    *resource.Quantity `json:"cpu"`
	Memory *resource.Quantity `json:"memory"`
}

// DownscaleResult is used by the VM informant to return whether it downscaled successfully, and
// some indication of its status when doing so
type DownscaleResult struct {
	Ok     bool
	Status string
}

// SuspendAgent is sent from the VM informant to the autoscaler-agent when it has been contacted by
// a new autoscaler-agent and wishes to switch to that instead
//
// Instead of just cutting off any connection(s) to the agent, the informant keeps it around in case
// the new one fails and it needs to fall back to the old one.
type SuspendAgent struct {
	ExpectedID uuid.UUID `json:"expectedID"`
}

// ResumeAgent is sent from the VM informant to the autoscaler-agent to resume contact when it was
// previously suspended.
type ResumeAgent struct {
	ExpectedID uuid.UUID `json:"expectedID"`
}
