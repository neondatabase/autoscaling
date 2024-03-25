package core

// Implementation of (*State).Dump()

import (
	"encoding/json"
	"time"

	"github.com/neondatabase/autoscaling/pkg/api"
)

func shallowCopy[T any](ptr *T, f ...func(T) T) *T {
	if ptr == nil {
		return nil
	} else {
		x := *ptr
		if len(f) != 0 {
			x = f[0](x)
		}
		return &x
	}
}

// StateDump provides introspection into the current values of the fields of State
//
// It implements json.Marshaler.
type StateDump struct {
	internal state
}

func (d StateDump) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.internal)
}

// Dump produces a JSON-serializable copy of the State
func (s *State) Dump() StateDump {
	return StateDump{
		internal: state{
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

func (b resourceBounds) deepCopy() resourceBounds {
	return resourceBounds{
		Confirmed: shallowCopy[api.Resources](b.Confirmed),
		Lower:     b.Lower,
		Upper:     b.Upper,
	}
}

func (s *pluginState) deepCopy() pluginState {
	return pluginState{
		OngoingRequest: s.OngoingRequest,
		LastRequest:    shallowCopy[pluginRequested](s.LastRequest),
		LastFailureAt:  shallowCopy[time.Time](s.LastFailureAt),
		Permitted:      shallowCopy[resourceBounds](s.Permitted, resourceBounds.deepCopy),
	}
}

func (s *monitorState) deepCopy() monitorState {
	return monitorState{
		OngoingRequest:   shallowCopy[ongoingMonitorRequest](s.OngoingRequest),
		RequestedUpscale: shallowCopy[requestedUpscale](s.RequestedUpscale),
		DeniedDownscale:  shallowCopy[deniedDownscale](s.DeniedDownscale),
		Approved:         shallowCopy[resourceBounds](s.Approved, resourceBounds.deepCopy),
		LastFailureAt:    shallowCopy[time.Time](s.LastFailureAt),
	}
}

func (s *neonvmState) deepCopy() neonvmState {
	return neonvmState{
		LastSuccess:      shallowCopy[api.Resources](s.LastSuccess),
		OngoingRequested: shallowCopy[api.Resources](s.OngoingRequested),
		CurrentResources: s.CurrentResources.deepCopy(),
		RequestFailedAt:  shallowCopy[time.Time](s.RequestFailedAt),
	}
}
