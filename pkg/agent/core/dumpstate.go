package core

// Implementation of (*State).Dump()

import (
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
type StateDump struct {
	Config  Config           `json:"config"`
	VM      api.VmInfo       `json:"vm"`
	Plugin  pluginStateDump  `json:"plugin"`
	Monitor monitorStateDump `json:"monitor"`
	NeonVM  neonvmStateDump  `json:"neonvm"`
	Metrics *api.Metrics     `json:"metrics"`
}

// Dump produces a JSON-serializable copy of the State
func (s *State) Dump() StateDump {
	return StateDump{
		Config:  s.internal.Config,
		VM:      s.internal.VM,
		Plugin:  s.internal.Plugin.dump(),
		Monitor: s.internal.Monitor.dump(),
		NeonVM:  s.internal.NeonVM.dump(),
		Metrics: shallowCopy(s.internal.Metrics),
	}
}

type pluginStateDump struct {
	OngoingRequest bool                 `json:"ongoingRequest"`
	ComputeUnit    *api.Resources       `json:"computeUnit"`
	LastRequest    *pluginRequestedDump `json:"lastRequest"`
	LastFailureAt  *time.Time           `json:"lastRequestAt"`
	Permit         *api.Resources       `json:"permit"`
}
type pluginRequestedDump struct {
	At        time.Time     `json:"time"`
	Resources api.Resources `json:"resources"`
}

func (s *pluginState) dump() pluginStateDump {
	var lastRequest *pluginRequestedDump
	if s.LastRequest != nil {
		lastRequest = &pluginRequestedDump{
			At:        s.LastRequest.At,
			Resources: s.LastRequest.Resources,
		}
	}

	return pluginStateDump{
		OngoingRequest: s.OngoingRequest,
		ComputeUnit:    shallowCopy(s.ComputeUnit),
		LastRequest:    lastRequest,
		LastFailureAt:  shallowCopy(s.LastFailureAt),
		Permit:         shallowCopy(s.Permit),
	}
}

type monitorStateDump struct {
	OngoingRequest     *OngoingMonitorRequestDump `json:"ongoingRequest"`
	RequestedUpscale   *requestedUpscaleDump      `json:"requestedUpscale"`
	DeniedDownscale    *deniedDownscaleDump       `json:"deniedDownscale"`
	Approved           *api.Resources             `json:"approved"`
	DownscaleFailureAt *time.Time                 `json:"downscaleFailureAt"`
	UpscaleFailureAt   *time.Time                 `json:"upscaleFailureAt"`
}
type OngoingMonitorRequestDump struct {
	Kind      monitorRequestKind `json:"kind"`
	Requested api.Resources      `json:"resources"`
}
type requestedUpscaleDump struct {
	At        time.Time         `json:"at"`
	Base      api.Resources     `json:"base"`
	Requested api.MoreResources `json:"requested"`
}
type deniedDownscaleDump struct {
	At        time.Time     `json:"at"`
	Current   api.Resources `json:"current"`
	Requested api.Resources `json:"requested"`
}

func (s *monitorState) dump() monitorStateDump {
	var requestedUpscale *requestedUpscaleDump
	if s.RequestedUpscale != nil {
		requestedUpscale = &requestedUpscaleDump{
			At:        s.RequestedUpscale.At,
			Base:      s.RequestedUpscale.Base,
			Requested: s.RequestedUpscale.Requested,
		}
	}

	var deniedDownscale *deniedDownscaleDump
	if s.DeniedDownscale != nil {
		deniedDownscale = &deniedDownscaleDump{
			At:        s.DeniedDownscale.At,
			Current:   s.DeniedDownscale.Current,
			Requested: s.DeniedDownscale.Requested,
		}
	}

	var ongoingRequest *OngoingMonitorRequestDump
	if s.OngoingRequest != nil {
		ongoingRequest = &OngoingMonitorRequestDump{
			Kind:      s.OngoingRequest.Kind,
			Requested: s.OngoingRequest.Requested,
		}
	}

	return monitorStateDump{
		OngoingRequest:     ongoingRequest,
		RequestedUpscale:   requestedUpscale,
		DeniedDownscale:    deniedDownscale,
		Approved:           shallowCopy(s.Approved),
		DownscaleFailureAt: shallowCopy(s.DownscaleFailureAt),
		UpscaleFailureAt:   shallowCopy(s.UpscaleFailureAt),
	}
}

type neonvmStateDump struct {
	LastSuccess      *api.Resources `json:"lastSuccess"`
	OngoingRequested *api.Resources `json:"ongoingRequested"`
	RequestFailedAt  *time.Time     `json:"requestFailedAt"`
}

func (s *neonvmState) dump() neonvmStateDump {
	return neonvmStateDump{
		LastSuccess:      shallowCopy(s.LastSuccess),
		OngoingRequested: shallowCopy(s.OngoingRequested),
		RequestFailedAt:  shallowCopy(s.RequestFailedAt),
	}
}
