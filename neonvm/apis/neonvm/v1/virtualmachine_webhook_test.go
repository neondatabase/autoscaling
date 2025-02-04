package v1

import (
	"testing"

	"github.com/samber/lo"
	"github.com/tychoish/fun/assert"
)

func TestFieldsAllowedToChangeFromNilOnly(t *testing.T) {
	t.Run("should allow change from nil values", func(t *testing.T) {
		defaultVm := &VirtualMachine{}
		// defaultVm.Default() returns an object with nil value for fields we are interested in
		defaultVm.Default()

		fromNilFields := []struct {
			setter func(*VirtualMachine)
			field  string
		}{
			{
				setter: func(vm *VirtualMachine) {
					vm.Spec.CpuScalingMode = lo.ToPtr(CpuScalingModeQMP)
				},
				field: ".spec.cpuScalingMode",
			},
			{
				setter: func(vm *VirtualMachine) {
					vm.Spec.TargetArchitecture = lo.ToPtr(CPUArchitectureAMD64)
				},
				field: ".spec.targetArchitecture",
			},
		}

		for _, field := range fromNilFields {
			vm2 := defaultVm.DeepCopy()
			field.setter(vm2)
			_, err := vm2.ValidateUpdate(defaultVm)
			assert.NotError(t, err)
		}
	})

	t.Run("should not allow change from non-nil values", func(t *testing.T) {
		defaultVm := &VirtualMachine{}
		defaultVm.Default()
		// override nil values with non-nil values
		defaultVm.Spec.CpuScalingMode = lo.ToPtr(CpuScalingModeQMP)
		defaultVm.Spec.TargetArchitecture = lo.ToPtr(CPUArchitectureAMD64)

		fromNilFields := []struct {
			setter func(*VirtualMachine)
			field  string
		}{
			{
				setter: func(vm *VirtualMachine) {
					vm.Spec.CpuScalingMode = lo.ToPtr(CpuScalingModeSysfs)
				},
				field: ".spec.cpuScalingMode",
			},
			{
				setter: func(vm *VirtualMachine) {
					vm.Spec.TargetArchitecture = lo.ToPtr(CPUArchitectureARM64)
				},
				field: ".spec.targetArchitecture",
			},
		}

		for _, field := range fromNilFields {
			vm2 := defaultVm.DeepCopy()
			field.setter(vm2)
			_, err := vm2.ValidateUpdate(defaultVm)
			assert.Error(t, err)
		}
	})

}
