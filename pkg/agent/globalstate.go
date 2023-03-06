package agent

import (
	"errors"
	"fmt"
	"sync"

	"github.com/tychoish/fun/pubsub"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/task"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// agentState is the global state for the autoscaler agent
//
// All fields are immutable, except pods.
type agentState struct {
	pods                 map[api.PodName]*podState
	podIP                string
	config               *Config
	kubeClient           *kubernetes.Clientset
	vmClient             *vmclient.Clientset
	schedulerEventBroker *pubsub.Broker[watchEvent]
	schedulerStore       *util.WatchStore[corev1.Pod]
}

func (r MainRunner) newAgentState(podIP string, broker *pubsub.Broker[watchEvent], schedulerStore *util.WatchStore[corev1.Pod]) agentState {
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

func (s *agentState) Stop(tm task.Manager) {
	handle := tm.SpawnAsSubgroup("shutdown-runners", func(tm task.Manager) {
		for name, pod := range s.pods {
			name, pod := name, pod
			tm.Spawn(fmt.Sprintf("stop-%v", name), func(tm task.Manager) {
				if err := pod.group.Shutdown(tm.Context()); err != nil {
					klog.Errorf("Error while stopping Runner %v: %s", name, err)
				}
			})
		}
	})

	if err := handle.TryWait(tm.Context()); err != nil {
		klog.Warningf("Error while waiting for Runners to shut down: %s", err)
	}
}

func (s *agentState) handleEvent(tm task.Manager, event podEvent) {
	klog.Infof("Handling pod event %+v", event)

	state, hasPod := s.pods[event.podName]

	switch event.kind {
	case podEventDeleted:
		if !hasPod {
			klog.Errorf("Received delete event for pod %v that isn't present", event.podName)
			return
		}

		tm.Spawn(fmt.Sprintf("stop-%v", event.podName), func(tm task.Manager) {
			if err := state.group.Shutdown(makeShutdownContext()); err != nil {
				klog.Errorf("Error while stopping Runner %v: %w", event.podName, err)
			}
		})
		delete(s.pods, event.podName)
	case podEventAdded:
		if hasPod {
			klog.Errorf("Received add event for pod %v while already present", event.podName)
			return
		}

		status := &podStatus{
			lock:     sync.Mutex{},
			done:     false,
			errored:  nil,
			panicked: false,
		}

		runner := &Runner{
			global: s,
			status: status,
			logger: RunnerLogger{
				prefix: fmt.Sprintf("Runner %v: ", event.podName),
			},
			// note: vm is expected to be nil before (*Runner).Run
			vm:                 nil,
			podName:            event.podName,
			podIP:              event.podIP,
			lock:               util.NewChanMutex(),
			vmStateLock:        util.NewChanMutex(),
			requestedUpscale:   api.MoreResources{Cpu: false, Memory: false},
			lastMetrics:        nil,
			scheduler:          nil,
			server:             nil,
			informant:          nil,
			computeUnit:        nil,
			lastApproved:       nil,
			lastSchedulerError: nil,
			lastInformantError: nil,
		}

		runnerGroup := runner.Spawn(tm, event.vmName)

		state = &podState{
			podName: event.podName,
			group:   runnerGroup,
			runner:  runner,
			status:  status,
		}
		s.pods[event.podName] = state
	default:
		panic(errors.New("bad event: unexpected event kind"))
	}
}

type podState struct {
	podName api.PodName

	group  task.SubgroupHandle
	runner *Runner
	status *podStatus
}

type podStatus struct {
	lock     sync.Mutex
	done     bool // if true, the runner finished
	errored  error
	panicked bool // if true, errored will be non-nil
}
