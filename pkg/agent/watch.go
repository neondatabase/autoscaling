package agent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"

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
				setVMMetrics(&perVMMetrics, vm, nodeName)

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
				updateVMMetrics(&perVMMetrics, oldVM, newVM, nodeName)

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
				deleteVMMetrics(&perVMMetrics, vm, nodeName)

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

func makeVMCPUMetrics(vm *vmapi.VirtualMachine) []vmMetric {
	var metrics []vmMetric

	addCPUMetric := func(resValType vmResourceValueType, val *vmapi.MilliCPU) {
		if val == nil {
			return
		}
		labels := makeLabels(vm.Namespace, vm.Name, vm.Labels[endpointLabel], resValType)
		metrics = append(metrics, vmMetric{
			labels: labels,
			value:  val.AsFloat64(),
		})
	}

	addCPUMetric(vmResourceValueMin, vm.Spec.Guest.CPUs.Min)
	addCPUMetric(vmResourceValueMax, vm.Spec.Guest.CPUs.Max)
	addCPUMetric(vmResourceValueSpecUse, vm.Spec.Guest.CPUs.Use)
	addCPUMetric(vmResourceValueStatusUse, vm.Status.CPUs)
	return metrics
}

func makeVMMemMetrics(vm *vmapi.VirtualMachine) []vmMetric {
	var metrics []vmMetric

	addMemMetric := func(resValType vmResourceValueType, val *int64) {
		if val == nil {
			return
		}
		labels := makeLabels(vm.Namespace, vm.Name, vm.Labels[endpointLabel], resValType)
		metrics = append(metrics, vmMetric{
			labels: labels,
			value:  float64(*val),
		})
	}

	specMemVal := func(m *int32) *int64 {
		if m == nil {
			return nil
		}
		memValue := vm.Spec.Guest.MemorySlotSize.Value() * int64(*m)
		return &memValue
	}
	statusMemVal := func(m *resource.Quantity) *int64 {
		memValue := m.Value()
		return &memValue
	}

	addMemMetric(vmResourceValueMin, specMemVal(vm.Spec.Guest.MemorySlots.Min))
	addMemMetric(vmResourceValueMax, specMemVal(vm.Spec.Guest.MemorySlots.Max))
	addMemMetric(vmResourceValueSpecUse, specMemVal(vm.Spec.Guest.MemorySlots.Use))
	addMemMetric(vmResourceValueStatusUse, statusMemVal(vm.Status.MemorySize))
	return metrics
}

func setVMMetrics(perVMMetrics *PerVMMetrics, vm *vmapi.VirtualMachine, nodeName string) {
	if vm.Status.Node != nodeName {
		return
	}

	cpuMetrics := makeVMCPUMetrics(vm)
	for _, m := range cpuMetrics {
		perVMMetrics.cpu.With(m.labels).Set(m.value)
	}

	memMetrics := makeVMMemMetrics(vm)
	for _, m := range memMetrics {
		perVMMetrics.memory.With(m.labels).Set(m.value)
	}
}

func updateVMMetrics(perVMMetrics *PerVMMetrics, oldVM, newVM *vmapi.VirtualMachine, nodeName string) {
	if newVM.Status.Node != nodeName || oldVM.Status.Node != nodeName {
		// The vm is either has been removed from the nodeName or has been added
		// there.
		deleteVMMetrics(perVMMetrics, oldVM, nodeName)
		setVMMetrics(perVMMetrics, newVM, nodeName)
		return
	}

	updateMetrics := func(gauge *prometheus.GaugeVec, oldMetrics, newMetrics []vmMetric) {
		for _, m := range oldMetrics {
			// this is a linear search, but since we have small number (~10) of
			// different metrics for each vm, this should be fine.
			ok := slices.ContainsFunc(newMetrics, func(vm vmMetric) bool {
				return maps.Equal(m.labels, vm.labels)
			})
			if !ok {
				gauge.Delete(m.labels)
			}
		}
		for _, m := range newMetrics {
			gauge.With(m.labels).Set(m.value)
		}
	}

	oldCPUMetrics := makeVMCPUMetrics(oldVM)
	newCPUMetrics := makeVMCPUMetrics(newVM)
	updateMetrics(perVMMetrics.cpu, oldCPUMetrics, newCPUMetrics)

	oldMemMetrics := makeVMMemMetrics(oldVM)
	newMemMetrics := makeVMMemMetrics(newVM)
	updateMetrics(perVMMetrics.memory, oldMemMetrics, newMemMetrics)
}

func deleteVMMetrics(perVMMetrics *PerVMMetrics, vm *vmapi.VirtualMachine, nodeName string) {
	if vm.Status.Node != nodeName {
		return
	}

	cpuMetrics := makeVMCPUMetrics(vm)
	for _, m := range cpuMetrics {
		perVMMetrics.cpu.Delete(m.labels)
	}

	memMetrics := makeVMMemMetrics(vm)
	for _, m := range memMetrics {
		perVMMetrics.memory.Delete(m.labels)
	}
}
