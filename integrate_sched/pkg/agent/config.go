package agent

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	Scaling   ScalingConfig   `json:"scaling"`
	Metrics   MetricsConfig   `json:"metrics"`
	Scheduler SchedulerConfig `json:"scheduler"`
}

// ScalingConfig defines the scheduling we use for scaling up and down
type ScalingConfig struct {
	// RequestTimeoutSeconds gives the timeout duration, in seconds, for VM patch requests
	RequestTimeoutSeconds uint `json:"requestTimeoutSeconds"`
	CPU struct {
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

// MetricsConfig defines a few parameters for metrics requests to the VM
type MetricsConfig struct {
	// RequestTimeoutSeconds gives the timeout duration, in seconds, for metrics requests
	RequestTimeoutSeconds uint `json:"requestTimeoutSeconds"`
	// SecondsBetweenRequests sets the number of seconds to wait between metrics requests
	SecondsBetweenRequests uint `json:"secondsBetweenRequests"`
	// InitialDelaySeconds gives the amount to delay at the start before making any requests, to
	// give the VM a chance to start up
	//
	// Setting this value too small doesn't have any permanent effects; we'll just retry after
	// SecondsBetweenRequests has passed.
	InitialDelaySeconds uint `json:"initialDelaySeconds"`
}

// SchedulerConfig defines a few parameters for scheduler requests
type SchedulerConfig struct {
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
		return nil, fmt.Errorf("Error opening config file %q: %s", path, err)
	}

	defer file.Close()
	var config Config
	jsonDecoder := json.NewDecoder(file)
	jsonDecoder.DisallowUnknownFields()
	if err = jsonDecoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("Error decoding JSON config in %q: %s", path, err)
	}

	if err = config.validate(); err != nil {
		return nil, fmt.Errorf("Invaid config: %s", err)
	}

	return &config, nil
}

func (c *Config) validate() error {
	cannotBeZero := func(fieldPath string) error {
		return fmt.Errorf("Field %s cannot be zero", fieldPath)
	}

	if c.Scaling.RequestTimeoutSeconds == 0 {
		return cannotBeZero(".scaling.requestTimeoutSeconds")
	} else if c.Scaling.CPU.DoubleRatio == 0 {
		return cannotBeZero(".scaling.cpu.doubleRatio")
	} else if c.Scaling.CPU.HalveRatio == 0 {
		return cannotBeZero(".scaling.cpu.halveRatio")
	} else if c.Metrics.RequestTimeoutSeconds == 0 {
		return cannotBeZero(".metrics.requestTimeoutSeconds")
	} else if c.Metrics.SecondsBetweenRequests == 0 {
		return cannotBeZero(".metrics.secondsBetweenRequests")
	} else if c.Metrics.InitialDelaySeconds == 0 {
		return cannotBeZero(".metrics.initialDelaySeconds")
	} else if c.Scheduler.RequestPort == 0 {
		return cannotBeZero(".scheduler.requestPort")
	}

	return nil
}
