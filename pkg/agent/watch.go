package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

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

const (
	endpointLabel = "neon/endpoint-id"
	projectLabel  = "neon/project-id"
)

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

// extractAutoscalingBounds extracts the ScalingBounds from a VM's autoscaling
// annotation, for the purpose of exposing it in per-VM metrics.
//
// We're not reusing api.ExtractVmInfo even though it also looks at the bounds
// annotation, because its data is less precise - CPU and memory values might
// come from the VM spec without us knowing.
func extractAutoscalingBounds(vm *vmapi.VirtualMachine) *api.ScalingBounds {
	boundsJSON, ok := vm.Annotations[api.AnnotationAutoscalingBounds]
	if !ok {
		return nil
	}
	var bounds api.ScalingBounds
	if err := json.Unmarshal([]byte(boundsJSON), &bounds); err != nil {
		return nil
	}
	return &bounds
}

type pair[T1 any, T2 any] struct {
	first  T1
	second T2
}

func makeVMMetric(vm *vmapi.VirtualMachine, valType vmResourceValueType, val float64) vmMetric {
	endpointID := vm.Labels[endpointLabel]
	projectID := vm.Labels[projectLabel]
	labels := makePerVMMetricsLabels(vm.Namespace, vm.Name, endpointID, projectID, valType)
	return vmMetric{
		labels: labels,
		value:  val,
	}
}

func makeVMCPUMetrics(vm *vmapi.VirtualMachine) []vmMetric {
	var metrics []vmMetric

	// metrics from spec
	specPairs := []pair[vmResourceValueType, *vmapi.MilliCPU]{
		{vmResourceValueSpecMin, vm.Spec.Guest.CPUs.Min},
		{vmResourceValueSpecMax, vm.Spec.Guest.CPUs.Max},
		{vmResourceValueSpecUse, vm.Spec.Guest.CPUs.Use},
	}
	for _, p := range specPairs {
		if p.second != nil {
			m := makeVMMetric(vm, p.first, p.second.AsFloat64())
			metrics = append(metrics, m)
		}
	}

	// metrics from status
	if vm.Status.CPUs != nil {
		m := makeVMMetric(vm, vmResourceValueStatusUse, vm.Status.CPUs.AsFloat64())
		metrics = append(metrics, m)
	}

	// metrics from autoscaling bounds annotation
	if bounds := extractAutoscalingBounds(vm); bounds != nil {
		boundPairs := []pair[vmResourceValueType, resource.Quantity]{
			{vmResourceValueAutoscalingMin, bounds.Min.CPU},
			{vmResourceValueAutoscalingMax, bounds.Max.CPU},
		}
		for _, p := range boundPairs {
			// avoid using resource.Quantity.AsApproximateFloat64() since it's quite inaccurate
			m := makeVMMetric(vm, p.first, vmapi.MilliCPUFromResourceQuantity(p.second).AsFloat64())
			metrics = append(metrics, m)
		}
	}

	return metrics
}

func makeVMMemMetrics(vm *vmapi.VirtualMachine) []vmMetric {
	var metrics []vmMetric

	memorySlotsToBytes := func(m int32) int64 {
		return vm.Spec.Guest.MemorySlotSize.Value() * int64(m)
	}

	// metrics from spec
	specPairs := []pair[vmResourceValueType, *int32]{
		{vmResourceValueSpecMin, vm.Spec.Guest.MemorySlots.Min},
		{vmResourceValueSpecMax, vm.Spec.Guest.MemorySlots.Max},
		{vmResourceValueSpecUse, vm.Spec.Guest.MemorySlots.Use},
	}
	for _, p := range specPairs {
		if p.second != nil {
			m := makeVMMetric(vm, p.first, float64(memorySlotsToBytes(*p.second)))
			metrics = append(metrics, m)
		}
	}

	// metrics from status
	if vm.Status.MemorySize != nil {
		m := makeVMMetric(vm, vmResourceValueStatusUse, float64(vm.Status.MemorySize.Value()))
		metrics = append(metrics, m)
	}

	// metrics from autoscaling bounds annotation
	if bounds := extractAutoscalingBounds(vm); bounds != nil {
		boundPairs := []pair[vmResourceValueType, resource.Quantity]{
			{vmResourceValueAutoscalingMin, bounds.Min.Mem},
			{vmResourceValueAutoscalingMax, bounds.Max.Mem},
		}
		for _, p := range boundPairs {
			m := makeVMMetric(vm, p.first, float64(p.second.Value()))
			metrics = append(metrics, m)
		}
	}

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
		// this case we don't need an in-place metric update. Either we just have
		// to add the new metrics, or delete the old ones, or nothing!
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
