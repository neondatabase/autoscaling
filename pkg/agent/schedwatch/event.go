package schedwatch

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type WatchEvent struct {
	info SchedulerInfo
	kind eventKind
}

type eventKind string

const (
	eventKindReady   eventKind = "ready"
	eventKindDeleted eventKind = "deleted"
)

type SchedulerInfo struct {
	PodName util.NamespacedName
	UID     types.UID
	IP      string
}

func newSchedulerInfo(pod *corev1.Pod) SchedulerInfo {
	return SchedulerInfo{
		PodName: util.NamespacedName{Name: pod.Name, Namespace: pod.Namespace},
		UID:     pod.UID,
		IP:      pod.Status.PodIP,
	}
}
