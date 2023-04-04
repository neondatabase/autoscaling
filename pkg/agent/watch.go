package agent

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type vmEvent struct {
	kind    vmEventKind
	vmInfo  api.VmInfo
	podName string
	podIP   string
}

type vmEventKind string

const (
	vmEventAdded   vmEventKind = "added"
	vmEventDeleted vmEventKind = "deleted"
)

// note: unlike startPodWatcher, we aren't able to use a field selector on VM status.node (currently; NeonVM v0.4.6)
func startVMWatcher(
	ctx context.Context,
	config *Config,
	vmClient *vmclient.Clientset,
	nodeName string,
	vmEvents chan<- vmEvent,
) (*util.WatchStore[vmapi.VirtualMachine], error) {
	return util.Watch(
		ctx,
		vmClient.NeonvmV1().VirtualMachines(corev1.NamespaceAll),
		util.WatchConfig{
			LogName: "VMs",
			// We want to be relatively snappy; don't wait for too long before retrying.
			RetryRelistAfter: util.NewTimeRange(time.Millisecond, 500, 1000),
			RetryWatchAfter:  util.NewTimeRange(time.Millisecond, 500, 1000),
		},
		util.WatchAccessors[*vmapi.VirtualMachineList, vmapi.VirtualMachine]{
			Items: func(list *vmapi.VirtualMachineList) []vmapi.VirtualMachine { return list.Items },
		},
		util.InitWatchModeDefer,
		metav1.ListOptions{},
		util.WatchHandlerFuncs[*vmapi.VirtualMachine]{
			AddFunc: func(vm *vmapi.VirtualMachine, preexisting bool) {
				if vmIsOurResponsibility(vm, config, nodeName) {
					event, err := makeVMEvent(vm, vmEventAdded)
					if err != nil {
						klog.Errorf("Erorr handling VM added: %s", err)
						return
					}
					vmEvents <- event
				}
			},
			UpdateFunc: func(oldVM, newVM *vmapi.VirtualMachine) {
				oldIsOurs := vmIsOurResponsibility(oldVM, config, nodeName)
				newIsOurs := vmIsOurResponsibility(newVM, config, nodeName)

				var vmForEvent *vmapi.VirtualMachine
				var eventKind vmEventKind

				if !oldIsOurs && newIsOurs {
					vmForEvent = newVM
					eventKind = vmEventAdded
				} else if oldIsOurs && !newIsOurs {
					vmForEvent = oldVM
					eventKind = vmEventDeleted
				} else {
					return
				}

				event, err := makeVMEvent(vmForEvent, eventKind)
				if err != nil {
					klog.Errorf("Error handling VM update: %s", err)
					return
				}

				vmEvents <- event
			},
			DeleteFunc: func(vm *vmapi.VirtualMachine, maybeStale bool) {
				if vmIsOurResponsibility(vm, config, nodeName) {
					event, err := makeVMEvent(vm, vmEventDeleted)
					if err != nil {
						klog.Errorf("Error handling VM deletion: %s", err)
						return
					}
					vmEvents <- event
				}
			},
		},
	)
}

func makeVMEvent(vm *vmapi.VirtualMachine, kind vmEventKind) (vmEvent, error) {
	info, err := api.ExtractVmInfo(vm)
	if err != nil {
		return vmEvent{}, fmt.Errorf("Error extracting VM info from %s:%s: %w", vm.Namespace, vm.Name, err)
	}

	return vmEvent{
		kind:    kind,
		vmInfo:  *info,
		podName: vm.Status.PodName,
		podIP:   vm.Status.PodIP,
	}, nil
}
