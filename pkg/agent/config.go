package agent

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/neondatabase/autoscaling/pkg/agent/billing"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/tychoish/fun/ers"
)

type Config struct {
	Scaling   ScalingConfig    `json:"scaling"`
	Informant InformantConfig  `json:"informant"`
	Metrics   MetricsConfig    `json:"metrics"`
	Scheduler SchedulerConfig  `json:"scheduler"`
	Monitor   MonitorConfig    `json:"monitor"`
	Billing   *billing.Config  `json:"billing,omitempty"`
	DumpState *DumpStateConfig `json:"dumpState"`
}

type MonitorConfig struct {
	ResponseTimeoutSeconds uint `json:"responseTimeoutSeconds"`
	// ConnectionTimeoutSeconds gives how long we may take to connect to the
	// monitor before cancelling.
	ConnectionTimeoutSeconds uint `json:"connectionTimeoutSeconds"`
}

// DumpStateConfig configures the endpoint to dump all internal state
type DumpStateConfig struct {
	// Port is the port to serve on
	Port uint16 `json:"port"`
	// TimeoutSeconds gives the maximum duration, in seconds, that we allow for a request to dump
	// internal state.
	TimeoutSeconds uint `json:"timeoutSeconds"`
}

// ScalingConfig defines the scheduling we use for scaling up and down
type ScalingConfig struct {
	// RequestTimeoutSeconds gives the timeout duration, in seconds, for VM patch requests
	RequestTimeoutSeconds uint `json:"requestTimeoutSeconds"`
	// DefaultConfig gives the default scaling config, to be used if there is no configuration
	// supplied with the "autoscaling.neon.tech/config" annotation.
	DefaultConfig api.ScalingConfig `json:"defaultConfig"`
}

type InformantConfig struct {
	// ServerPort is the port that the VM informant serves from
	ServerPort uint16 `json:"serverPort"`

	// CallbackPort is the port that the agent listens on for informant -> agent requests
	CallbackPort int `json:"callbackPort"`

	// RetryServerMinWaitSeconds gives the minimum duration, in seconds, that we must wait between the
	// start of one InformantServer and the next
	//
	// This "minimum wait" is only used when thethe
	RetryServerMinWaitSeconds uint `json:"retryServerMinWaitSeconds"`
	// RetryServerNormalWaitSeconds gives the typical duration, in seconds, that we wait between an
	// InformantServer failing and our retry.
	RetryServerNormalWaitSeconds uint `json:"retryServerNormalWaitSeconds"`
	// RegisterRetrySeconds gives the duration, in seconds, to wait between retrying a failed
	// register request.
	RegisterRetrySeconds uint `json:"registerRetrySeconds"`

	// RequestTimeoutSeconds gives the timeout for any individual request to the informant, except
	// for those with separately-defined values below.
	RequestTimeoutSeconds uint `json:"requestTimeoutSeconds"`
	// RegisterTimeoutSeconds gives the timeout duration, in seconds, for a register request.
	//
	// This is a separate field from RequestTimeoutSeconds because registering may require that the
	// informant suspend a previous agent, which could take longer.
	RegisterTimeoutSeconds uint `json:"registerTimeoutSeconds"`
	// DownscaleTimeoutSeconds gives the timeout duration, in seconds, for a downscale request.
	//
	// This is a separate field from RequestTimeoutSeconds it's possible that downscaling may
	// require some non-trivial work that we want to allow to complete.
	DownscaleTimeoutSeconds uint `json:"downscaleTimeoutSeconds"`

	// UnhealthyAfterSilenceDurationSeconds gives the duration, in seconds, after which failing to
	// receive a successful request from the informant indicates that it is probably unhealthy.
	UnhealthyAfterSilenceDurationSeconds uint `json:"unhealthyAfterSilenceDurationSeconds"`
	// UnhealthyStartupGracePeriodSeconds gives the duration, in seconds, after which we will no
	// longer excuse total VM informant failures - i.e. when unhealthyAfterSilenceDurationSeconds
	// kicks in.
	UnhealthyStartupGracePeriodSeconds uint `json:"unhealthyStartupGracePeriodSeconds"`
}

// MetricsConfig defines a few parameters for metrics requests to the VM
type MetricsConfig struct {
	// LoadMetricPrefix is the prefix at the beginning of the load metrics that we use. For
	// node_exporter, this is "node_", and for vector it's "host_"
	LoadMetricPrefix string `json:"loadMetricPrefix"`
	// RequestTimeoutSeconds gives the timeout duration, in seconds, for metrics requests
	RequestTimeoutSeconds uint `json:"requestTimeoutSeconds"`
	// SecondsBetweenRequests sets the number of seconds to wait between metrics requests
	SecondsBetweenRequests uint `json:"secondsBetweenRequests"`
}

// SchedulerConfig defines a few parameters for scheduler requests
type SchedulerConfig struct {
	// SchedulerName is the name of the scheduler we're expecting to communicate with.
	//
	// Any VMs that don't have a matching Spec.SchedulerName will not be autoscaled.
	SchedulerName string `json:"schedulerName"`
	// RequestTimeoutSeconds gives the timeout duration, in seconds, for requests to the scheduler
	//
	// If zero, requests will have no timeout.
	RequestTimeoutSeconds uint `json:"requestTimeoutSeconds"`
	// RequestPort defines the port to access the scheduler's ✨special✨ API with
	RequestPort uint16 `json:"requestPort"`
}

func ReadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Error opening config file %q: %w", path, err)
	}

	defer file.Close()
	var config Config
	jsonDecoder := json.NewDecoder(file)
	jsonDecoder.DisallowUnknownFields()
	if err = jsonDecoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("Error decoding JSON config in %q: %w", path, err)
	}

	if err = config.validate(); err != nil {
		return nil, fmt.Errorf("Invalid config: %w", err)
	}

	return &config, nil
}

func (c *Config) validate() error {
	const (
		emptyTmpl = "field %q cannot be empty"
		zeroTmpl  = "field %q cannot be zero"
	)

	return ers.Join(
		ers.Whenf(c.Billing != nil && c.Billing.ActiveTimeMetricName == "", emptyTmpl, ".billing.activeTimeMetricName"),
		ers.Whenf(c.Billing != nil && c.Billing.CPUMetricName == "", emptyTmpl, ".billing.cpuMetricName"),
		ers.Whenf(c.Billing != nil && c.Billing.CollectEverySeconds == 0, zeroTmpl, ".billing.collectEverySeconds"),
		ers.Whenf(c.Billing != nil && c.Billing.AccumulateEverySeconds == 0, zeroTmpl, ".billing.accumulateEverySeconds"),
		ers.Whenf(c.Billing != nil && c.Billing.PushEverySeconds == 0, zeroTmpl, ".billing.pushEverySeconds"),
		ers.Whenf(c.Billing != nil && c.Billing.PushRequestTimeoutSeconds == 0, zeroTmpl, ".billing.pushRequestTimeoutSeconds"),
		ers.Whenf(c.Billing != nil && c.Billing.MaxBatchSize == 0, zeroTmpl, ".billing.maxBatchSize"),
		ers.Whenf(c.Billing != nil && c.Billing.URL == "", emptyTmpl, ".billing.url"),
		ers.Whenf(c.DumpState != nil && c.DumpState.Port == 0, zeroTmpl, ".dumpState.port"),
		ers.Whenf(c.DumpState != nil && c.DumpState.TimeoutSeconds == 0, zeroTmpl, ".dumpState.timeoutSeconds"),
		ers.Whenf(c.Informant.DownscaleTimeoutSeconds == 0, zeroTmpl, ".informant.downscaleTimeoutSeconds"),
		ers.Whenf(c.Informant.RegisterRetrySeconds == 0, zeroTmpl, ".informant.registerRetrySeconds"),
		ers.Whenf(c.Informant.RegisterTimeoutSeconds == 0, zeroTmpl, ".informant.registerTimeoutSeconds"),
		ers.Whenf(c.Informant.RequestTimeoutSeconds == 0, zeroTmpl, ".informant.requestTimeoutSeconds"),
		ers.Whenf(c.Informant.RetryServerMinWaitSeconds == 0, zeroTmpl, ".informant.retryServerMinWaitSeconds"),
		ers.Whenf(c.Informant.RetryServerNormalWaitSeconds == 0, zeroTmpl, ".informant.retryServerNormalWaitSeconds"),
		ers.Whenf(c.Informant.ServerPort == 0, zeroTmpl, ".informant.serverPort"),
		ers.Whenf(c.Informant.CallbackPort == 0, zeroTmpl, ".informant.callbackPort"),
		ers.Whenf(c.Informant.UnhealthyAfterSilenceDurationSeconds == 0, zeroTmpl, ".informant.unhealthyAfterSilenceDurationSeconds"),
		ers.Whenf(c.Informant.UnhealthyStartupGracePeriodSeconds == 0, zeroTmpl, ".informant.unhealthyStartupGracePeriodSeconds"),
		ers.Whenf(c.Metrics.LoadMetricPrefix == "", emptyTmpl, ".metrics.loadMetricPrefix"),
		ers.Whenf(c.Metrics.SecondsBetweenRequests == 0, zeroTmpl, ".metrics.secondsBetweenRequests"),
		ers.Whenf(c.Scaling.RequestTimeoutSeconds == 0, zeroTmpl, ".scaling.requestTimeoutSeconds"),
		ers.Whenf(c.Monitor.ResponseTimeoutSeconds == 0, zeroTmpl, ".monitor.responseTimeoutSeconds"),
		ers.Whenf(c.Monitor.ConnectionTimeoutSeconds == 0, zeroTmpl, ".monitor.connectionTimeoutSeconds"),
		// add all errors if there are any: https://github.com/neondatabase/autoscaling/pull/195#discussion_r1170893494
		c.Scaling.DefaultConfig.Validate(),
		ers.Whenf(c.Scheduler.RequestPort == 0, zeroTmpl, ".scheduler.requestPort"),
		ers.Whenf(c.Scheduler.RequestTimeoutSeconds == 0, zeroTmpl, ".scheduler.requestTimeoutSeconds"),
		ers.Whenf(c.Scheduler.SchedulerName == "", emptyTmpl, ".scheduler.schedulerName"),
	)
}
