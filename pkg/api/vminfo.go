// API-relevant types extracted from NeonVM VMs

package api

import (
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

const (
	LabelTestingOnlyAlwaysMigrate = "autoscaler/testing-only-always-migrate"
	LabelEnableAutoscaling        = "autoscaling.neon.tech/enabled"
	AnnotationAutoscalingBounds   = "autoscaling.neon.tech/bounds"
)

// HasAutoscalingEnabled returns true iff the object has the label that enables autoscaling
func HasAutoscalingEnabled(obj metav1.ObjectMetaAccessor) bool {
	labels := obj.GetObjectMeta().GetLabels()
	value, ok := labels[LabelEnableAutoscaling]
	return ok && value == "true"
}

// VmInfo is the subset of vmapi.VirtualMachineSpec that the scheduler plugin and autoscaler agent
// care about. It takes various labels and annotations into account, so certain fields might be
// different from what's strictly in the VirtualMachine object.
type VmInfo struct {
	Name           string    `json:"name"`
	Namespace      string    `json:"namespace"`
	Cpu            VmCpuInfo `json:"cpu"`
	Mem            VmMemInfo `json:"mem"`
	AlwaysMigrate  bool      `json:"alwaysMigrate"`
	ScalingEnabled bool      `json:"scalingEnabled"`
}

type VmCpuInfo struct {
	Min resource.Quantity `json:"min"`
	Max resource.Quantity `json:"max"`
	Use resource.Quantity `json:"use"`
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

	getNonNilResourceQuantity := func(err *error, ptr *resource.Quantity, name string) (val resource.Quantity) {
		if *err != nil {
			return
		} else if ptr == nil {
			*err = fmt.Errorf("expected non-nil field %s", name)
			return
		} else {
			return *ptr
		}
	}

	_, alwaysMigrate := vm.Labels[LabelTestingOnlyAlwaysMigrate]
	scalingEnabled := HasAutoscalingEnabled(vm)

	slotSize := vm.Spec.Guest.MemorySlotSize // explicitly copy slot size so we aren't keeping the VM object around
	info := VmInfo{
		Name:      vm.Name,
		Namespace: vm.Namespace,
		Cpu: VmCpuInfo{
			Min: getNonNilResourceQuantity(&err, vm.Spec.Guest.CPUs.Min, ".spec.guest.cpus.min"),
			Max: getNonNilResourceQuantity(&err, vm.Spec.Guest.CPUs.Max, ".spec.guest.cpus.max"),
			Use: getNonNilResourceQuantity(&err, vm.Spec.Guest.CPUs.Use, ".spec.guest.cpus.use"),
		},
		Mem: VmMemInfo{
			Min:      uint16(getNonNilInt(&err, vm.Spec.Guest.MemorySlots.Min, ".spec.guest.memorySlots.min")),
			Max:      uint16(getNonNilInt(&err, vm.Spec.Guest.MemorySlots.Max, ".spec.guest.memorySlots.max")),
			Use:      uint16(getNonNilInt(&err, vm.Spec.Guest.MemorySlots.Use, ".spec.guest.memorySlots.use")),
			SlotSize: &slotSize,
		},
		AlwaysMigrate:  alwaysMigrate,
		ScalingEnabled: scalingEnabled,
	}

	if err != nil {
		return nil, err
	}

	if boundsJSON, ok := vm.Annotations[AnnotationAutoscalingBounds]; ok {
		var bounds scalingBounds
		if err := json.Unmarshal([]byte(boundsJSON), &bounds); err != nil {
			return nil, fmt.Errorf("Error unmarshaling annotation %q: %w", AnnotationAutoscalingBounds, err)
		}

		if err := bounds.validate(info.Mem.SlotSize); err != nil {
			return nil, fmt.Errorf("Bad scaling bounds in annotation %q: %w", AnnotationAutoscalingBounds, err)
		}
		info.applyBounds(bounds)
	}

	min := info.Min()
	using := info.Using()
	max := info.Max()

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

func (vm *VmInfo) applyBounds(b scalingBounds) {
	vm.Cpu.Min = *b.Min.CPU
	vm.Cpu.Max = *b.Max.CPU

	// FIXME: this will be incorrect if b.{Min,Max}.Mem.Value() is greater than
	// (2^16-1) * info.Mem.SlotSize.Value().
	vm.Mem.Min = uint16(b.Min.Mem.Value() / vm.Mem.SlotSize.Value())
	vm.Mem.Max = uint16(b.Max.Mem.Value() / vm.Mem.SlotSize.Value())
}

type scalingBounds struct {
	Min *resourceBound `json:"min,omitempty"`
	Max *resourceBound `json:"max,omitempty"`
}

type resourceBound struct {
	CPU *resource.Quantity `json:"cpu,omitempty"`
	Mem *resource.Quantity `json:"mem,omitempty"`
}

func (b scalingBounds) validate(memSlotSize *resource.Quantity) error {
	if b.Min == nil {
		return errors.New("missing field 'min'")
	} else if b.Max == nil {
		return errors.New("missing field 'max'")
	}

	if field, err := b.Min.validate(memSlotSize); err != nil {
		return fmt.Errorf("error at .min%s: %w", field, err)
	} else if field, err := b.Max.validate(memSlotSize); err != nil {
		return fmt.Errorf("error at .max%s: %w", field, err)
	}

	return nil
}

func (b resourceBound) validate(memSlotSize *resource.Quantity) (field string, _ error) {
	if b.CPU == nil {
		return "", errors.New("missing field 'cpu'")
	} else if b.Mem == nil {
		return "", errors.New("missing field 'mem'")
	}

	if b.CPU.IsZero() {
		return ".cpu", errors.New("value cannot be zero")
	}

	if b.Mem.IsZero() || b.Mem.Value() < 0 {
		return ".mem", errors.New("value must be greater than zero")
	} else if b.Mem.Value()%memSlotSize.Value() != 0 {
		return ".mem", fmt.Errorf("value must be divisible by VM memory slot size %s", memSlotSize)
	}

	return "", nil
}

// the reason we have custom formatting for VmInfo is because without it, the formatting of memory
// slot size (which is a resource.Quantity) has all the fields, meaning (a) it's hard to read, and
// (b) there's a lot of unnecessary information. But in order to enable "proper" formatting given a
// VmInfo, we have to implement Format from the top down, so we do.
func (vm VmInfo) Format(state fmt.State, verb rune) {
	switch {
	case verb == 'v' && state.Flag('#'):
		state.Write([]byte(fmt.Sprintf(
			"api.VmInfo{Name:%q, Namespace:%q, Cpu:%#v, Mem:%#v, AlwaysMigrate:%t, ScalingEnabled:%t}",
			vm.Name, vm.Namespace, vm.Cpu, vm.Mem, vm.AlwaysMigrate, vm.ScalingEnabled,
		)))
	default:
		if verb != 'v' {
			state.Write([]byte("%!"))
			state.Write([]byte(string(verb)))
			state.Write([]byte("(api.VmInfo="))
		}

		state.Write([]byte(fmt.Sprintf(
			"{Name:%s Namespace:%s Cpu:%v Mem:%v AlwaysMigrate:%t ScalingEnabled:%t}",
			vm.Name, vm.Namespace, vm.Cpu, vm.Mem, vm.AlwaysMigrate, vm.ScalingEnabled,
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
