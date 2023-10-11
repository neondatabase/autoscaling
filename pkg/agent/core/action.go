package core

import (
	"time"

	"go.uber.org/zap/zapcore"

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

func addObjectPtr[T zapcore.ObjectMarshaler](enc zapcore.ObjectEncoder, key string, value *T) error {
	if value != nil {
		return enc.AddObject(key, *value)
	} else {
		// nil ObjectMarshaler is not sound, but nil reflected is, and it shortcuts reflection
		return enc.AddReflected(key, nil)
	}
}

func (s ActionSet) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	addObjectPtr(enc, "wait", s.Wait)
	addObjectPtr(enc, "pluginRequest", s.PluginRequest)
	addObjectPtr(enc, "neonvmRequest", s.NeonVMRequest)
	addObjectPtr(enc, "monitorDownscale", s.MonitorDownscale)
	addObjectPtr(enc, "monitorUpscale", s.MonitorUpscale)
	return nil
}

// MarshalLogObject implements zapcore.ObjectMarshaler, so that ActionWait can be used with zap.Object
func (a ActionWait) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddDuration("duration", a.Duration)
	return nil
}

// MarshalLogObject implements zapcore.ObjectMarshaler, so that ActionPluginRequest can be used with zap.Object
func (a ActionPluginRequest) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	addObjectPtr(enc, "lastPermit", a.LastPermit)
	enc.AddObject("target", a.Target)
	enc.AddReflected("metrics", a.Metrics)
	return nil
}

// MarshalLogObject implements zapcore.ObjectMarshaler, so that ActionNeonVMRequest can be used with zap.Object
func (a ActionNeonVMRequest) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddObject("current", a.Current)
	enc.AddObject("target", a.Target)
	return nil
}

// MarshalLogObject implements zapcore.ObjectMarshaler, so that ActionMonitorDownscale can be used with zap.Object
func (a ActionMonitorDownscale) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddObject("current", a.Current)
	enc.AddObject("target", a.Target)
	return nil
}

// MarshalLogObject implements zapcore.ObjectMarshaler, so that ActionMonitorUpscale can be used with zap.Object
func (a ActionMonitorUpscale) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddObject("current", a.Current)
	enc.AddObject("target", a.Target)
	return nil
}
