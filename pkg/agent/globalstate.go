package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tychoish/fun"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
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
		pod.stop()
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

		state.stop()
		delete(s.pods, event.podName)
	case podEventAdded:
		if hasPod {
			klog.Errorf("Received add event for pod %v while already present", event.podName)
			return
		}

		runnerCtx, cancelRunnerContext := context.WithCancel(context.TODO())

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
			podName:               event.podName,
			podIP:                 event.podIP,
			lock:                  util.NewChanMutex(),
			vmStateLock:           util.NewChanMutex(),
			lastMetrics:           nil,
			scheduler:             nil,
			server:                nil,
			informant:             nil,
			computeUnit:           nil,
			lastApproved:          nil,
			lastSchedulerError:    nil,
			backgroundWorkerCount: atomic.Int64{},
			backgroundPanic:       make(chan error),
		}

		state = &podState{
			podName: event.podName,
			stop:    cancelRunnerContext,
			status:  status,
		}
		s.pods[event.podName] = state
		runner.Spawn(runnerCtx, event.vmName)
	default:
		panic(errors.New("bad event: unexpected event kind"))
	}
}

type podState struct {
	podName api.PodName

	stop   context.CancelFunc
	runner *Runner //nolint:unused // this will be used soon, with an HTTP endpoint to dump state
	status *podStatus
}

type podStatus struct {
	lock     sync.Mutex
	done     bool // if true, the runner finished
	errored  error
	panicked bool // if true, errored will be non-nil
}
