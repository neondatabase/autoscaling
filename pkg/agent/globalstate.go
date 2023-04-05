package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tychoish/fun/pubsub"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// agentState is the global state for the autoscaler agent
//
// All fields are immutable, except pods.
type agentState struct {
	// lock guards access to pods
	lock util.ChanMutex
	pods map[api.PodName]*podState

	podIP                string
	config               *Config
	kubeClient           *kubernetes.Clientset
	vmClient             *vmclient.Clientset
	schedulerEventBroker *pubsub.Broker[watchEvent]
	schedulerStore       *util.WatchStore[corev1.Pod]
}

func (r MainRunner) newAgentState(podIP string, broker *pubsub.Broker[watchEvent], schedulerStore *util.WatchStore[corev1.Pod]) agentState {
	return agentState{
		lock:                 util.NewChanMutex(),
		pods:                 make(map[api.PodName]*podState),
		config:               r.Config,
		kubeClient:           r.KubeClient,
		vmClient:             r.VMClient,
		podIP:                podIP,
		schedulerEventBroker: broker,
		schedulerStore:       schedulerStore,
	}
}

func vmIsOurResponsibility(vm *vmapi.VirtualMachine, config *Config, nodeName string) bool {
	return vm.Status.Node == nodeName &&
		vm.Status.Phase == vmapi.VmRunning &&
		vm.Status.PodIP != "" &&
		vm.Spec.SchedulerName == config.Scheduler.SchedulerName
}

func (s *agentState) Stop() {
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, pod := range s.pods {
		pod.stop()
	}
}

func (s *agentState) handleEvent(ctx context.Context, event vmEvent) {
	klog.Infof("Handling pod event %+v", event)

	if err := s.lock.TryLock(ctx); err != nil {
		klog.Warningf("context canceled while starting to handle event: %s", err)
		return
	}
	defer s.lock.Unlock()

	podName := api.PodName{Namespace: event.vmInfo.Namespace, Name: event.podName}
	state, hasPod := s.pods[podName]

	switch event.kind {
	case vmEventDeleted:
		if !hasPod {
			klog.Errorf("Received delete event for pod %v that isn't present", event.podName)
			return
		}

		state.stop()
		delete(s.pods, podName)
	case vmEventAdded:
		if hasPod {
			klog.Errorf("Received add event for pod %v while already present", event.podName)
			return
		}

		runnerCtx, cancelRunnerContext := context.WithCancel(ctx)

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
			schedulerRespondedWithMigration: false,

			shutdown:              cancelRunnerContext,
			vm:                    event.vmInfo,
			podName:               podName,
			podIP:                 event.podIP,
			lock:                  util.NewChanMutex(),
			vmStateLock:           util.NewChanMutex(),
			requestedUpscale:      api.MoreResources{Cpu: false, Memory: false},
			lastMetrics:           nil,
			scheduler:             nil,
			server:                nil,
			informant:             nil,
			computeUnit:           nil,
			lastApproved:          nil,
			lastSchedulerError:    nil,
			lastInformantError:    nil,
			backgroundWorkerCount: atomic.Int64{},
			backgroundPanic:       make(chan error),
		}

		state = &podState{
			podName: podName,
			stop:    cancelRunnerContext,
			runner:  runner,
			status:  status,
		}
		s.pods[podName] = state
		runner.Spawn(runnerCtx)
	default:
		panic(errors.New("bad event: unexpected event kind"))
	}
}

type podState struct {
	podName api.PodName

	stop   context.CancelFunc
	runner *Runner
	status *podStatus
}

type podStateDump struct {
	PodName         api.PodName   `json:"podName"`
	Status          podStatusDump `json:"status"`
	Runner          *RunnerState  `json:"runner,omitempty"`
	CollectionError error         `json:"collectionError,omitempty"`
}

func (p *podState) dump(ctx context.Context) podStateDump {
	status := p.status.dump()
	runner, collectErr := p.runner.State(ctx)
	if collectErr != nil {
		collectErr = fmt.Errorf("error reading runner state: %w", collectErr)
	}
	return podStateDump{
		PodName:         p.podName,
		Status:          status,
		Runner:          runner,
		CollectionError: collectErr,
	}
}

type podStatus struct {
	lock     sync.Mutex
	done     bool // if true, the runner finished
	errored  error
	panicked bool // if true, errored will be non-nil
}

type podStatusDump struct {
	Done     bool  `json:"done"`
	Errored  error `json:"errored"`
	Panicked bool  `json:"panicked"`
}

func (s *podStatus) dump() podStatusDump {
	s.lock.Lock()
	defer s.lock.Unlock()

	return podStatusDump{
		Done:     s.done,
		Errored:  s.errored,
		Panicked: s.panicked,
	}
}
