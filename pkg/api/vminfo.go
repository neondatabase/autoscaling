// API-relevant types extracted from NeonVM VMs

package api

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

const (
	LabelTestingOnlyAlwaysMigrate = "autoscaler/testing-only-always-migrate"
	LabelEnableAutoscaling        = "autoscaling.neon.tech/enabled"
)

// HasAutoscalingEnabled returns true iff the object has the label that enables autoscaling
func HasAutoscalingEnabled[T metav1.ObjectMetaAccessor](obj T) bool {
	labels := obj.GetObjectMeta().GetLabels()
	value, ok := labels[LabelEnableAutoscaling]
	return ok && value == "true"
}

// VmInfo is the subset of vmapi.VirtualMachineSpec that the scheduler plugin and autoscaler agent
// care about
type VmInfo struct {
	Name           string    `json:"name"`
	Namespace      string    `json:"namespace"`
	Cpu            VmCpuInfo `json:"cpu"`
	Mem            VmMemInfo `json:"mem"`
	AlwaysMigrate  bool      `json:"alwaysMigrate"`
	ScalingEnabled bool      `json:"scalingEnabled"`
}

type VmCpuInfo struct {
	Min uint16 `json:"min"`
	Max uint16 `json:"max"`
	Use uint16 `json:"use"`
}

type VmMemInfo struct {
	Min uint16 `json:"min"`
	Max uint16 `json:"max"`
	Use uint16 `json:"use"`

	SlotSize resource.Quantity `json:"slotSize"`
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

	_, alwaysMigrate := vm.Labels[LabelTestingOnlyAlwaysMigrate]
	scalingEnabled := HasAutoscalingEnabled(vm)

	info := VmInfo{
		Name:      vm.Name,
		Namespace: vm.Namespace,
		Cpu: VmCpuInfo{
			Min: uint16(getNonNilInt(&err, vm.Spec.Guest.CPUs.Min, ".spec.guest.cpus.min")),
			Max: uint16(getNonNilInt(&err, vm.Spec.Guest.CPUs.Max, ".spec.guest.cpus.max")),
			Use: uint16(getNonNilInt(&err, vm.Spec.Guest.CPUs.Use, ".spec.guest.cpus.use")),
		},
		Mem: VmMemInfo{
			Min:      uint16(getNonNilInt(&err, vm.Spec.Guest.MemorySlots.Min, ".spec.guest.memorySlots.min")),
			Max:      uint16(getNonNilInt(&err, vm.Spec.Guest.MemorySlots.Max, ".spec.guest.memorySlots.max")),
			Use:      uint16(getNonNilInt(&err, vm.Spec.Guest.MemorySlots.Use, ".spec.guest.memorySlots.use")),
			SlotSize: vm.Spec.Guest.MemorySlotSize,
		},
		AlwaysMigrate:  alwaysMigrate,
		ScalingEnabled: scalingEnabled,
	}

	if err != nil {
		return nil, err
	}

	min := info.Min()
	using := info.Using()
	max := info.Max()

	if using.HasFieldLessThan(min) {
		return nil, fmt.Errorf("current usage %+v has field less than minimum %+v", using, min)
	} else if using.HasFieldGreaterThan(max) {
		return nil, fmt.Errorf("current usage %+v has field greater than maximum %+v", using, max)
	}

	return &info, nil
}
