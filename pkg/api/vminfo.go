// API-relevant types extracted from NeonVM VMs

package api

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tychoish/fun/erc"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/util"
)

const (
	LabelTestingOnlyAlwaysMigrate = "autoscaling.neon.tech/testing-only-always-migrate"
	LabelEnableAutoscaling        = "autoscaling.neon.tech/enabled"
	AnnotationAutoscalingBounds   = "autoscaling.neon.tech/bounds"
	AnnotationAutoscalingConfig   = "autoscaling.neon.tech/config"
)

// HasAutoscalingEnabled returns true iff the object has the label that enables autoscaling
func HasAutoscalingEnabled(obj metav1.ObjectMetaAccessor) bool {
	labels := obj.GetObjectMeta().GetLabels()
	value, ok := labels[LabelEnableAutoscaling]
	return ok && value == "true"
}

func HasAlwaysMigrateLabel(obj metav1.ObjectMetaAccessor) bool {
	labels := obj.GetObjectMeta().GetLabels()
	value, ok := labels[LabelTestingOnlyAlwaysMigrate]
	return ok && value == "true"
}

// VmInfo is the subset of vmapi.VirtualMachineSpec that the scheduler plugin and autoscaler agent
// care about. It takes various labels and annotations into account, so certain fields might be
// different from what's strictly in the VirtualMachine object.
type VmInfo struct {
	Name           string         `json:"name"`
	Namespace      string         `json:"namespace"`
	Cpu            VmCpuInfo      `json:"cpu"`
	Mem            VmMemInfo      `json:"mem"`
	ScalingConfig  *ScalingConfig `json:"scalingConfig,omitempty"`
	AlwaysMigrate  bool           `json:"alwaysMigrate"`
	ScalingEnabled bool           `json:"scalingEnabled"`
}

type VmCpuInfo struct {
	Min MilliCPU `json:"min"`
	Max MilliCPU `json:"max"`
	Use MilliCPU `json:"use"`
}

type VmMemInfo struct {
	Min uint16 `json:"min"`
	Max uint16 `json:"max"`
	Use uint16 `json:"use"`

	SlotSize *resource.Quantity `json:"slotSize"`
}

// Using returns the Resources that this VmInfo says the VM is using
func (vm VmInfo) Using() Resources {
	return Resources{
		VCPU: vm.Cpu.Use,
		Mem:  vm.Mem.Use,
	}
}

// SetUsing sets the values of vm.{Cpu,Mem}.Use to those provided by r
func (vm *VmInfo) SetUsing(r Resources) {
	vm.Cpu.Use = r.VCPU
	vm.Mem.Use = r.Mem
}

// Min returns the Resources representing the minimum amount this VmInfo says the VM must reserve
func (vm VmInfo) Min() Resources {
	return Resources{
		VCPU: vm.Cpu.Min,
		Mem:  vm.Mem.Min,
	}
}

// Max returns the Resources representing the maximum amount this VmInfo says the VM may reserve
func (vm VmInfo) Max() Resources {
	return Resources{
		VCPU: vm.Cpu.Max,
		Mem:  vm.Mem.Max,
	}
}

func (vm VmInfo) NamespacedName() util.NamespacedName {
	return util.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}
}

func ExtractVmInfo(vm *vmapi.VirtualMachine) (*VmInfo, error) {
	var err error

	getNonNilInt := func(err *error, ptr *int32, name string) (val int32) {
		if *err != nil {
			return
		} else if ptr == nil {
			*err = fmt.Errorf("expected non-nil field %s", name)
			return
		} else {
			return *ptr
		}
	}

	getNonNilMilliCPU := func(err *error, ptr *resource.Quantity, name string) (val MilliCPU) {
		if *err != nil {
			return
		} else if ptr == nil {
			*err = fmt.Errorf("expected non-nil field %s", name)
			return
		} else {
			return MilliCPUFromResourceQuantity(*ptr)
		}
	}

	scalingEnabled := HasAutoscalingEnabled(vm)
	alwaysMigrate := HasAlwaysMigrateLabel(vm)

	slotSize := vm.Spec.Guest.MemorySlotSize // explicitly copy slot size so we aren't keeping the VM object around
	info := VmInfo{
		Name:      vm.Name,
		Namespace: vm.Namespace,
		Cpu: VmCpuInfo{
			Min: getNonNilMilliCPU(&err, vm.Spec.Guest.CPUs.Min, ".spec.guest.cpus.min"),
			Max: getNonNilMilliCPU(&err, vm.Spec.Guest.CPUs.Max, ".spec.guest.cpus.max"),
			Use: getNonNilMilliCPU(&err, vm.Spec.Guest.CPUs.Use, ".spec.guest.cpus.use"),
		},
		Mem: VmMemInfo{
			Min:      uint16(getNonNilInt(&err, vm.Spec.Guest.MemorySlots.Min, ".spec.guest.memorySlots.min")),
			Max:      uint16(getNonNilInt(&err, vm.Spec.Guest.MemorySlots.Max, ".spec.guest.memorySlots.max")),
			Use:      uint16(getNonNilInt(&err, vm.Spec.Guest.MemorySlots.Use, ".spec.guest.memorySlots.use")),
			SlotSize: &slotSize,
		},
		ScalingConfig:  nil, // set below, maybe
		AlwaysMigrate:  alwaysMigrate,
		ScalingEnabled: scalingEnabled,
	}

	if err != nil {
		return nil, err
	}

	if boundsJSON, ok := vm.Annotations[AnnotationAutoscalingBounds]; ok {
		var bounds ScalingBounds
		if err := json.Unmarshal([]byte(boundsJSON), &bounds); err != nil {
			return nil, fmt.Errorf("Error unmarshaling annotation %q: %w", AnnotationAutoscalingBounds, err)
		}

		if err := bounds.Validate(info.Mem.SlotSize); err != nil {
			return nil, fmt.Errorf("Bad scaling bounds in annotation %q: %w", AnnotationAutoscalingBounds, err)
		}
		info.applyBounds(bounds)
	}

	if configJSON, ok := vm.Annotations[AnnotationAutoscalingConfig]; ok {
		var config ScalingConfig
		if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
			return nil, fmt.Errorf("Error unmarshaling annotation %q: %w", AnnotationAutoscalingConfig, err)
		}

		if err := config.Validate(); err != nil {
			return nil, fmt.Errorf("Bad scaling config in annotation %q: %w", AnnotationAutoscalingConfig, err)
		}
		info.ScalingConfig = &config
	}

	min := info.Min()
	using := info.Using()
	max := info.Max()

	// we can't do validation for resource.Quantity with kubebuilder
	// so do it here
	if err := min.CheckValuesAreReasonablySized(); err != nil {
		return nil, err
	}

	if err := max.CheckValuesAreReasonablySized(); err != nil {
		return nil, err
	}

	// check: min <= max
	if min.HasFieldGreaterThan(max) {
		return nil, fmt.Errorf("min resources %+v has field greater than maximum %+v", min, max)
	}

	// check: min <= using <= max
	if using.HasFieldLessThan(min) {
		return nil, fmt.Errorf("current usage %+v has field less than minimum %+v", using, min)
	} else if using.HasFieldGreaterThan(max) {
		return nil, fmt.Errorf("current usage %+v has field greater than maximum %+v", using, max)
	}

	return &info, nil
}

func (vm VmInfo) EqualScalingBounds(cmp VmInfo) bool {
	return vm.Min() != cmp.Min() || vm.Max() != cmp.Max()
}

func (vm *VmInfo) applyBounds(b ScalingBounds) {
	vm.Cpu.Min = MilliCPUFromResourceQuantity(b.Min.CPU)
	vm.Cpu.Max = MilliCPUFromResourceQuantity(b.Max.CPU)

	// FIXME: this will be incorrect if b.{Min,Max}.Mem.Value() is greater than
	// (2^16-1) * info.Mem.SlotSize.Value().
	vm.Mem.Min = uint16(b.Min.Mem.Value() / vm.Mem.SlotSize.Value())
	vm.Mem.Max = uint16(b.Max.Mem.Value() / vm.Mem.SlotSize.Value())
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
	// should be. For example, with a value of 0.7, we'd want load average to sit at 0.7 ×
	// CPU,
	// scaling CPU to make this happen.
	LoadAverageFractionTarget float64 `json:"loadAverageFractionTarget"`
}

func (c *ScalingConfig) Validate() error {
	ec := &erc.Collector{}

	// Check c.loadAverageFractionTarget is between 0 and 2. We don't
	// *strictly* need the upper
	// bound, but it's a good safety check.
	erc.Whenf(ec, c.LoadAverageFractionTarget < 0.0, "%s must be set to value >= 0", ".loadAverageFractionTarget")
	erc.Whenf(ec, c.LoadAverageFractionTarget >= 2.0, "%s must be set to value < 2 ", ".loadAverageFractionTarget")

	// heads-up! some functions elsewhere depend on the concrete return type of this function.
	return ec.Resolve()
}

// the reason we have custom formatting for VmInfo is because without it, the formatting of memory
// slot size (which is a resource.Quantity) has all the fields, meaning (a) it's hard to read, and
// (b) there's a lot of unnecessary information. But in order to enable "proper" formatting given a
// VmInfo, we have to implement Format from the top down, so we do.
func (vm VmInfo) Format(state fmt.State, verb rune) {
	switch {
	case verb == 'v' && state.Flag('#'):
		state.Write([]byte(fmt.Sprintf(
			"api.VmInfo{Name:%q, Namespace:%q, Cpu:%#v, Mem:%#v, ScalingConfig:%#v, AlwaysMigrate:%t, ScalingEnabled:%t}",
			vm.Name, vm.Namespace, vm.Cpu, vm.Mem, vm.ScalingConfig, vm.AlwaysMigrate, vm.ScalingEnabled,
		)))
	default:
		if verb != 'v' {
			state.Write([]byte("%!"))
			state.Write([]byte(string(verb)))
			state.Write([]byte("(api.VmInfo="))
		}

		state.Write([]byte(fmt.Sprintf(
			"{Name:%s Namespace:%s Cpu:%v Mem:%v ScalingConfig:%+v AlwaysMigrate:%t ScalingEnabled:%t}",
			vm.Name, vm.Namespace, vm.Cpu, vm.Mem, vm.ScalingConfig, vm.AlwaysMigrate, vm.ScalingEnabled,
		)))

		if verb != 'v' {
			state.Write([]byte{')'})
		}
	}
}

func (cpu VmCpuInfo) Format(state fmt.State, verb rune) {
	// same-ish style as for VmInfo, differing slightly from default repr.
	switch {
	case verb == 'v' && state.Flag('#'):
		state.Write([]byte(fmt.Sprintf("api.VmCpuInfo{Min:%d, Max:%d, Use:%d}", cpu.Min, cpu.Max, cpu.Use)))
	default:
		state.Write([]byte(fmt.Sprintf("{Min:%d Max:%d Use:%d}", cpu.Min, cpu.Max, cpu.Use)))
	}
}

func (mem VmMemInfo) Format(state fmt.State, verb rune) {
	// same-ish style as for VmInfo, differing slightly from default repr.
	switch {
	case verb == 'v' && state.Flag('#'):
		state.Write([]byte(fmt.Sprintf("api.VmMemInfo{Min:%d, Max:%d, Use:%d, SlotSize:%#v}", mem.Min, mem.Max, mem.Use, mem.SlotSize)))
	default:
		state.Write([]byte(fmt.Sprintf("{Min:%d Max:%d Use:%d SlotSize:%v}", mem.Min, mem.Max, mem.Use, mem.SlotSize)))
	}
}
