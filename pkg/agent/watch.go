package agent

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

type vmEvent struct {
	kind    vmEventKind
	vmInfo  api.VmInfo
	podName string
	podIP   string
	// if present, the ID of the endpoint associated with the VM. May be empty.
	endpointID string
}

const endpointLabel = "neon/endpoint-id"

// MarshalLogObject implements zapcore.ObjectMarshaler
func (ev vmEvent) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("kind", string(ev.kind))
	enc.AddString("podName", ev.podName)
	enc.AddString("podIP", ev.podIP)
	enc.AddString("endpointID", ev.endpointID)
	if err := enc.AddReflected("vmInfo", ev.vmInfo); err != nil {
		return err
	}
	return nil
}

type vmEventKind string

const (
	vmEventAdded   vmEventKind = "added"
	vmEventUpdated vmEventKind = "updated"
	vmEventDeleted vmEventKind = "deleted"
)

// note: unlike startPodWatcher, we aren't able to use a field selector on VM status.node (currently; NeonVM v0.4.6)
func startVMWatcher(
	ctx context.Context,
	parentLogger *zap.Logger,
	config *Config,
	vmClient *vmclient.Clientset,
	metrics watch.Metrics,
	perVMMetrics PerVMMetrics,
	nodeName string,
	submitEvent func(vmEvent),
) (*watch.Store[vmapi.VirtualMachine], error) {
	logger := parentLogger.Named("vm-watch")

	return watch.Watch(
		ctx,
		logger.Named("watch"),
		vmClient.NeonvmV1().VirtualMachines(corev1.NamespaceAll),
		watch.Config{
			ObjectNameLogField: "virtualmachine",
			Metrics: watch.MetricsConfig{
				Metrics:  metrics,
				Instance: "VirtualMachines",
			},
			// We want to be relatively snappy; don't wait for too long before retrying.
			RetryRelistAfter: util.NewTimeRange(time.Millisecond, 500, 1000),
			RetryWatchAfter:  util.NewTimeRange(time.Millisecond, 500, 1000),
		},
		watch.Accessors[*vmapi.VirtualMachineList, vmapi.VirtualMachine]{
			Items: func(list *vmapi.VirtualMachineList) []vmapi.VirtualMachine { return list.Items },
		},
		watch.InitModeDefer,
		metav1.ListOptions{},
		watch.HandlerFuncs[*vmapi.VirtualMachine]{
			AddFunc: func(vm *vmapi.VirtualMachine, preexisting bool) {
				setMetricsFromVM(vm, nodeName, &perVMMetrics)

				if vmIsOurResponsibility(vm, config, nodeName) {
					event, err := makeVMEvent(logger, vm, vmEventAdded)
					if err != nil {
						logger.Error(
							"Failed to create vmEvent for added VM",
							util.VMNameFields(vm), zap.Error(err),
						)
						return
					}
					submitEvent(event)
				}
			},
			UpdateFunc: func(oldVM, newVM *vmapi.VirtualMachine) {
				deleteMetricsfromVM(oldVM, nodeName, &perVMMetrics)
				setMetricsFromVM(newVM, nodeName, &perVMMetrics)

				oldIsOurs := vmIsOurResponsibility(oldVM, config, nodeName)
				newIsOurs := vmIsOurResponsibility(newVM, config, nodeName)

				if !oldIsOurs && !newIsOurs {
					return
				}

				var vmForEvent *vmapi.VirtualMachine
				var eventKind vmEventKind

				if !oldIsOurs && newIsOurs {
					vmForEvent = newVM
					eventKind = vmEventAdded
				} else if oldIsOurs && !newIsOurs {
					vmForEvent = oldVM
					eventKind = vmEventDeleted
				} else {
					vmForEvent = newVM
					eventKind = vmEventUpdated
				}

				event, err := makeVMEvent(logger, vmForEvent, eventKind)
				if err != nil {
					logger.Error(
						"Failed to create vmEvent for updated VM",
						util.VMNameFields(vmForEvent), zap.Error(err),
					)
					return
				}

				submitEvent(event)
			},
			DeleteFunc: func(vm *vmapi.VirtualMachine, maybeStale bool) {
				deleteMetricsfromVM(vm, nodeName, &perVMMetrics)

				if vmIsOurResponsibility(vm, config, nodeName) {
					event, err := makeVMEvent(logger, vm, vmEventDeleted)
					if err != nil {
						logger.Error(
							"Failed to create vmEvent for deleted VM",
							util.VMNameFields(vm), zap.Error(err),
						)
						return
					}
					submitEvent(event)
				}
			},
		},
	)
}

func makeVMEvent(logger *zap.Logger, vm *vmapi.VirtualMachine, kind vmEventKind) (vmEvent, error) {
	info, err := api.ExtractVmInfo(logger, vm)
	if err != nil {
		return vmEvent{}, fmt.Errorf("Error extracting VM info: %w", err)
	}

	endpointID := ""
	if vm.Labels != nil {
		endpointID = vm.Labels[endpointLabel]
	}

	return vmEvent{
		kind:       kind,
		vmInfo:     *info,
		podName:    vm.Status.PodName,
		podIP:      vm.Status.PodIP,
		endpointID: endpointID,
	}, nil
}

// custom formatting for vmEvent so that it prints in the same way as VmInfo
func (e vmEvent) Format(state fmt.State, verb rune) {
	switch {
	case verb == 'v' && state.Flag('#'):
		state.Write([]byte(fmt.Sprintf(
			// note: intentionally order podName and podIP before vmInfo because vmInfo is large.
			"agent.vmEvent{kind:%q, podName:%q, podIP:%q, vmInfo:%#v, endpointID:%q}",
			e.kind, e.podName, e.podIP, e.vmInfo, e.endpointID,
		)))
	default:
		if verb != 'v' {
			state.Write([]byte("%!"))
			state.Write([]byte(string(verb)))
			state.Write([]byte("(agent.vmEvent="))
		}

		state.Write([]byte(fmt.Sprintf(
			"{kind:%s podName:%s podIP:%s vmInfo:%v endpointID:%s}",
			e.kind, e.podName, e.podIP, e.vmInfo, e.endpointID,
		)))

		if verb != 'v' {
			state.Write([]byte{')'})
		}
	}
}

type kv[K any, V any] struct {
	key   K
	value V
}

func setMetricsFromVM(vm *vmapi.VirtualMachine, nodeName string, perVMMetrics *PerVMMetrics) {
	if vm.Status.Node != nodeName {
		return
	}

	commonLabelValues := []string{
		vm.Namespace,
		vm.Name,
		vm.Labels[endpointLabel],
	}

	// CPU metrics derived from spec
	specCPU := []kv[vmResourceValueType, *vmapi.MilliCPU]{
		{key: vmResourceValueMin, value: vm.Spec.Guest.CPUs.Min},
		{key: vmResourceValueMax, value: vm.Spec.Guest.CPUs.Max},
		{key: vmResourceValueSpecUse, value: vm.Spec.Guest.CPUs.Use},
	}
	for _, t := range specCPU {
		if t.value != nil {
			perVMMetrics.cpu.WithLabelValues(append(commonLabelValues, string(t.key))...).Set(t.value.AsFloat64())
		}
	}

	if vm.Status.CPUs != nil {
		perVMMetrics.cpu.WithLabelValues(append(commonLabelValues, string(vmResourceValueStatusUse))...).Set(vm.Status.CPUs.AsFloat64())
	}
	// Memory metrics derived from spec
	specMem := []kv[vmResourceValueType, *int32]{
		{key: vmResourceValueMin, value: vm.Spec.Guest.MemorySlots.Min},
		{key: vmResourceValueMax, value: vm.Spec.Guest.MemorySlots.Max},
		{key: vmResourceValueSpecUse, value: vm.Spec.Guest.MemorySlots.Use},
	}
	for _, t := range specMem {
		if t.value != nil {
			memValue := float64(vm.Spec.Guest.MemorySlotSize.Value() * int64(*t.value))
			perVMMetrics.memory.WithLabelValues(append(commonLabelValues, string(t.key))...).Set(memValue)
		}
	}
}

func deleteMetricsfromVM(vm *vmapi.VirtualMachine, nodeName string, perVMMetrics *PerVMMetrics) {
	if vm.Status.Node != nodeName {
		return
	}
	commonLabels := prometheus.Labels{
		"vm_namespace": vm.Namespace,
		"vm_name":      vm.Name,
		"endpoint_id":  vm.Labels[endpointLabel],
	}
	perVMMetrics.cpu.DeletePartialMatch(commonLabels)
	perVMMetrics.memory.DeletePartialMatch(commonLabels)
}
