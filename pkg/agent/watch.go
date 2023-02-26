package agent

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	vmapi "github.com/neondatabase/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type podEvent struct {
	kind    podEventKind
	vmName  string
	podName api.PodName
	podIP   string
}

type podEventKind string

const (
	podEventAdded   podEventKind = "added"
	podEventDeleted podEventKind = "deleted"
)

func startPodWatcher(
	ctx context.Context,
	config *Config,
	kubeClient *kubernetes.Clientset,
	nodeName string,
	podEvents chan<- podEvent,
) (*util.WatchStore[corev1.Pod], error) {
	return util.Watch(
		ctx,
		kubeClient.CoreV1().Pods(corev1.NamespaceAll),
		util.WatchConfig{
			LogName: "pods",
			// Detecting new/deleted pods is not *critical*; we're ok retrying on a longer duration.
			RetryRelistAfter: util.NewTimeRange(time.Second, 3, 5),
			RetryWatchAfter:  util.NewTimeRange(time.Second, 3, 5),
		},
		util.WatchAccessors[*corev1.PodList, corev1.Pod]{
			Items: func(list *corev1.PodList) []corev1.Pod { return list.Items },
		},
		util.InitWatchModeDefer,
		metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
		},
		util.WatchHandlerFuncs[*corev1.Pod]{
			AddFunc: func(pod *corev1.Pod, preexisting bool) {
				vmName, podHasVM := pod.Labels[vmapi.VirtualMachineNameLabel]
				if podHasVM && podIsOurResponsibility(pod, config, nodeName) {
					podEvents <- podEvent{
						podName: api.PodName{Name: pod.Name, Namespace: pod.Namespace},
						podIP:   pod.Status.PodIP,
						vmName:  vmName,
						kind:    podEventAdded,
					}
				}
			},
			UpdateFunc: func(oldPod, newPod *corev1.Pod) {
				vmName, podHasVM := newPod.Labels[vmapi.VirtualMachineNameLabel]
				if podHasVM {
					oldIsOurs := podIsOurResponsibility(oldPod, config, nodeName)
					newIsOurs := podIsOurResponsibility(newPod, config, nodeName)

					var kind podEventKind
					var podIP string

					if !oldIsOurs && newIsOurs {
						kind = podEventAdded
						podIP = newPod.Status.PodIP
					} else if oldIsOurs && !newIsOurs {
						kind = podEventDeleted
						podIP = oldPod.Status.PodIP
					} else {
						// If oldIsOurs == newIsOurs, nothing's changed, so do nothing.
						return
					}

					podEvents <- podEvent{
						// doesn't matter which pod we take these from; they won't change.
						podName: api.PodName{Name: newPod.Name, Namespace: newPod.Namespace},
						podIP:   podIP,
						vmName:  vmName,
						kind:    kind,
					}
				}
			},
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				vmName, podHadVM := pod.Labels[vmapi.VirtualMachineNameLabel]
				if podHadVM && podIsOurResponsibility(pod, config, nodeName) {
					podEvents <- podEvent{
						podName: api.PodName{Name: pod.Name, Namespace: pod.Namespace},
						podIP:   pod.Status.PodIP,
						vmName:  vmName,
						kind:    podEventDeleted,
					}
				}
			},
		},
	)
}

// note: unlike startPodWatcher, we aren't able to use a field selector on VM status.node (currently; NeonVM v0.4.6)
func startVMWatcher(
	ctx context.Context,
	vmClient *vmclient.Clientset,
	nodeName string,
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
		util.WatchHandlerFuncs[*vmapi.VirtualMachine]{},
	)
}
