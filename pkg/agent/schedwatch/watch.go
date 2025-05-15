package schedwatch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/neondatabase/autoscaling/pkg/agent/config"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

func isActivePod(pod *corev1.Pod) bool {
	return pod.Status.PodIP != "" && util.PodReady(pod)
}

type SchedulerTracker struct {
	sp *schedPods

	Stop func()
}

func (s SchedulerTracker) Get() *SchedulerInfo {
	s.sp.mu.RLock()
	defer s.sp.mu.RUnlock()

	return s.sp.current
}

type schedPods struct {
	mu      sync.RWMutex
	current *SchedulerInfo
	pods    map[types.UID]*SchedulerInfo
}

const schedulerNamespace string = "kube-system"

func schedulerLabelSelector(schedulerName string) string {
	return fmt.Sprintf("name=%s", schedulerName)
}

func StartSchedulerWatcher(
	ctx context.Context,
	parentLogger *zap.Logger,
	kubeClient *kubernetes.Clientset,
	metrics watch.Metrics,
	schedulerName string,
	schedulerConfig config.SchedulerConfig,
) (*SchedulerTracker, error) {
	logger := parentLogger.Named("watch-schedulers")

	sp := &schedPods{
		mu:      sync.RWMutex{},
		current: nil,
		pods:    make(map[types.UID]*SchedulerInfo),
	}

	store, err := watch.Watch(
		ctx,
		logger.Named("watch"),
		kubeClient.CoreV1().Pods(schedulerNamespace),
		watch.Config{
			ObjectNameLogField: "pod",
			Metrics: watch.MetricsConfig{
				Metrics:  metrics,
				Instance: "Scheduler Pod",
			},
			// We don't need to be super responsive to scheduler changes.
			RetryRelistAfter: schedulerConfig.RetryRelistIntervals.ToTimeRange(),
			RetryWatchAfter:  schedulerConfig.RetryWatchIntervals.ToTimeRange(),
		},
		watch.Accessors[*corev1.PodList, corev1.Pod]{
			Items: func(list *corev1.PodList) []corev1.Pod { return list.Items },
		},
		watch.InitModeSync,
		metav1.ListOptions{LabelSelector: schedulerLabelSelector(schedulerName)},
		watch.HandlerFuncs[*corev1.Pod]{
			AddFunc: func(pod *corev1.Pod, preexisting bool) {
				if isActivePod(pod) {
					info := newSchedulerInfo(pod)
					logger.Info("New scheduler, already ready", zap.Object("scheduler", info))
					sp.add(logger, &info)
				}
			},
			UpdateFunc: func(oldPod, newPod *corev1.Pod) {
				oldReady := isActivePod(oldPod)
				newReady := isActivePod(newPod)

				if !oldReady && newReady {
					info := newSchedulerInfo(newPod)
					logger.Info("Existing scheduler became ready", zap.Object("scheduler", info))
					sp.add(logger, &info)
				} else if oldReady && !newReady {
					info := newSchedulerInfo(newPod)
					logger.Info("Existing scheduler no longer ready", zap.Object("scheduler", info))
					sp.remove(logger, &info)
				}
			},
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				wasReady := isActivePod(pod)
				if wasReady {
					info := newSchedulerInfo(pod)
					logger.Info("Previously-ready scheduler deleted", zap.Object("scheduler", info))
					sp.remove(logger, &info)
				}
			},
		},
	)
	if err != nil {
		return nil, err
	}

	return &SchedulerTracker{
		sp:   sp,
		Stop: store.Stop,
	}, nil
}

func (s *schedPods) add(logger *zap.Logger, pod *SchedulerInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pods[pod.UID] = pod
	s.reconcile(logger)
}

func (s *schedPods) remove(logger *zap.Logger, pod *SchedulerInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.pods, pod.UID)
	s.reconcile(logger)
}

// reconcile refreshes the value of s.current based on s.pods.
// s.mu MUST be exclusively locked while calling reconcile.
func (s *schedPods) reconcile(logger *zap.Logger) {
	var newCurrent *SchedulerInfo
	// There's *basically* guaranteed to be ≤ 2 scheduler pods because the scheduler deployment has
	// replicas=1, so "just" looping here is fine; it's not worth a more complex data structure.
	for _, pod := range s.pods {
		// Use the pod if we don't already have one, or if it was created more recently than
		// whatever we've seen so far.
		// The ordering isn't *too* important here, but we need to pick one to be consistent, and
		// preferring a newer scheduler (remember: the pod is 'Ready') is likely to be more correct.
		if newCurrent == nil || newCurrent.CreationTimestamp.Before(pod.CreationTimestamp) {
			newCurrent = pod
		}
	}

	if s.current != nil && newCurrent != nil {
		count := len(s.pods)
		if s.current.UID != newCurrent.UID {
			logger.Info("Scheduler pod selection changed", zap.Int("count", count), zap.Object("scheduler", newCurrent))
		} else {
			logger.Info("Scheduler pod selection is unchanged", zap.Int("count", count), zap.Object("scheduler", newCurrent))
		}
	} else if newCurrent == nil && s.current != nil {
		logger.Warn("No scheduler pod available anymore")
	} else if newCurrent != nil && s.current == nil {
		logger.Info("Scheduler pod now available (there was none before)", zap.Object("scheduler", newCurrent))
	} else /* newCurrent == nil && s.current.pod == nil */ {
		logger.Warn("No scheduler pod available (still)")
	}
	s.current = newCurrent
}
