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

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
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

// VmInfo is the subset of vmv1.VirtualMachineSpec that the scheduler plugin and autoscaler agent
// care about. It takes various labels and annotations into account, so certain fields might be
// different from what's strictly in the VirtualMachine object.
type VmInfo struct {
	Name            string                 `json:"name"`
	Namespace       string                 `json:"namespace"`
	Cpu             VmCpuInfo              `json:"cpu"`
	Mem             VmMemInfo              `json:"mem"`
	Config          VmConfig               `json:"config"`
	CurrentRevision *vmv1.RevisionWithTime `json:"currentRevision,omitempty"`
}

type VmCpuInfo struct {
	Min vmv1.MilliCPU `json:"min"`
	Max vmv1.MilliCPU `json:"max"`
	Use vmv1.MilliCPU `json:"use"`
}

func NewVmCpuInfo(cpus vmv1.CPUs) VmCpuInfo {
	return VmCpuInfo{
		Min: cpus.Min,
		Max: cpus.Max,
		Use: cpus.Use,
	}
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

func NewVmMemInfo(memSlots vmv1.MemorySlots, memSlotSize resource.Quantity) VmMemInfo {
	return VmMemInfo{
		Min:      uint16(memSlots.Min),
		Max:      uint16(memSlots.Max),
		Use:      uint16(memSlots.Use),
		SlotSize: Bytes(memSlotSize.Value()),
	}
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

func ExtractVmInfo(logger *zap.Logger, vm *vmv1.VirtualMachine) (*VmInfo, error) {
	logger = logger.With(util.VMNameFields(vm))
	info, err := extractVmInfoGeneric(logger, vm.Name, vm, vm.Spec.Resources())
	if err != nil {
		return nil, fmt.Errorf("error extracting VM info: %w", err)
	}

	info.CurrentRevision = vm.Status.CurrentRevision
	return info, nil
}

func ExtractVmInfoFromPod(logger *zap.Logger, pod *corev1.Pod) (*VmInfo, error) {
	logger = logger.With(util.PodNameFields(pod))
	resourcesJSON := pod.Annotations[vmv1.VirtualMachineResourcesAnnotation]

	var resources vmv1.VirtualMachineResources
	if err := json.Unmarshal([]byte(resourcesJSON), &resources); err != nil {
		return nil, fmt.Errorf("Error unmarshaling %q: %w",
			vmv1.VirtualMachineResourcesAnnotation, err)
	}

	vmName := pod.Labels[vmv1.VirtualMachineNameLabel]
	return extractVmInfoGeneric(logger, vmName, pod, resources)
}

func extractVmInfoGeneric(
	logger *zap.Logger,
	vmName string,
	obj metav1.ObjectMetaAccessor,
	resources vmv1.VirtualMachineResources,
) (*VmInfo, error) {
	cpuInfo := NewVmCpuInfo(resources.CPUs)
	memInfo := NewVmMemInfo(resources.MemorySlots, resources.MemorySlotSize)

	autoMigrationEnabled := HasAutoMigrationEnabled(obj)
	scalingEnabled := HasAutoscalingEnabled(obj)
	alwaysMigrate := HasAlwaysMigrateLabel(obj)

	info := VmInfo{
		Name:      vmName,
		Namespace: obj.GetObjectMeta().GetNamespace(),
		Cpu:       cpuInfo,
		Mem:       memInfo,
		Config: VmConfig{
			AutoMigrationEnabled: autoMigrationEnabled,
			AlwaysMigrate:        alwaysMigrate,
			ScalingEnabled:       scalingEnabled,
			ScalingConfig:        nil, // set below, maybe
		},
		CurrentRevision: nil, // set later, maybe
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

	minResources := info.Min()
	using := info.Using()
	maxResources := info.Max()

	// we can't do validation for resource.Quantity with kubebuilder
	// so do it here
	if err := minResources.CheckValuesAreReasonablySized(); err != nil {
		return nil, fmt.Errorf("min resources are invalid: %w", err)
	}

	if err := maxResources.CheckValuesAreReasonablySized(); err != nil {
		return nil, fmt.Errorf("max resources are invalid: %w", err)
	}

	// check: min <= max
	if minResources.HasFieldGreaterThan(maxResources) {
		return nil, fmt.Errorf("min resources %+v has field greater than maximum %+v", minResources, maxResources)
	}

	// check: min <= using <= max
	if using.HasFieldLessThan(minResources) {
		logger.Warn(
			"Current usage has field less than minimum",
			zap.Object("using", using), zap.Object("min", minResources),
		)
	} else if using.HasFieldGreaterThan(maxResources) {
		logger.Warn(
			"Current usage has field greater than maximum",
			zap.Object("using", using), zap.Object("max", maxResources),
		)
	}

	return &info, nil
}

func (vm VmInfo) EqualScalingBounds(cmp VmInfo) bool {
	return vm.Min() == cmp.Min() && vm.Max() == cmp.Max()
}

func (vm *VmInfo) applyBounds(b ScalingBounds) {
	vm.Cpu.Min = vmv1.MilliCPUFromResourceQuantity(b.Min.CPU)
	vm.Cpu.Max = vmv1.MilliCPUFromResourceQuantity(b.Max.CPU)

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

	// MemoryUsageFractionTarget sets the maximum fraction of total memory that postgres allocations
	// (MemoryUsage) must fit into. This doesn't count the LFC memory.
	// This memory may also be viewed as "unreclaimable" (contrary to e.g. page cache).
	//
	// For example, with a value of 0.75 on a 4GiB VM, we will try to upscale if the unreclaimable
	// memory usage exceeds 3GiB.
	//
	// When specifying the autoscaler-agent config, this field is required. For an individual VM, if
	// this field is left out the settings will fall back on the global default.
	MemoryUsageFractionTarget *float64 `json:"memoryUsageFractionTarget,omitempty"`

	// MemoryTotalFractionTarget sets the maximum fraction of total memory that postgres allocations
	// PLUS LFC memory (MemoryUsage + MemoryCached) must fit into.
	//
	// Compared with MemoryUsageFractionTarget, this value can be set higher (e.g. 0.9 vs 0.75),
	// because we can tolerate higher fraction of consumption for both in-VM memory consumers.
	MemoryTotalFractionTarget *float64 `json:"memoryTotalFractionTarget,omitempty"`

	// EnableLFCMetrics, if true, enables fetching additional metrics about the Local File Cache
	// (LFC) to provide as input to the scaling algorithm.
	//
	// When specifying the autoscaler-agent config, this field is required. False is a safe default.
	// For an individual VM, if this field is left out the settings will fall back on the global
	// default.
	EnableLFCMetrics *bool `json:"enableLFCMetrics,omitempty"`

	// LFCToMemoryRatio dictates the amount of memory in any given Compute Unit that will be
	// allocated to the LFC. For example, if the LFC is sized at 75% of memory, then this value
	// would be 0.75.
	LFCToMemoryRatio *float64 `json:"lfcToMemoryRatio,omitempty"`

	// LFCMinWaitBeforeDownscaleMinutes dictates the minimum duration we must wait before lowering
	// the goal CU based on LFC working set size.
	// For example, a value of 15 means we will not allow downscaling below the working set size
	// over the past 15 minutes. This allows us to accommodate spiky workloads without flushing the
	// cache every time.
	LFCMinWaitBeforeDownscaleMinutes *int `json:"lfcMinWaitBeforeDownscaleMinutes,omitempty"`

	// LFCWindowSizeMinutes dictates the minimum duration we must use during internal calculations
	// of the rate of increase in LFC working set size.
	LFCWindowSizeMinutes *int `json:"lfcWindowSizeMinutes,omitempty"`

	// CPUStableZoneRatio is the ratio of the stable load zone size relative to load5.
	// For example, a value of 0.25 means that stable zone will be load5±25%.
	CPUStableZoneRatio *float64 `json:"cpuStableZoneRatio,omitempty"`

	// CPUMixedZoneRatio is the ratio of the mixed load zone size relative to load5.
	// Since mixed zone starts after stable zone, values CPUStableZoneRatio=0.25 and CPUMixedZoneRatio=0.15
	// means that stable zone will be from 0.75*load5 to 1.25*load5, and mixed zone will be
	// from 0.6*load5 to 0.75*load5, and from 1.25*load5 to 1.4*load5.
	CPUMixedZoneRatio *float64 `json:"cpuMixedZoneRatio,omitempty"`
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
	if overrides.MemoryTotalFractionTarget != nil {
		defaults.MemoryTotalFractionTarget = lo.ToPtr(*overrides.MemoryTotalFractionTarget)
	}
	if overrides.EnableLFCMetrics != nil {
		defaults.EnableLFCMetrics = lo.ToPtr(*overrides.EnableLFCMetrics)
	}
	if overrides.LFCToMemoryRatio != nil {
		defaults.LFCToMemoryRatio = lo.ToPtr(*overrides.LFCToMemoryRatio)
	}
	if overrides.LFCWindowSizeMinutes != nil {
		defaults.LFCWindowSizeMinutes = lo.ToPtr(*overrides.LFCWindowSizeMinutes)
	}
	if overrides.LFCMinWaitBeforeDownscaleMinutes != nil {
		defaults.LFCMinWaitBeforeDownscaleMinutes = lo.ToPtr(*overrides.LFCMinWaitBeforeDownscaleMinutes)
	}

	if overrides.CPUStableZoneRatio != nil {
		defaults.CPUStableZoneRatio = lo.ToPtr(*overrides.CPUStableZoneRatio)
	}
	if overrides.CPUMixedZoneRatio != nil {
		defaults.CPUMixedZoneRatio = lo.ToPtr(*overrides.CPUMixedZoneRatio)
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
	// Make sure c.MemoryTotalFractionTarget is between 0 and 1
	if c.MemoryTotalFractionTarget != nil {
		erc.Whenf(ec, *c.MemoryTotalFractionTarget < 0.0, "%s must be set to value >= 0", ".memoryTotalFractionTarget")
		erc.Whenf(ec, *c.MemoryTotalFractionTarget >= 1.0, "%s must be set to value < 1 ", ".memoryTotalFractionTarget")
	} else if requireAll {
		ec.Add(fmt.Errorf("%s is a required field", ".memoryTotalFractionTarget"))
	}

	if requireAll {
		erc.Whenf(ec, c.EnableLFCMetrics == nil, "%s is a required field", ".enableLFCMetrics")
		erc.Whenf(ec, c.LFCToMemoryRatio == nil, "%s is a required field", ".lfcToMemoryRatio")
		erc.Whenf(ec, c.LFCWindowSizeMinutes == nil, "%s is a required field", ".lfcWindowSizeMinutes")
		erc.Whenf(ec, c.LFCMinWaitBeforeDownscaleMinutes == nil, "%s is a required field", ".lfcMinWaitBeforeDownscaleMinutes")
		erc.Whenf(ec, c.CPUStableZoneRatio == nil, "%s is a required field", ".cpuStableZoneRatio")
		erc.Whenf(ec, c.CPUMixedZoneRatio == nil, "%s is a required field", ".cpuMixedZoneRatio")
	}

	// heads-up! some functions elsewhere depend on the concrete return type of this function.
	return ec.Resolve()
}
