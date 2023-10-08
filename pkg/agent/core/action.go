package core

import (
	"time"

	"github.com/neondatabase/autoscaling/pkg/api"
)

type ActionSet struct {
	Wait             *ActionWait             `json:"wait,omitempty"`
	PluginRequest    *ActionPluginRequest    `json:"pluginRequest,omitempty"`
	NeonVMRequest    *ActionNeonVMRequest    `json:"neonvmRequest,omitempty"`
	MonitorDownscale *ActionMonitorDownscale `json:"monitorDownscale,omitempty"`
	MonitorUpscale   *ActionMonitorUpscale   `json:"monitorUpscale,omitempty"`
}

type ActionWait struct {
	Duration time.Duration `json:"duration"`
}

type ActionPluginRequest struct {
	LastPermit *api.Resources `json:"current"`
	Target     api.Resources  `json:"target"`
	Metrics    *api.Metrics   `json:"metrics"`
}

type ActionNeonVMRequest struct {
	Current api.Resources `json:"current"`
	Target  api.Resources `json:"target"`
}

type ActionMonitorDownscale struct {
	Current api.Resources `json:"current"`
	Target  api.Resources `json:"target"`
}

type ActionMonitorUpscale struct {
	Current api.Resources `json:"current"`
	Target  api.Resources `json:"target"`
}
