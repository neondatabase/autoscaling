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
		Config:  s.config,
		VM:      s.vm,
		Plugin:  s.plugin.dump(),
		Monitor: s.monitor.dump(),
		NeonVM:  s.neonvm.dump(),
		Metrics: shallowCopy(s.metrics),
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
	if s.lastRequest != nil {
		lastRequest = &pluginRequestedDump{
			At:        s.lastRequest.at,
			Resources: s.lastRequest.resources,
		}
	}

	return pluginStateDump{
		OngoingRequest: s.ongoingRequest,
		ComputeUnit:    shallowCopy(s.computeUnit),
		LastRequest:    lastRequest,
		LastFailureAt:  shallowCopy(s.lastFailureAt),
		Permit:         shallowCopy(s.permit),
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
	if s.requestedUpscale != nil {
		requestedUpscale = &requestedUpscaleDump{
			At:        s.requestedUpscale.at,
			Base:      s.requestedUpscale.base,
			Requested: s.requestedUpscale.requested,
		}
	}

	var deniedDownscale *deniedDownscaleDump
	if s.deniedDownscale != nil {
		deniedDownscale = &deniedDownscaleDump{
			At:        s.deniedDownscale.at,
			Current:   s.deniedDownscale.current,
			Requested: s.deniedDownscale.requested,
		}
	}

	var ongoingRequest *OngoingMonitorRequestDump
	if s.ongoingRequest != nil {
		ongoingRequest = &OngoingMonitorRequestDump{
			Kind:      s.ongoingRequest.kind,
			Requested: s.ongoingRequest.requested,
		}
	}

	return monitorStateDump{
		OngoingRequest:     ongoingRequest,
		RequestedUpscale:   requestedUpscale,
		DeniedDownscale:    deniedDownscale,
		Approved:           shallowCopy(s.approved),
		DownscaleFailureAt: shallowCopy(s.downscaleFailureAt),
		UpscaleFailureAt:   shallowCopy(s.upscaleFailureAt),
	}
}

type neonvmStateDump struct {
	LastSuccess      *api.Resources `json:"lastSuccess"`
	OngoingRequested *api.Resources `json:"ongoingRequested"`
	RequestFailedAt  *time.Time     `json:"requestFailedAt"`
}

func (s *neonvmState) dump() neonvmStateDump {
	return neonvmStateDump{
		LastSuccess:      shallowCopy(s.lastSuccess),
		OngoingRequested: shallowCopy(s.ongoingRequested),
		RequestFailedAt:  shallowCopy(s.requestFailedAt),
	}
}
