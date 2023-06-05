package schedwatch

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type WatchEvent struct {
	info SchedulerInfo
	kind eventKind
}

func (ev WatchEvent) Format(state fmt.State, verb rune) {
	switch {
	case verb == 'v' && state.Flag('#'):
		state.Write([]byte(fmt.Sprintf(
			"schedwatch.WatchEvent{kind:%q, info:%#v}",
			string(ev.kind), ev.info,
		)))
	default:
		if verb != 'v' {
			state.Write([]byte("%!"))
			state.Write([]byte(string(verb)))
			state.Write([]byte("(schedwatch.WatchEvent="))
		}

		state.Write([]byte(fmt.Sprintf(
			"{kind:%v info:%v}",
			ev.kind, ev.info,
		)))

		if verb != 'v' {
			state.Write([]byte{')'})
		}
	}
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

func (info SchedulerInfo) Format(state fmt.State, verb rune) {
	switch {
	case verb == 'v' && state.Flag('#'):
		state.Write([]byte(fmt.Sprintf(
			"schedwatch.SchedulerInfo{PodName:%#v, IP:%q, UID:%q}",
			info.PodName, info.IP, string(info.UID),
		)))
	default:
		if verb != 'v' {
			state.Write([]byte("%!"))
			state.Write([]byte(string(verb)))
			state.Write([]byte("(schedwatch.SchedulerInfo="))
		}

		state.Write([]byte(fmt.Sprintf(
			"{PodName:%v IP:%q UID:%q}",
			info.PodName, info.IP, info.UID,
		)))

		if verb != 'v' {
			state.Write([]byte{')'})
		}
	}
}
