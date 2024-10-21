package controllers

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
)

// handleCPUScaling is extracted from doReconcile and encapsulates the logic to handle CPU scaling.
// if vm scaling mode is set to CpuSysfsState, the scaling is delegated to neonvm-daemon
// otherwise the scaling is first done by scaling amount of cores in the VM using QMP and then by updating the cgroup
func (r *VMReconciler) handleCPUScaling(ctx context.Context, vm *vmv1.VirtualMachine, vmRunner *corev1.Pod) (bool, error) {

	log := log.FromContext(ctx)
	useCpuSysfsStateScaling := false
	if vm.Spec.CpuScalingMode != nil && *vm.Spec.CpuScalingMode == vmv1.CpuScalingModeSysfs {
		useCpuSysfsStateScaling = true
	}

	var scaled bool
	var err error
	if !useCpuSysfsStateScaling {
		scaled, err = r.handleCPUScalingQMP(ctx, vm, vmRunner)
	} else {
		scaled, err = r.handleCPUScalingSysfs(ctx, vm, vmRunner)
	}

	if err != nil {
		log.Error(err, "Failed to scale CPU", "VirtualMachine", vm.Name, "use_cpu_sysfs_state", useCpuSysfsStateScaling)
		return false, err
	}

	return scaled, nil
}

// handleCPUScalingQMP handles CPU scaling using QMP, extracted as is from doReconcile
func (r *VMReconciler) handleCPUScalingQMP(ctx context.Context, vm *vmv1.VirtualMachine, vmRunner *corev1.Pod) (bool, error) {
	log := log.FromContext(ctx)
	specCPU := vm.Spec.Guest.CPUs.Use
	cgroupUsage, err := getRunnerCgroup(ctx, vm)
	if err != nil {
		log.Error(err, "Failed to get CPU details from runner", "VirtualMachine", vm.Name)
		return false, err
	}
	var hotPlugCPUScaled bool
	var pluggedCPU uint32
	cpuSlotsPlugged, _, err := QmpGetCpus(QmpAddr(vm))
	if err != nil {
		log.Error(err, "Failed to get CPU details from VirtualMachine", "VirtualMachine", vm.Name)
		return false, err
	}
	pluggedCPU = uint32(len(cpuSlotsPlugged))

	log.Info("Using QMP CPU control")
	if specCPU.RoundedUp() > pluggedCPU {
		// going to plug one CPU
		log.Info("Plug one more CPU into VM")
		if err := QmpPlugCpu(QmpAddr(vm)); err != nil {
			return false, err
		}
		r.Recorder.Event(vm, "Normal", "ScaleUp",
			fmt.Sprintf("One more CPU was plugged into VM %s",
				vm.Name))
	} else if specCPU.RoundedUp() < pluggedCPU {
		// going to unplug one CPU
		log.Info("Unplug one CPU from VM")
		if err := QmpUnplugCpu(QmpAddr(vm)); err != nil {
			return false, err
		}
		r.Recorder.Event(vm, "Normal", "ScaleDown",
			fmt.Sprintf("One CPU was unplugged from VM %s",
				vm.Name))
		return false, nil
	} else if specCPU != cgroupUsage.VCPUs {
		_, err := r.handleCgroupCPUUpdate(ctx, vm, cgroupUsage)
		if err != nil {
			log.Error(err, "Failed to update cgroup CPU", "VirtualMachine", vm.Name)
			return false, err
		}
	} else {
		log.Info("No need to plug or unplug CPU")
		hotPlugCPUScaled = true
	}
	r.updateVMStatusCPU(ctx, vm, vmRunner, pluggedCPU, cgroupUsage)
	return hotPlugCPUScaled, nil
}

func (r *VMReconciler) handleCPUScalingSysfs(ctx context.Context, vm *vmv1.VirtualMachine, vmRunner *corev1.Pod) (bool, error) {
	log := log.FromContext(ctx)
	specCPU := vm.Spec.Guest.CPUs.Use

	cgroupUsage, err := getRunnerCgroup(ctx, vm)
	if err != nil {
		log.Error(err, "Failed to get CPU details from runner", "VirtualMachine", vm.Name)
		return false, err
	}
	if specCPU != cgroupUsage.VCPUs {
		return r.handleCgroupCPUUpdate(ctx, vm, cgroupUsage)
	}
	r.updateVMStatusCPU(ctx, vm, vmRunner, cgroupUsage.VCPUs.RoundedUp(), cgroupUsage)
	return true, nil

}

func (r *VMReconciler) handleCgroupCPUUpdate(ctx context.Context, vm *vmv1.VirtualMachine, cgroupUsage *api.VCPUCgroup) (bool, error) {
	specCPU := vm.Spec.Guest.CPUs.Use
	if err := setRunnerCgroup(ctx, vm, specCPU); err != nil {
		return false, err
	}
	reason := "ScaleDown"
	if specCPU > cgroupUsage.VCPUs {
		reason = "ScaleUp"
	}
	r.Recorder.Event(vm, "Normal", reason,
		fmt.Sprintf("Runner pod cgroups was updated on VM %s %s",
			vm.Name, specCPU))
	return true, nil
}
