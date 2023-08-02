package agent

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/tychoish/fun/erc"

	"github.com/neondatabase/autoscaling/pkg/agent/billing"
	"github.com/neondatabase/autoscaling/pkg/api"
)

type Config struct {
	Scaling   ScalingConfig    `json:"scaling"`
	Informant InformantConfig  `json:"informant"`
	Metrics   MetricsConfig    `json:"metrics"`
	Scheduler SchedulerConfig  `json:"scheduler"`
	Billing   *billing.Config  `json:"billing,omitempty"`
	DumpState *DumpStateConfig `json:"dumpState"`
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

	// Port that the agent listens on for informant -> agent requests
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
	ec := &erc.Collector{}

	const (
		emptyTmpl = "field %q cannot be empty"
		zeroTmpl  = "field %q cannot be zero"
	)

	erc.Whenf(ec, c.Billing != nil && c.Billing.ActiveTimeMetricName == "", emptyTmpl, ".billing.activeTimeMetricName")
	erc.Whenf(ec, c.Billing != nil && c.Billing.CPUMetricName == "", emptyTmpl, ".billing.cpuMetricName")
	erc.Whenf(ec, c.Billing != nil && c.Billing.CollectEverySeconds == 0, zeroTmpl, ".billing.collectEverySeconds")
	erc.Whenf(ec, c.Billing != nil && c.Billing.AccumulateEverySeconds == 0, zeroTmpl, ".billing.accumulateEverySeconds")
	erc.Whenf(ec, c.Billing != nil && c.Billing.PushEverySeconds == 0, zeroTmpl, ".billing.pushEverySeconds")
	erc.Whenf(ec, c.Billing != nil && c.Billing.PushRequestTimeoutSeconds == 0, zeroTmpl, ".billing.pushRequestTimeoutSeconds")
	erc.Whenf(ec, c.Billing != nil && c.Billing.MaxBatchSize == 0, zeroTmpl, ".billing.maxBatchSize")
	erc.Whenf(ec, c.Billing != nil && c.Billing.URL == "", emptyTmpl, ".billing.url")
	erc.Whenf(ec, c.DumpState != nil && c.DumpState.Port == 0, zeroTmpl, ".dumpState.port")
	erc.Whenf(ec, c.DumpState != nil && c.DumpState.TimeoutSeconds == 0, zeroTmpl, ".dumpState.timeoutSeconds")
	erc.Whenf(ec, c.Informant.DownscaleTimeoutSeconds == 0, zeroTmpl, ".informant.downscaleTimeoutSeconds")
	erc.Whenf(ec, c.Informant.RegisterRetrySeconds == 0, zeroTmpl, ".informant.registerRetrySeconds")
	erc.Whenf(ec, c.Informant.RegisterTimeoutSeconds == 0, zeroTmpl, ".informant.registerTimeoutSeconds")
	erc.Whenf(ec, c.Informant.RequestTimeoutSeconds == 0, zeroTmpl, ".informant.requestTimeoutSeconds")
	erc.Whenf(ec, c.Informant.RetryServerMinWaitSeconds == 0, zeroTmpl, ".informant.retryServerMinWaitSeconds")
	erc.Whenf(ec, c.Informant.RetryServerNormalWaitSeconds == 0, zeroTmpl, ".informant.retryServerNormalWaitSeconds")
	erc.Whenf(ec, c.Informant.ServerPort == 0, zeroTmpl, ".informant.serverPort")
	erc.Whenf(ec, c.Informant.CallbackPort == 0, zeroTmpl, ".informant.callbackPort")
	erc.Whenf(ec, c.Informant.UnhealthyAfterSilenceDurationSeconds == 0, zeroTmpl, ".informant.unhealthyAfterSilenceDurationSeconds")
	erc.Whenf(ec, c.Informant.UnhealthyStartupGracePeriodSeconds == 0, zeroTmpl, ".informant.unhealthyStartupGracePeriodSeconds")
	erc.Whenf(ec, c.Metrics.LoadMetricPrefix == "", emptyTmpl, ".metrics.loadMetricPrefix")
	erc.Whenf(ec, c.Metrics.SecondsBetweenRequests == 0, zeroTmpl, ".metrics.secondsBetweenRequests")
	erc.Whenf(ec, c.Scaling.RequestTimeoutSeconds == 0, zeroTmpl, ".scaling.requestTimeoutSeconds")
	// add all errors if there are any: https://github.com/neondatabase/autoscaling/pull/195#discussion_r1170893494
	ec.Add(c.Scaling.DefaultConfig.Validate())
	erc.Whenf(ec, c.Scheduler.RequestPort == 0, zeroTmpl, ".scheduler.requestPort")
	erc.Whenf(ec, c.Scheduler.RequestTimeoutSeconds == 0, zeroTmpl, ".scheduler.requestTimeoutSeconds")
	erc.Whenf(ec, c.Scheduler.SchedulerName == "", emptyTmpl, ".scheduler.schedulerName")

	return ec.Resolve()
}
