package agent

import (
	"context"
	"fmt"
	"time"

	vmapi "github.com/neondatabase/neonvm/apis/neonvm/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

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

					// doesn't matter which pod we take these from; they won't change.
					podName := api.PodName{Name: newPod.Name, Namespace: newPod.Namespace}

					event := podEvent{podName: podName, vmName: vmName}

					if !oldIsOurs && newIsOurs {
						event.kind = podEventAdded
						event.podIP = newPod.Status.PodIP
						podEvents <- event
					} else if oldIsOurs && !newIsOurs {
						event.kind = podEventDeleted
						event.podIP = oldPod.Status.PodIP
						podEvents <- event
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
