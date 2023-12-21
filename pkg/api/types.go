package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"go.uber.org/zap/zapcore"

	"k8s.io/apimachinery/pkg/api/resource"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

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
	// Last used in release version v0.6.0.
	PluginProtoV1_1

	// PluginProtoV2_0 represents v2.0 of the agent<->scheduler plugin protocol.
	//
	// Changes from v1.1:
	//
	// * Supports fractional CPU
	//
	// Last used in release version v0.19.x.
	PluginProtoV2_0

	// PluginProtoV2_1 represents v2.1 of the agent<->scheduler plugin protocol.
	//
	// Changes from v2.0:
	//
	// * added AgentRequest.LastPermit
	//
	// Currently the latest version.
	PluginProtoV2_1

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
	case PluginProtoV2_0:
		return "v2.0"
	case PluginProtoV2_1:
		return "v2.1"
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

func (v PluginProtoVersion) SupportsFractionalCPU() bool {
	return v >= PluginProtoV2_0
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
	// LastPermit indicates the last permit that the agent has received from the scheduler plugin.
	// In case of a failure, the new running scheduler uses LastPermit to recover the previous state.
	// LastPermit may be nil.
	LastPermit *Resources `json:"lastPermit"`
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
	VCPU vmapi.MilliCPU `json:"vCPUs"`
	// Mem gives the number of slots of memory (typically 1G ea.) requested. The slot size is set by
	// the value of the VM's VirtualMachineSpec.Guest.MemorySlotSize.
	Mem uint16 `json:"mem"`
}

// MarshalLogObject implements zapcore.ObjectMarshaler, so that Resources can be used with zap.Object
func (r Resources) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("vCPU", fmt.Sprintf("%v", r.VCPU))
	enc.AddUint16("mem", r.Mem)
	return nil
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
		VCPU: vmapi.MilliCPU(factor) * r.VCPU,
		Mem:  factor * r.Mem,
	}
}

// AbsDiff returns a new Resources with each field F as the absolute value of the difference between
// r.F and cmp.F
func (r Resources) AbsDiff(cmp Resources) Resources {
	return Resources{
		VCPU: util.AbsDiff(r.VCPU, cmp.VCPU),
		Mem:  util.AbsDiff(r.Mem, cmp.Mem),
	}
}

// Increase returns a MoreResources with each field F true when r.F > old.F.
func (r Resources) IncreaseFrom(old Resources) MoreResources {
	return MoreResources{
		Cpu:    r.VCPU > old.VCPU,
		Memory: r.Mem > old.Mem,
	}
}

// ConvertToRaw produces the Allocation equivalent to these Resources with the given slot size
func (r Resources) ConvertToAllocation(memSlotSize *resource.Quantity) Allocation {
	return Allocation{
		Cpu: r.VCPU.ToResourceQuantity().AsApproximateFloat64(),
		Mem: uint64(int64(r.Mem) * memSlotSize.Value()),
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

// MoreResources holds the data associated with a MoreResourcesRequest
type MoreResources struct {
	// Cpu is true if the vm-monitor is requesting more CPU
	Cpu bool `json:"cpu"`
	// Memory is true if the vm-monitor is requesting more memory
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

////////////////////////////////////
// Controller <-> Runner Messages //
////////////////////////////////////

// VCPUChange is used to notify runner that it had some changes in its CPUs
// runner uses this info to adjust qemu cgroup
type VCPUChange struct {
	VCPUs vmapi.MilliCPU
}

// VCPUCgroup is used in runner to reply to controller
// it represents the vCPU usage as controlled by cgroup
type VCPUCgroup struct {
	VCPUs vmapi.MilliCPU
}

// this a similar version type for controller <-> runner communications
// see PluginProtoVersion comment for details
type RunnerProtoVersion uint32

const (
	RunnerProtoV1 RunnerProtoVersion = iota + 1
)

func (v RunnerProtoVersion) SupportsCgroupFractionalCPU() bool {
	return v >= RunnerProtoV1
}

////////////////////////////////////
//   Agent <-> Monitor Messages   //
////////////////////////////////////

// Represents the resources that a VM has been granted
type Allocation struct {
	// Number of vCPUs
	Cpu float64 `json:"cpu"`

	// Number of bytes
	Mem uint64 `json:"mem"`
}

// ** Types sent by monitor **

// This type is sent to the agent as a way to request immediate upscale.
// Since the agent cannot control if the agent will choose to upscale the VM,
// it does not return anything. If an upscale is granted, the agent will notify
// the monitor via an UpscaleConfirmation
type UpscaleRequest struct{}

// This type is sent to the agent to confirm it successfully upscaled, meaning
// it increased its filecache and/or cgroup memory limits. The agent does not
// need to respond.
type UpscaleConfirmation struct{}

// This type is sent to the agent to indicate if downscaling was successful. The
// agent does not need to respond.
type DownscaleResult struct {
	Ok     bool
	Status string
}

// ** Types sent by agent **

// This type is sent to the monitor to inform it that it has been granted a geater
// allocation. Once the monitor is done applying this new allocation (i.e, increasing
// file cache size, cgroup memory limits) it should reply with an UpscaleConfirmation.
type UpscaleNotification struct {
	Granted Allocation `json:"granted"`
}

// This type is sent to the monitor as a request to downscale its resource usage.
// Once the monitor has downscaled or failed to do so, it should respond with a
// DownscaleResult.
type DownscaleRequest struct {
	Target Allocation `json:"target"`
}

// ** Types shared by agent and monitor **

// This type can be sent by either party whenever they receive a message they
// cannot deserialize properly.
type InvalidMessage struct {
	Error string `json:"error"`
}

// This type can be sent by either party to signal that an error occurred carrying
// out the other party's request, for example, the monitor erroring while trying
// to downscale. The receiving party can they log the error or propagate it as they
// see fit.
type InternalError struct {
	Error string `json:"error"`
}

// This type is sent as part of a bidirectional heartbeat between the monitor and
// agent. The check is initiated by the agent.
type HealthCheck struct{}

// This function is used to prepare a message for serialization. Any data passed
// to the monitor should be serialized with this function. As of protocol v1.0,
// the following types maybe be sent to the monitor, and thus passed in:
// - DownscaleRequest
// - UpscaleNotification
// - InvalidMessage
// - InternalError
// - HealthCheck
func SerializeMonitorMessage(content any, id uint64) ([]byte, error) {
	// The final type that gets sent over the wire
	type Bundle struct {
		Content any    `json:"content"`
		Type    string `json:"type"`
		Id      uint64 `json:"id"`
	}

	var typeStr string
	switch content.(type) {
	case DownscaleRequest:
		typeStr = "DownscaleRequest"
	case UpscaleNotification:
		typeStr = "UpscaleNotification"
	case InvalidMessage:
		typeStr = "InvalidMessage"
	case InternalError:
		typeStr = "InternalError"
	case HealthCheck:
		typeStr = "HealthCheck"
	default:
		return nil, fmt.Errorf("unknown message type \"%s\"", reflect.TypeOf(content))
	}

	return json.Marshal(Bundle{
		Content: content,
		Type:    typeStr,
		Id:      id,
	})
}

// MonitorProtoVersion represents a single version of the agent<->monitor protocol
//
// Each version of the agent<->monitor protocol is named independently from releases of the
// repository containing this code. Names follow semver, although this does not necessarily
// guarantee support - for example, the monitor may only support versions above v1.1.
//
// Version compatibility is documented in the neighboring file VERSIONING.md.
type MonitorProtoVersion uint32

const (
	// MonitorProtoV1_0 represents v1.0 of the agent<->monitor protocol - the initial version.
	//
	// Currently the latest version.
	MonitorProtoV1_0 = iota + 1

	// latestMonitorProtoVersion represents the latest version of the agent<->Monitor protocol
	//
	// This value is kept private because it should not be used externally; any desired
	// functionality that could be implemented with it should instead be a method on
	// MonitorProtoVersion.
	latestMonitorProtoVersion MonitorProtoVersion = iota // excluding +1 makes it equal to previous
)

func (v MonitorProtoVersion) String() string {
	var zero MonitorProtoVersion

	switch v {
	case zero:
		return "<invalid: zero>"
	case MonitorProtoV1_0:
		return "v1.0"
	default:
		diff := v - latestMonitorProtoVersion
		return fmt.Sprintf("<unknown = %v + %d>", latestMonitorProtoVersion, diff)
	}
}

// Sent back by the monitor after figuring out what protocol version we should use
type MonitorProtocolResponse struct {
	// If `Error` is nil, contains the value of the settled on protocol version.
	// Otherwise, will be set to 0 (MonitorProtocolVersion's zero value).
	Version MonitorProtoVersion `json:"version,omitempty"`

	// Will be nil if no error occurred.
	Error *string `json:"error,omitempty"`
}
