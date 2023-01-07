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
}

// ScalingConfig defines the scheduling we use for scaling up and down
type ScalingConfig struct {
	// RequestTimeoutSeconds gives the timeout duration, in seconds, for VM patch requests
	RequestTimeoutSeconds uint `json:"requestTimeoutSeconds"`
	CPU                   struct {
		// DoubleRatio gives the ratio between the VM's load average and number of CPUs above which
		// we'll request double the vCPU count
		//
		// Basically: if LoadAverage / NumCPUs > DoubleRatio, we set NumCPUs = NumCPUs * 2.
		//
		// TODO: Currently unused
		DoubleRatio float32 `json:"doubleRatio"`
		// HalveRatio gives the ratio between the VM's load average and the number of CPUs below
		// which we'll halve the number of vCPUs we're using
		//
		// Basically: if LoadAverage / NumCPUs < HalveRatio, we set NumCPUs = NumCPUs / 2.
		//
		// TODO: Currently unused
		HalveRatio float32 `json:"halveRatio"`
	} `json:"cpu"`
}

type InformantConfig struct {
	// ServerPort is the port that the VM informant serves from
	ServerPort uint16 `json:"serverPort"`
	// MaxStartupSeconds is the total maximum amount of time we're allowed to wait for the VM
	// informant to come online
	MaxStartupSeconds uint `json:"maxStartupSeconds"`
	// RequestTimeoutSeconds gives the timeout for any individual request to the VM informant
	RequestTimeoutSeconds uint `json:"requestTimeoutSeconds"`
	// RetryRegisterAfterSeconds gives the amount of time we should wait before retrying a register
	// request
	RetryRegisterAfterSeconds uint `json:"retryRegisterAfterSeconds"`
	// RetryServerDelaySeconds gives the initial delay for for recreating a server that's exited
	RetryServerAfterSeconds uint `json:"retryServerAfterSeconds"`
	// RetryFailedServerDelaySeconds gives the amount of time to wait between server creation
	// retries, after it has already failed once
	RetryFailedServerDelaySeconds uint `json:"retryFailedServerDelaySeconds"`
}

// MetricsConfig defines a few parameters for metrics requests to the VM
type MetricsConfig struct {
	// LoadMetricPrefix is the prefix at at the beginning of the load metrics that we use. For
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
	} else if c.Scaling.CPU.DoubleRatio == 0 {
		return cannotBeZero(".scaling.cpu.doubleRatio")
	} else if c.Scaling.CPU.HalveRatio == 0 {
		return cannotBeZero(".scaling.cpu.halveRatio")
	} else if c.Informant.ServerPort == 0 {
		return cannotBeZero(".informant.serverPort")
	} else if c.Informant.MaxStartupSeconds == 0 {
		return cannotBeZero(".informant.maxStartupSeconds")
	} else if c.Informant.RequestTimeoutSeconds == 0 {
		return cannotBeZero(".informant.requestTimeoutSeconds")
	} else if c.Informant.RetryRegisterAfterSeconds == 0 {
		return cannotBeZero(".informant.retryRegisterAfterSeconds")
	} else if c.Metrics.RequestTimeoutSeconds == 0 {
		return cannotBeZero(".metrics.requestTimeoutSeconds")
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
	}

	return nil
}
