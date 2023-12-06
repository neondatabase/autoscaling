package schedwatch

import (
	"fmt"
	"time"

	"go.uber.org/zap/zapcore"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type SchedulerInfo struct {
	PodName           util.NamespacedName
	UID               types.UID
	IP                string
	CreationTimestamp time.Time
}

// MarshalLogObject implements zapcore.ObjectMarshaler
func (s SchedulerInfo) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if err := enc.AddObject("pod", s.PodName); err != nil {
		return err
	}
	enc.AddString("uid", string(s.UID))
	enc.AddString("ip", string(s.IP))
	enc.AddTime("creationTimestamp", s.CreationTimestamp)
	return nil
}

func newSchedulerInfo(pod *corev1.Pod) SchedulerInfo {
	return SchedulerInfo{
		PodName:           util.NamespacedName{Name: pod.Name, Namespace: pod.Namespace},
		UID:               pod.UID,
		IP:                pod.Status.PodIP,
		CreationTimestamp: pod.CreationTimestamp.Time,
	}
}

func (info SchedulerInfo) Format(state fmt.State, verb rune) {
	switch {
	case verb == 'v' && state.Flag('#'):
		state.Write([]byte(fmt.Sprintf(
			"schedwatch.SchedulerInfo{PodName:%#v, IP:%q, UID:%q, CreationTimestamp: %#v}",
			info.PodName, info.IP, string(info.UID), info.CreationTimestamp,
		)))
	default:
		if verb != 'v' {
			state.Write([]byte("%!"))
			state.Write([]byte(string(verb)))
			state.Write([]byte("(schedwatch.SchedulerInfo="))
		}

		state.Write([]byte(fmt.Sprintf(
			"{PodName:%v IP:%q UID:%q CreationTimestamp:%q}",
			info.PodName, info.IP, info.UID, info.CreationTimestamp,
		)))

		if verb != 'v' {
			state.Write([]byte{')'})
		}
	}
}
