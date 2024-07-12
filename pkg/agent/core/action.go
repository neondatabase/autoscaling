package core

import (
	"time"

	"go.uber.org/zap/zapcore"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
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
	LastPermit     *api.Resources        `json:"current"`
	Target         api.Resources         `json:"target"`
	Metrics        *api.Metrics          `json:"metrics"`
	TargetRevision vmv1.RevisionWithTime `json:"targetRevision"`
}

type ActionNeonVMRequest struct {
	Current        api.Resources         `json:"current"`
	Target         api.Resources         `json:"target"`
	TargetRevision vmv1.RevisionWithTime `json:"targetRevision"`
}

type ActionMonitorDownscale struct {
	Current        api.Resources         `json:"current"`
	Target         api.Resources         `json:"target"`
	TargetRevision vmv1.RevisionWithTime `json:"targetRevision"`
}

type ActionMonitorUpscale struct {
	Current        api.Resources         `json:"current"`
	Target         api.Resources         `json:"target"`
	TargetRevision vmv1.RevisionWithTime `json:"targetRevision"`
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
	_ = addObjectPtr(enc, "wait", s.Wait)
	_ = addObjectPtr(enc, "pluginRequest", s.PluginRequest)
	_ = addObjectPtr(enc, "neonvmRequest", s.NeonVMRequest)
	_ = addObjectPtr(enc, "monitorDownscale", s.MonitorDownscale)
	_ = addObjectPtr(enc, "monitorUpscale", s.MonitorUpscale)
	return nil
}

// MarshalLogObject implements zapcore.ObjectMarshaler, so that ActionWait can be used with zap.Object
func (a ActionWait) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddDuration("duration", a.Duration)
	return nil
}

// MarshalLogObject implements zapcore.ObjectMarshaler, so that ActionPluginRequest can be used with zap.Object
func (a ActionPluginRequest) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	_ = addObjectPtr(enc, "lastPermit", a.LastPermit)
	_ = enc.AddObject("target", a.Target)
	_ = enc.AddReflected("metrics", a.Metrics)
	return nil
}

// MarshalLogObject implements zapcore.ObjectMarshaler, so that ActionNeonVMRequest can be used with zap.Object
func (a ActionNeonVMRequest) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	_ = enc.AddObject("current", a.Current)
	_ = enc.AddObject("target", a.Target)
	return nil
}

// MarshalLogObject implements zapcore.ObjectMarshaler, so that ActionMonitorDownscale can be used with zap.Object
func (a ActionMonitorDownscale) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	_ = enc.AddObject("current", a.Current)
	_ = enc.AddObject("target", a.Target)
	return nil
}

// MarshalLogObject implements zapcore.ObjectMarshaler, so that ActionMonitorUpscale can be used with zap.Object
func (a ActionMonitorUpscale) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	_ = enc.AddObject("current", a.Current)
	_ = enc.AddObject("target", a.Target)
	return nil
}
