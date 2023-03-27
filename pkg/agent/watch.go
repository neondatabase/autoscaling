package agent

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	podEvents chan<- vmEvent,
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
					podEvents <- makeVMEvent(vm, vmEventAdded)
				}
			},
			UpdateFunc: func(oldVM, newVM *vmapi.VirtualMachine) {
				oldIsOurs := vmIsOurResponsibility(oldVM, config, nodeName)
				newIsOurs := vmIsOurResponsibility(newVM, config, nodeName)

				if !oldIsOurs && newIsOurs {
					podEvents <- makeVMEvent(newVM, vmEventAdded)
				} else if oldIsOurs && !newIsOurs {
					podEvents <- makeVMEvent(oldVM, vmEventDeleted)
				}
			},
			DeleteFunc: func(vm *vmapi.VirtualMachine, maybeStale bool) {
				if vmIsOurResponsibility(vm, config, nodeName) {
					podEvents <- makeVMEvent(vm, vmEventDeleted)
				}
			},
		},
	)
}

func makeVMEvent(vm *vmapi.VirtualMachine, kind vmEventKind) vmEvent {
	info, err := api.ExtractVmInfo(vm)
	if err != nil {
		panic(fmt.Errorf("unexpected failure while extracting VM info from %s:%s: %w", vm.Namespace, vm.Name, err))
	}

	return vmEvent{
		kind:    kind,
		vmInfo:  *info,
		podName: vm.Status.PodName,
		podIP:   vm.Status.PodIP,
	}
}
