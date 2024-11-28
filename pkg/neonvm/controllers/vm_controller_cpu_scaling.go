package controllers

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
)

// handleCPUScaling encapsulates the logic to handle CPU scaling.
// If vm scaling mode is set to CpuScalingModeSysfs, the scaling is delegated to neonvm-daemon to scale using sys fs state of the CPU cores.
// otherwise the scaling is first done by scaling amount of cores in the VM using QMP and then by updating the cgroups through neonvm-daemon.
// At the moment the cgroup update is not implemented in the daemon, so effectively the scaling is done by QMP or by using sys fs state for the CPU cores.
func (r *VMReconciler) handleCPUScaling(ctx context.Context, vm *vmv1.VirtualMachine, vmRunner *corev1.Pod) (bool, error) {
	log := log.FromContext(ctx)
	useCpuSysfsStateScaling := *vm.Spec.CpuScalingMode == vmv1.CpuScalingModeSysfs

	var scaled bool
	var err error
	if !useCpuSysfsStateScaling {
		scaled, err = r.handleCPUScalingQMP(ctx, vm, vmRunner)
	} else {
		scaled, err = r.handleCPUScalingSysfs(ctx, vm, vmRunner)
	}

	if err != nil {
		log.Error(err, "Failed to scale CPU", "VirtualMachine", vm.Name, "CpuScalingMode", vm.Spec.CpuScalingMode)
		return false, err
	}

	return scaled, nil
}

// handleCPUScalingQMP handles CPU scaling using qemu CPU hotplug/unplug feature.
func (r *VMReconciler) handleCPUScalingQMP(ctx context.Context, vm *vmv1.VirtualMachine, vmRunner *corev1.Pod) (bool, error) {
	log := log.FromContext(ctx)
	specCPU := vm.Spec.Guest.CPUs.Use

	// get cgroups CPU details from runner pod
	cgroupUsage, err := getRunnerCPULimits(ctx, vm)
	if err != nil {
		log.Error(err, "Failed to get CPU details from runner", "VirtualMachine", vm.Name)
		return false, err
	}

	// get CPU details from QEMU
	var pluggedCPU uint32
	cpuSlotsPlugged, _, err := QmpGetCpus(QmpAddr(vm))
	if err != nil {
		log.Error(err, "Failed to get CPU details from VirtualMachine", "VirtualMachine", vm.Name)
		return false, err
	}
	pluggedCPU = uint32(len(cpuSlotsPlugged))

	// start scaling CPU
	log.Info("Scaling using QMP CPU control")
	var hotPlugCPUScaled bool
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
		hotPlugCPUScaled = true
	}

	// update status by CPUs used in the VM
	r.updateVMStatusCPU(ctx, vm, vmRunner, pluggedCPU, cgroupUsage)
	return hotPlugCPUScaled, nil
}

func (r *VMReconciler) handleCPUScalingSysfs(ctx context.Context, vm *vmv1.VirtualMachine, vmRunner *corev1.Pod) (bool, error) {
	log := log.FromContext(ctx)
	specCPU := vm.Spec.Guest.CPUs.Use

	cgroupUsage, err := getRunnerCPULimits(ctx, vm)
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
	if err := setRunnerCPULimits(ctx, vm, specCPU); err != nil {
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
