package api

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/neondatabase/autoscaling/pkg/util"
)

/////////////////////////////////
// (Autoscaler) Agent Messages //
/////////////////////////////////

// PluginProtoVersion represents a single version of the agent<->scheduler plugin protocol
//
// Each version of the agent<->scheduler plugin protocol is named independently from releases of the
// repository containing this code. Names follow semver, although this does not necessarily
// guarantee support - for example, the plugin may only support a single version, even though others
// may appear to be semver-compatible.
//
// Version compatibility is documented in the neighboring file VERSIONING.md.
type PluginProtoVersion uint32

const (
	// PluginProtoV1_0 represents v1.0 of the agent<->scheduler plugin protocol - the initial
	// version.
	//
	// Last used in release version v0.1.8.
	PluginProtoV1_0 PluginProtoVersion = iota + 1 // start from zero, for backwards compatibility with pre-versioned messages

	// PluginProtoV1_1 represents v1.1 of the agent<->scheduler plugin protocol.
	//
	// Changes from v1.0:
	//
	// * Allows a nil value of the AgentRequest.Metrics field.
	//
	// Currently the latest version.
	PluginProtoV1_1

	// latestPluginProtoVersion represents the latest version of the agent<->scheduler plugin
	// protocol
	//
	// This value is kept private because it should not be used externally; any desired
	// functionality that could be implemented with it should instead be a method on
	// PluginProtoVersion.
	latestPluginProtoVersion PluginProtoVersion = iota // excluding +1 makes it equal to previous
)

func (v PluginProtoVersion) String() string {
	var zero PluginProtoVersion

	switch v {
	case zero:
		return "<invalid: zero>"
	case PluginProtoV1_0:
		return "v1.0"
	case PluginProtoV1_1:
		return "v1.1"
	default:
		diff := v - latestPluginProtoVersion
		return fmt.Sprintf("<unknown = %v + %d>", latestPluginProtoVersion, diff)
	}
}

// IsValid returns whether the protocol version is valid. The zero value is not valid.
func (v PluginProtoVersion) IsValid() bool {
	return uint(v) != 0
}

// AllowsNilMetrics returns whether this version of the protocol allows the autoscaler-agent to send
// a nil metrics field.
//
// This is true for version v1.1 and greater.
func (v PluginProtoVersion) AllowsNilMetrics() bool {
	return v >= PluginProtoV1_1
}

// AgentRequest is the type of message sent from an autoscaler-agent to the scheduler plugin
//
// All AgentRequests expect a PluginResponse.
type AgentRequest struct {
	// ProtoVersion is the version of the protocol that the autoscaler-agent is expecting to use
	//
	// If the scheduler does not support this version, then it will respond with a 400 status.
	ProtoVersion PluginProtoVersion `json:"protoVersion"`
	// Pod is the namespaced name of the pod making the request
	Pod util.NamespacedName `json:"pod"`
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
	//
	// In some protocol versions, this field may be nil.
	Metrics *Metrics `json:"metrics"`
}

// ProtocolRange returns a VersionRange exactly equal to r.ProtoVersion
func (r AgentRequest) ProtocolRange() VersionRange[PluginProtoVersion] {
	return VersionRange[PluginProtoVersion]{
		Min: r.ProtoVersion,
		Max: r.ProtoVersion,
	}
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
	VCPU MilliCPU `json:"vCPUs"`
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

func (r Resources) CheckValuesAreReasonablySized() error {
	if r.VCPU < 50 {
		return errors.New("VCPU is smaller than 0.05")
	}
	if r.VCPU > 512*1000 {
		return errors.New("VCPU is bigger than 512")
	}

	return nil
}

// HasFieldGreaterThan returns true if and only if there is a field F where r.F > cmp.F
func (r Resources) HasFieldGreaterThan(cmp Resources) bool {
	return r.VCPU > cmp.VCPU || r.Mem > cmp.Mem
}

// HasFieldGreaterThan returns true if and only if there is a field F where r.F < cmp.F
func (r Resources) HasFieldLessThan(cmp Resources) bool {
	return cmp.HasFieldGreaterThan(r)
}

// Min returns a new Resources value with each field F as the minimum of r.F and cmp.F
func (r Resources) Min(cmp Resources) Resources {
	return Resources{
		VCPU: util.Min(r.VCPU, cmp.VCPU),
		Mem:  util.Min(r.Mem, cmp.Mem),
	}
}

// Max returns a new Resources value with each field F as the maximum of r.F and cmp.F
func (r Resources) Max(cmp Resources) Resources {
	return Resources{
		VCPU: util.Max(r.VCPU, cmp.VCPU),
		Mem:  util.Max(r.Mem, cmp.Mem),
	}
}

// Mul returns the result of multiplying each resource by factor
func (r Resources) Mul(factor uint16) Resources {
	return Resources{
		VCPU: MilliCPU(uint32(factor) * uint32(r.VCPU)),
		Mem:  factor * r.Mem,
	}
}

// Increase returns a MoreResources with each field F true when r.F > old.F.
func (r Resources) IncreaseFrom(old Resources) MoreResources {
	return MoreResources{
		Cpu:    r.VCPU > old.VCPU,
		Memory: r.Mem > old.Mem,
	}
}

// ConvertToRaw produces the RawResources equivalent to these Resources with the given slot size
func (r Resources) ConvertToRaw(memSlotSize *resource.Quantity) RawResources {
	cpuQuantity := r.VCPU.ToResourceQuantity()
	return RawResources{
		Cpu:    &cpuQuantity,
		Memory: resource.NewQuantity(int64(r.Mem)*memSlotSize.Value(), resource.BinarySI),
	}
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

// InformantProtoVersion represents a single version of the agent<->informant protocol
//
// Each version of the agent<->informant protocol is named independently from releases of the
// repository containing this code. Names follow semver, although this does not necessarily
// guarantee support - for example, the VM informant may only support versions above v1.1.
//
// Version compatibility is documented in the neighboring file VERSIONING.md.
type InformantProtoVersion uint32

const (
	// InformantProtoV1_0 represents v1.0 of the agent<->informant protocol - the initial version.
	//
	// Last used in release version 0.1.2.
	InformantProtoV1_0 InformantProtoVersion = iota + 1 // +1 so we start from 1

	// InformantProtoV1_1 represents v1.1 of the agent<->informant protocol.
	//
	// Changes from v1.0:
	//
	// * Adds /try-upscale endpoint to the autoscaler-agent.
	//
	// Currently the latest version.
	InformantProtoV1_1

	// latestInformantProtoVersion represents the latest version of the agent<->informant protocol
	//
	// This value is kept private because it should not be used externally; any desired
	// functionality that could be implemented with it should instead be a method on
	// InformantProtoVersion.
	latestInformantProtoVersion InformantProtoVersion = iota // excluding +1 makes it equal to previous
)

func (v InformantProtoVersion) String() string {
	var zero InformantProtoVersion

	switch v {
	case zero:
		return "<invalid: zero>"
	case InformantProtoV1_0:
		return "v1.0"
	case InformantProtoV1_1:
		return "v1.1"
	default:
		diff := v - latestInformantProtoVersion
		return fmt.Sprintf("<unknown = %v + %d>", latestInformantProtoVersion, diff)
	}
}

// IsValid returns whether the protocol version is valid. The zero value is not valid.
func (v InformantProtoVersion) IsValid() bool {
	return uint(v) != 0
}

// HasTryUpscale returns whether this version of the protocol has the /try-upscale endpoint
//
// This is true for version v1.1 and greater.
func (v InformantProtoVersion) HasTryUpscale() bool {
	return v >= InformantProtoV1_1
}

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
	MinProtoVersion InformantProtoVersion `json:"minProtoVersion"`
	// MaxProtoVersion is the maximum version of the agent<->informant protocol that the
	// autoscaler-agent supports, inclusive.
	//
	// Protocol versions are always non-zero.
	//
	// AgentDesc must always have MinProtoVersion <= MaxProtoVersion.
	MaxProtoVersion InformantProtoVersion `json:"maxProtoVersion"`
}

// ProtocolRange returns a VersionRange from d.MinProtoVersion to d.MaxProtoVersion.
func (d AgentDesc) ProtocolRange() VersionRange[InformantProtoVersion] {
	return VersionRange[InformantProtoVersion]{
		Min: d.MinProtoVersion,
		Max: d.MaxProtoVersion,
	}
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
	ProtoVersion InformantProtoVersion `json:"protoVersion"`

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

// MoreResourcesRequest is the request type wrapping MoreResources that's sent by the VM informant
// to the autoscaler-agent's /try-upscale endpoint when the VM is urgently in need of more
// resources.
type MoreResourcesRequest struct {
	MoreResources

	// ExpectedID is the expected AgentID of the autoscaler-agent
	ExpectedID uuid.UUID `json:"expectedID"`
}

// MoreResources holds the data associated with a MoreResourcesRequest
type MoreResources struct {
	// Cpu is true if the VM informant is requesting more CPU
	Cpu bool `json:"cpu"`
	// Memory is true if the VM informant is requesting more memory
	Memory bool `json:"memory"`
}

// Not returns the field-wise logical "not" of m
func (m MoreResources) Not() MoreResources {
	return MoreResources{
		Cpu:    !m.Cpu,
		Memory: !m.Memory,
	}
}

// And returns the field-wise logical "and" of m and cmp
func (m MoreResources) And(cmp MoreResources) MoreResources {
	return MoreResources{
		Cpu:    m.Cpu && cmp.Cpu,
		Memory: m.Memory && cmp.Memory,
	}
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

type VCPUChange struct {
	VCPUs resource.Quantity
}

type MilliCPU uint32

func MilliCPUFromResourceQuantity(r resource.Quantity) MilliCPU {
	return MilliCPU(r.MilliValue())
}

func (m *MilliCPU) ToResourceQuantity() resource.Quantity {
	return *resource.NewMilliQuantity(int64(*m), resource.BinarySI)
}
