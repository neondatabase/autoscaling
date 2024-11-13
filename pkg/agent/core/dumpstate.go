package core

// Implementation of (*State).Dump()

import (
	"encoding/json"
	"time"

	"github.com/neondatabase/autoscaling/pkg/api"
)

func shallowCopy[T any](ptr *T) *T {
	if ptr == nil {
		return nil
	} else {
		x := *ptr
		return &x
	}
}

// StateDump provides introspection into the current values of the fields of State
//
// It implements json.Marshaler.
type StateDump struct {
	internal state[*StdAlgorithm]
}

func (d StateDump) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.internal)
}

// Dump produces a JSON-serializable copy of the State
func DumpState(s *State[*StdAlgorithm]) StateDump {
	return StateDump{
		internal: state[*StdAlgorithm]{
			Debug:                s.internal.Debug,
			Config:               s.internal.Config,
			VM:                   s.internal.VM,
			Plugin:               s.internal.Plugin.deepCopy(),
			Monitor:              s.internal.Monitor.deepCopy(),
			NeonVM:               s.internal.NeonVM.deepCopy(),
			Algorithm:            shallowCopy[StdAlgorithm](s.internal.Algorithm),
			TargetRevision:       s.internal.TargetRevision,
			LastDesiredResources: s.internal.LastDesiredResources,
		},
	}
}

func (s *pluginState) deepCopy() pluginState {
	return pluginState{
		OngoingRequest:  s.OngoingRequest,
		LastRequest:     shallowCopy[pluginRequested](s.LastRequest),
		LastFailureAt:   shallowCopy[time.Time](s.LastFailureAt),
		Permit:          shallowCopy[api.Resources](s.Permit),
		CurrentRevision: s.CurrentRevision,
	}
}

func (s *monitorState) deepCopy() monitorState {
	return monitorState{
		OngoingRequest:     shallowCopy[ongoingMonitorRequest](s.OngoingRequest),
		RequestedUpscale:   shallowCopy[requestedUpscale](s.RequestedUpscale),
		DeniedDownscale:    shallowCopy[deniedDownscale](s.DeniedDownscale),
		Approved:           shallowCopy[api.Resources](s.Approved),
		DownscaleFailureAt: shallowCopy[time.Time](s.DownscaleFailureAt),
		UpscaleFailureAt:   shallowCopy[time.Time](s.UpscaleFailureAt),
		CurrentRevision:    s.CurrentRevision,
	}
}

func (s *neonvmState) deepCopy() neonvmState {
	return neonvmState{
		LastSuccess:      shallowCopy[api.Resources](s.LastSuccess),
		OngoingRequested: shallowCopy[api.Resources](s.OngoingRequested),
		RequestFailedAt:  shallowCopy[time.Time](s.RequestFailedAt),
		TargetRevision:   s.TargetRevision,
		CurrentRevision:  s.CurrentRevision,
	}
}
