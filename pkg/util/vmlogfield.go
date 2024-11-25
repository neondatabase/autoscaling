package util

// Helper for creating a zap.Field for a VM

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	corev1 "k8s.io/api/core/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type nameFields struct {
	virtualmachine NamespacedName
	pod            NamespacedName
}

// MarshalLogObject implements zapcore.ObjectMarshaler
func (f nameFields) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if err := enc.AddObject("virtualmachine", f.virtualmachine); err != nil {
		return err
	}
	if err := enc.AddObject("pod", f.pod); err != nil {
		return err
	}
	return nil
}

func VMNameFields(vm *vmv1.VirtualMachine) zap.Field {
	vmName := GetNamespacedName(vm)

	// If the VM has a pod, log both the VM and the pod, otherwise just the VM.
	if vm.Status.PodName == "" {
		return zap.Object("virtualmachine", vmName)
	} else {
		podName := NamespacedName{Namespace: vm.Namespace, Name: vm.Status.PodName}

		return zap.Inline(nameFields{
			virtualmachine: vmName,
			pod:            podName,
		})
	}
}

func PodNameFields(pod *corev1.Pod) zap.Field {
	podName := GetNamespacedName(pod)

	if vmName, ok := pod.Labels[vmv1.VirtualMachineNameLabel]; ok {
		vmName := NamespacedName{Namespace: pod.Namespace, Name: vmName}

		return zap.Inline(nameFields{
			virtualmachine: vmName,
			pod:            podName,
		})
	} else {
		return zap.Object("pod", podName)
	}
}
