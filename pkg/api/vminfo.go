// API-relevant types extracted from NeonVM VMs

package api

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	vmapi "github.com/neondatabase/neonvm/apis/neonvm/v1"
)

const LabelTestingOnlyAlwaysMigrate = "autoscaler/testing-only-always-migrate"

// VmInfo is the subset of vmapi.VirtualMachineSpec that the scheduler plugin and autoscaler agent
// care about
type VmInfo struct {
	Name          string
	Namespace     string
	Cpu           VmCpuInfo
	Mem           VmMemInfo
	AlwaysMigrate bool
}

type VmCpuInfo struct {
	Min uint16
	Max uint16
	Use uint16
}

type VmMemInfo struct {
	Min uint16
	Max uint16
	Use uint16

	SlotSize resource.Quantity
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
		AlwaysMigrate: alwaysMigrate,
	}

	if err != nil {
		return nil, err
	}

	return &info, nil
}
