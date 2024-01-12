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
type StateDump[M ToAPIMetrics] struct {
	internal state[M]
}

func (d StateDump[M]) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.internal)
}

// Dump produces a JSON-serializable copy of the State
func (s *State[M]) Dump() StateDump[M] {
	return StateDump[M]{
		internal: state[M]{
			Debug:   s.internal.Debug,
			Config:  s.internal.Config,
			VM:      s.internal.VM,
			Plugin:  s.internal.Plugin.deepCopy(),
			Monitor: s.internal.Monitor.deepCopy(),
			NeonVM:  s.internal.NeonVM.deepCopy(),
			Metrics: shallowCopy[api.Metrics](s.internal.Metrics),
		},
	}
}

func (s *pluginState) deepCopy() pluginState {
	return pluginState{
		OngoingRequest: s.OngoingRequest,
		LastRequest:    shallowCopy[pluginRequested](s.LastRequest),
		LastFailureAt:  shallowCopy[time.Time](s.LastFailureAt),
		Permit:         shallowCopy[api.Resources](s.Permit),
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
	}
}

func (s *neonvmState) deepCopy() neonvmState {
	return neonvmState{
		LastSuccess:      shallowCopy[api.Resources](s.LastSuccess),
		OngoingRequested: shallowCopy[api.Resources](s.OngoingRequested),
		RequestFailedAt:  shallowCopy[time.Time](s.RequestFailedAt),
	}
}
