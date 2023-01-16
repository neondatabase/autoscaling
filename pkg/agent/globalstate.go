package agent

import (
	"context"
	"sync"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"
	"github.com/tychoish/fun"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// agentState is the global state for the autoscaler agent
type agentState struct {
	pods                 map[api.PodName]*podState
	podIP                string
	config               *Config
	kubeClient           *kubernetes.Clientset
	vmClient             *vmclient.Clientset
	schedulerEventBroker *fun.Broker[watchEvent]
	schedulerStore       *util.WatchStore[corev1.Pod]
}

func (r MainRunner) newAgentState(podIP string, broker *fun.Broker[watchEvent], schedulerStore *util.WatchStore[corev1.Pod]) agentState {
	return agentState{
		pods:                 make(map[api.PodName]*podState),
		config:               r.Config,
		kubeClient:           r.KubeClient,
		vmClient:             r.VMClient,
		podIP:                podIP,
		schedulerEventBroker: broker,
		schedulerStore:       schedulerStore,
	}
}

func podIsOurResponsibility(pod *corev1.Pod, config *Config, nodeName string) bool {
	return pod.Spec.NodeName == nodeName &&
		pod.Status.PodIP != "" &&
		pod.Spec.SchedulerName == config.Scheduler.SchedulerName &&
		util.PodReady(pod)
}

func (s *agentState) Stop() {
	for _, pod := range s.pods {
		pod.stop.Send()
	}
}

func (s *agentState) handleEvent(event podEvent) {
	klog.Infof("Handling pod event %+v", event)

	state, hasPod := s.pods[event.podName]

	switch event.kind {
	case podEventDeleted:
		if !hasPod {
			klog.Errorf("Received delete event for pod %v that isn't present", event.podName)
			return
		}

		state.deleted.Send()
		delete(s.pods, event.podName)
	case podEventAdded:
		if hasPod {
			klog.Errorf("Received add event for pod %v while already present", event.podName)
			return
		}

		sendStop, recvStop := util.NewSingleSignalPair()
		sendDeleted, recvDeleted := util.NewSingleSignalPair()

		runner := runner{
			config:     s.config,
			vmClient:   s.vmClient,
			kubeClient: s.kubeClient,
			thisIP:     s.podIP,
			podName:    event.podName,
			podIP:      event.podIP,
			vm: &api.VmInfo{
				Name:      event.vmName,
				Namespace: event.podName.Namespace,
			},
			stop:                 recvStop,
			deleted:              recvDeleted,
			schedulerEventBroker: s.schedulerEventBroker,
			schedulerStore:       s.schedulerStore,
		}

		state = &podState{
			podName: event.podName,
			stop:    sendStop,
			deleted: sendDeleted,
			status: podStatus{
				lock:      sync.Mutex{},
				errored:   nil,
				migrating: false,
			},
		}
		s.pods[event.podName] = state
		runner.Spawn(context.Background(), &state.status)
	default:
		panic("bad event: unexpected event kind")
	}
}

type podState struct {
	podName api.PodName
	stop    util.SignalSender
	deleted util.SignalSender

	status podStatus
}

type podStatus struct {
	lock      sync.Mutex
	done      bool // if true, the runner finished
	errored   error
	migrating bool
	panicked  bool // if true, errored will be non-nil
}
