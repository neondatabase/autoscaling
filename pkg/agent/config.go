package agent

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	Scaling   ScalingConfig   `json:"scaling"`
	Informant InformantConfig `json:"informant"`
	Metrics   MetricsConfig   `json:"metrics"`
	Scheduler SchedulerConfig `json:"scheduler"`
	Billing   *BillingConfig  `json:"billing,omitempty"`
}

// ScalingConfig defines the scheduling we use for scaling up and down
type ScalingConfig struct {
	// RequestTimeoutSeconds gives the timeout duration, in seconds, for VM patch requests
	RequestTimeoutSeconds uint `json:"requestTimeoutSeconds"`
}

type InformantConfig struct {
	// ServerPort is the port that the VM informant serves from
	ServerPort uint16 `json:"serverPort"`

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
	cannotBeZero := func(fieldPath string) error {
		return fmt.Errorf("Field %s cannot be zero", fieldPath)
	}
	cannotBeEmpty := func(fieldPath string) error {
		return fmt.Errorf("Field %s cannot be empty", fieldPath)
	}

	if c.Scaling.RequestTimeoutSeconds == 0 {
		return cannotBeZero(".scaling.requestTimeoutSeconds")
	} else if c.Informant.ServerPort == 0 {
		return cannotBeZero(".informant.serverPort")
	} else if c.Informant.RetryServerMinWaitSeconds == 0 {
		return cannotBeZero(".informant.retryServerMinWaitSeconds")
	} else if c.Informant.RetryServerNormalWaitSeconds == 0 {
		return cannotBeZero(".informant.retryServerNormalWaitSeconds")
	} else if c.Informant.RegisterRetrySeconds == 0 {
		return cannotBeZero(".informant.registerRetrySeconds")
	} else if c.Informant.RequestTimeoutSeconds == 0 {
		return cannotBeZero(".informant.requestTimeoutSeconds")
	} else if c.Informant.RegisterTimeoutSeconds == 0 {
		return cannotBeZero(".informant.registerTimeoutSeconds")
	} else if c.Informant.DownscaleTimeoutSeconds == 0 {
		return cannotBeZero(".informant.downscaleTimeoutSeconds")
	} else if c.Metrics.SecondsBetweenRequests == 0 {
		return cannotBeZero(".metrics.secondsBetweenRequests")
	} else if c.Metrics.LoadMetricPrefix == "" {
		return cannotBeEmpty(".metrics.loadMetricPrefix")
	} else if c.Scheduler.SchedulerName == "" {
		return cannotBeEmpty(".scheduler.schedulerName")
	} else if c.Scheduler.RequestTimeoutSeconds == 0 {
		return cannotBeZero(".scheduler.requestTimeoutSeconds")
	} else if c.Scheduler.RequestPort == 0 {
		return cannotBeZero(".scheduler.requestPort")
	} else if c.Billing != nil && c.Billing.URL == "" {
		return cannotBeEmpty(".billing.url")
	} else if c.Billing != nil && c.Billing.CPUMetricName == "" {
		return cannotBeEmpty(".billing.cpuMetricName")
	} else if c.Billing != nil && c.Billing.CollectEverySeconds == 0 {
		return cannotBeZero(".billing.collectEverySeconds")
	} else if c.Billing != nil && c.Billing.PushEverySeconds == 0 {
		return cannotBeZero(".billing.pushEverySeconds")
	} else if c.Billing != nil && c.Billing.PushTimeoutSeconds == 0 {
		return cannotBeZero(".billing.pushTimeoutSeconds")
	}

	return nil
}
