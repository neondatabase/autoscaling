// API-relevant types extracted from NeonVM VMs

package api

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/samber/lo"
	"github.com/tychoish/fun/erc"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/util"
)

const (
	LabelEnableAutoMigration      = "autoscaling.neon.tech/auto-migration-enabled"
	LabelTestingOnlyAlwaysMigrate = "autoscaling.neon.tech/testing-only-always-migrate"
	LabelEnableAutoscaling        = "autoscaling.neon.tech/enabled"
	AnnotationAutoscalingBounds   = "autoscaling.neon.tech/bounds"
	AnnotationAutoscalingConfig   = "autoscaling.neon.tech/config"
	AnnotationBillingEndpointID   = "autoscaling.neon.tech/billing-endpoint-id"
)

func hasTrueLabel(obj metav1.ObjectMetaAccessor, labelName string) bool {
	labels := obj.GetObjectMeta().GetLabels()
	value, ok := labels[labelName]
	return ok && value == "true"
}

// HasAutoscalingEnabled returns true iff the object has the label that enables autoscaling
func HasAutoscalingEnabled(obj metav1.ObjectMetaAccessor) bool {
	return hasTrueLabel(obj, LabelEnableAutoscaling)
}

// HasAutoMigrationEnabled returns true iff the object has the label that enables "automatic"
// scheduler-triggered migration, and it's set to "true"
func HasAutoMigrationEnabled(obj metav1.ObjectMetaAccessor) bool {
	return hasTrueLabel(obj, LabelEnableAutoMigration)
}

func HasAlwaysMigrateLabel(obj metav1.ObjectMetaAccessor) bool {
	return hasTrueLabel(obj, LabelTestingOnlyAlwaysMigrate)
}

// VmInfo is the subset of vmapi.VirtualMachineSpec that the scheduler plugin and autoscaler agent
// care about. It takes various labels and annotations into account, so certain fields might be
// different from what's strictly in the VirtualMachine object.
type VmInfo struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Cpu       VmCpuInfo `json:"cpu"`
	Mem       VmMemInfo `json:"mem"`
	Config    VmConfig  `json:"config"`
}

type VmCpuInfo struct {
	Min vmapi.MilliCPU `json:"min"`
	Max vmapi.MilliCPU `json:"max"`
	Use vmapi.MilliCPU `json:"use"`
}

func NewVmCpuInfo(cpus vmapi.CPUs) (*VmCpuInfo, error) {
	if cpus.Min == nil {
		return nil, errors.New("expected non-nil field Min")
	}
	if cpus.Max == nil {
		return nil, errors.New("expected non-nil field Max")
	}
	if cpus.Use == nil {
		return nil, errors.New("expected non-nil field Use")
	}
	return &VmCpuInfo{
		Min: *cpus.Min,
		Max: *cpus.Max,
		Use: *cpus.Use,
	}, nil
}

type VmMemInfo struct {
	// Min is the minimum number of memory slots available
	Min uint16 `json:"min"`
	// Max is the maximum number of memory slots available
	Max uint16 `json:"max"`
	// Use is the number of memory slots currently plugged in the VM
	Use uint16 `json:"use"`

	SlotSize Bytes `json:"slotSize"`
}

func NewVmMemInfo(memSlots vmapi.MemorySlots, memSlotSize resource.Quantity) (*VmMemInfo, error) {
	if memSlots.Min == nil {
		return nil, errors.New("expected non-nil field Min")
	}
	if memSlots.Max == nil {
		return nil, errors.New("expected non-nil field Max")
	}
	if memSlots.Use == nil {
		return nil, errors.New("expected non-nil field Use")
	}
	return &VmMemInfo{
		Min:      uint16(*memSlots.Min),
		Max:      uint16(*memSlots.Max),
		Use:      uint16(*memSlots.Use),
		SlotSize: Bytes(memSlotSize.Value()),
	}, nil

}

// VmConfig stores the autoscaling-specific "extra" configuration derived from labels and
// annotations on the VM object.
//
// This is separate from the bounds information stored in VmInfo (even though that's also derived
// from annotations), because VmConfig is meant to store values that either qualitatively change the
// handling for a VM (e.g., AutoMigrationEnabled) or are expected to largely be the same for most VMs
// (e.g., ScalingConfig).
type VmConfig struct {
	// AutoMigrationEnabled indicates to the scheduler plugin that it's allowed to trigger migration
	// for this VM. This defaults to false because otherwise we might disrupt VMs that don't have
	// adequate networking support to preserve connections across live migration.
	AutoMigrationEnabled bool `json:"autoMigrationEnabled"`
	// AlwaysMigrate is a test-only debugging flag that, if present in the VM's labels, will always
	// prompt it to migrate, regardless of whether the VM actually *needs* to.
	AlwaysMigrate  bool           `json:"alwaysMigrate"`
	ScalingEnabled bool           `json:"scalingEnabled"`
	ScalingConfig  *ScalingConfig `json:"scalingConfig,omitempty"`
}

// Using returns the Resources that this VmInfo says the VM is using
func (vm VmInfo) Using() Resources {
	return Resources{
		VCPU: vm.Cpu.Use,
		Mem:  vm.Mem.SlotSize * Bytes(vm.Mem.Use),
	}
}

// SetUsing sets the values of vm.{Cpu,Mem}.Use to those provided by r
func (vm *VmInfo) SetUsing(r Resources) {
	vm.Cpu.Use = r.VCPU
	vm.Mem.Use = uint16(r.Mem / vm.Mem.SlotSize)
}

// Min returns the Resources representing the minimum amount this VmInfo says the VM must reserve
func (vm VmInfo) Min() Resources {
	return Resources{
		VCPU: vm.Cpu.Min,
		Mem:  vm.Mem.SlotSize * Bytes(vm.Mem.Min),
	}
}

// Max returns the Resources representing the maximum amount this VmInfo says the VM may reserve
func (vm VmInfo) Max() Resources {
	return Resources{
		VCPU: vm.Cpu.Max,
		Mem:  vm.Mem.SlotSize * Bytes(vm.Mem.Max),
	}
}

func (vm VmInfo) NamespacedName() util.NamespacedName {
	return util.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}
}

func ExtractVmInfo(logger *zap.Logger, vm *vmapi.VirtualMachine) (*VmInfo, error) {
	logger = logger.With(util.VMNameFields(vm))
	return extractVmInfoGeneric(logger, vm.Name, vm, vm.Spec.Resources())
}

func ExtractVmInfoFromPod(logger *zap.Logger, pod *corev1.Pod) (*VmInfo, error) {
	logger = logger.With(util.PodNameFields(pod))
	resourcesJSON := pod.Annotations[vmapi.VirtualMachineResourcesAnnotation]

	var resources vmapi.VirtualMachineResources
	if err := json.Unmarshal([]byte(resourcesJSON), &resources); err != nil {
		return nil, fmt.Errorf("Error unmarshaling %q: %w",
			vmapi.VirtualMachineResourcesAnnotation, err)
	}

	vmName := pod.Labels[vmapi.VirtualMachineNameLabel]
	return extractVmInfoGeneric(logger, vmName, pod, resources)
}

func extractVmInfoGeneric(
	logger *zap.Logger,
	vmName string,
	obj metav1.ObjectMetaAccessor,
	resources vmapi.VirtualMachineResources,
) (*VmInfo, error) {
	cpuInfo, err := NewVmCpuInfo(resources.CPUs)
	if err != nil {
		return nil, fmt.Errorf("Error extracting CPU info: %w", err)
	}

	memInfo, err := NewVmMemInfo(resources.MemorySlots, resources.MemorySlotSize)
	if err != nil {
		return nil, fmt.Errorf("Error extracting memory info: %w", err)
	}

	autoMigrationEnabled := HasAutoMigrationEnabled(obj)
	scalingEnabled := HasAutoscalingEnabled(obj)
	alwaysMigrate := HasAlwaysMigrateLabel(obj)

	info := VmInfo{
		Name:      vmName,
		Namespace: obj.GetObjectMeta().GetNamespace(),
		Cpu:       *cpuInfo,
		Mem:       *memInfo,
		Config: VmConfig{
			AutoMigrationEnabled: autoMigrationEnabled,
			AlwaysMigrate:        alwaysMigrate,
			ScalingEnabled:       scalingEnabled,
			ScalingConfig:        nil, // set below, maybe
		},
	}

	if boundsJSON, ok := obj.GetObjectMeta().GetAnnotations()[AnnotationAutoscalingBounds]; ok {
		var bounds ScalingBounds
		if err := json.Unmarshal([]byte(boundsJSON), &bounds); err != nil {
			return nil, fmt.Errorf("Error unmarshaling annotation %q: %w", AnnotationAutoscalingBounds, err)
		}

		if err := bounds.Validate(&resources.MemorySlotSize); err != nil {
			return nil, fmt.Errorf("Bad scaling bounds in annotation %q: %w", AnnotationAutoscalingBounds, err)
		}
		info.applyBounds(bounds)
	}

	if configJSON, ok := obj.GetObjectMeta().GetAnnotations()[AnnotationAutoscalingConfig]; ok {
		var config ScalingConfig
		if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
			return nil, fmt.Errorf("Error unmarshaling annotation %q: %w", AnnotationAutoscalingConfig, err)
		}

		if err := config.ValidateOverrides(); err != nil {
			return nil, fmt.Errorf("Bad scaling config in annotation %q: %w", AnnotationAutoscalingConfig, err)
		}
		info.Config.ScalingConfig = &config
	}

	min := info.Min()
	using := info.Using()
	max := info.Max()

	// we can't do validation for resource.Quantity with kubebuilder
	// so do it here
	if err := min.CheckValuesAreReasonablySized(); err != nil {
		return nil, fmt.Errorf("min resources are invalid: %w", err)
	}

	if err := max.CheckValuesAreReasonablySized(); err != nil {
		return nil, fmt.Errorf("max resources are invalid: %w", err)
	}

	// check: min <= max
	if min.HasFieldGreaterThan(max) {
		return nil, fmt.Errorf("min resources %+v has field greater than maximum %+v", min, max)
	}

	// check: min <= using <= max
	if using.HasFieldLessThan(min) {
		logger.Warn(
			"Current usage has field less than minimum",
			zap.Object("using", using), zap.Object("min", min),
		)
	} else if using.HasFieldGreaterThan(max) {
		logger.Warn(
			"Current usage has field greater than maximum",
			zap.Object("using", using), zap.Object("max", max),
		)
	}

	return &info, nil
}

func (vm VmInfo) EqualScalingBounds(cmp VmInfo) bool {
	return vm.Min() == cmp.Min() && vm.Max() == cmp.Max()
}

func (vm *VmInfo) applyBounds(b ScalingBounds) {
	vm.Cpu.Min = vmapi.MilliCPUFromResourceQuantity(b.Min.CPU)
	vm.Cpu.Max = vmapi.MilliCPUFromResourceQuantity(b.Max.CPU)

	// FIXME: this will be incorrect if b.{Min,Max}.Mem.Value() is greater than
	// (2^16-1) * info.Mem.SlotSize.Value().
	vm.Mem.Min = uint16(BytesFromResourceQuantity(b.Min.Mem) / vm.Mem.SlotSize)
	vm.Mem.Max = uint16(BytesFromResourceQuantity(b.Max.Mem) / vm.Mem.SlotSize)
}

// ScalingBounds is the type that we deserialize from the "autoscaling.neon.tech/bounds" annotation
//
// All fields (and sub-fields) are pointers so that our handling can distinguish between "field not
// set" and "field equal to zero". Please note that all field are still required to be set and
// non-zero, though.
type ScalingBounds struct {
	Min ResourceBounds `json:"min"`
	Max ResourceBounds `json:"max"`
}

type ResourceBounds struct {
	CPU resource.Quantity `json:"cpu"`
	Mem resource.Quantity `json:"mem"`
}

// Validate checks that the ScalingBounds are all reasonable values - all fields initialized and
// non-zero.
func (b ScalingBounds) Validate(memSlotSize *resource.Quantity) error {
	ec := &erc.Collector{}

	b.Min.validate(ec, ".min", memSlotSize)
	b.Max.validate(ec, ".max", memSlotSize)

	return ec.Resolve()
}

// TODO: This could be made better - see:
// https://github.com/neondatabase/autoscaling/pull/190#discussion_r1169405645
func (b ResourceBounds) validate(ec *erc.Collector, path string, memSlotSize *resource.Quantity) {
	errAt := func(field string, err error) error {
		return fmt.Errorf("error at %s%s: %w", path, field, err)
	}

	if b.CPU.IsZero() {
		ec.Add(errAt(".cpu", errors.New("must be set to a non-zero value")))
	}

	if b.Mem.IsZero() || b.Mem.Value() < 0 {
		ec.Add(errAt(".mem", errors.New("must be set to a value greater than zero")))
	} else if b.Mem.Value()%memSlotSize.Value() != 0 {
		ec.Add(errAt(".mem", fmt.Errorf("must be divisible by VM memory slot size %s", memSlotSize)))
	}
}

// ScalingConfig provides bits of configuration for how the autoscaler-agent makes scaling decisions
type ScalingConfig struct {
	// LoadAverageFractionTarget sets the desired fraction of current CPU that the load average
	// should be. For example, with a value of 0.7, we'd want load average to sit at 0.7 × CPU,
	// scaling CPU to make this happen.
	//
	// When specifying the autoscaler-agent config, this field is required. For an individual VM, if
	// this field is left out the settings will fall back on the global default.
	LoadAverageFractionTarget *float64 `json:"loadAverageFractionTarget,omitempty"`

	// MemoryUsageFractionTarget sets the desired fraction of current memory that
	// we would like to be using. For example, with a value of 0.7, on a 4GB VM
	// we'd like to be using 2.8GB of memory.
	//
	// When specifying the autoscaler-agent config, this field is required. For an individual VM, if
	// this field is left out the settings will fall back on the global default.
	MemoryUsageFractionTarget *float64 `json:"memoryUsageFractionTarget,omitempty"`
}

// WithOverrides returns a new copy of defaults, where fields set in overrides replace the ones in
// defaults but all others remain the same.
//
// overrides may be nil; if so, this method just returns defaults.
func (defaults ScalingConfig) WithOverrides(overrides *ScalingConfig) ScalingConfig {
	if overrides == nil {
		return defaults
	}

	if overrides.LoadAverageFractionTarget != nil {
		defaults.LoadAverageFractionTarget = lo.ToPtr(*overrides.LoadAverageFractionTarget)
	}
	if overrides.MemoryUsageFractionTarget != nil {
		defaults.MemoryUsageFractionTarget = lo.ToPtr(*overrides.MemoryUsageFractionTarget)
	}

	return defaults
}

// ValidateDefaults checks that the ScalingConfig is safe to use as default settings.
//
// This is more strict than ValidateOverride, where some fields need not be specified.
// Refer to the comments on ScalingConfig for more - each field specifies whether it is required,
// and when.
func (c *ScalingConfig) ValidateDefaults() error {
	return c.validate(true)
}

// ValidateOverrides checks that the ScalingConfig is safe to use to override preexisting settings.
//
// This is less strict than ValidateDefaults, because with ValidateOverrides even required fields
// are optional.
func (c *ScalingConfig) ValidateOverrides() error {
	return c.validate(false)
}

func (c *ScalingConfig) validate(requireAll bool) error {
	ec := &erc.Collector{}

	// Check c.LoadAverageFractionTarget is between 0 and 2. We don't *strictly* need the upper
	// bound, but it's a good safety check.
	if c.LoadAverageFractionTarget != nil {
		erc.Whenf(ec, *c.LoadAverageFractionTarget < 0.0, "%s must be set to value >= 0", ".loadAverageFractionTarget")
		erc.Whenf(ec, *c.LoadAverageFractionTarget >= 2.0, "%s must be set to value < 2 ", ".loadAverageFractionTarget")
	} else if requireAll {
		ec.Add(fmt.Errorf("%s is a required field", ".loadAverageFractionTarget"))
	}

	// Make sure c.MemoryUsageFractionTarget is between 0 and 1
	if c.MemoryUsageFractionTarget != nil {
		erc.Whenf(ec, *c.MemoryUsageFractionTarget < 0.0, "%s must be set to value >= 0", ".memoryUsageFractionTarget")
		erc.Whenf(ec, *c.MemoryUsageFractionTarget >= 1.0, "%s must be set to value < 1 ", ".memoryUsageFractionTarget")
	} else if requireAll {
		ec.Add(fmt.Errorf("%s is a required field", ".memoryUsageFractionTarget"))
	}

	// heads-up! some functions elsewhere depend on the concrete return type of this function.
	return ec.Resolve()
}
