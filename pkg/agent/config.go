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
	RefreshStateIntervalSeconds uint `json:"refereshStateIntervalSeconds"`

	Scaling   ScalingConfig    `json:"scaling"`
	Metrics   MetricsConfig    `json:"metrics"`
	Scheduler SchedulerConfig  `json:"scheduler"`
	Monitor   MonitorConfig    `json:"monitor"`
	NeonVM    NeonVMConfig     `json:"neonvm"`
	Billing   billing.Config   `json:"billing"`
	DumpState *DumpStateConfig `json:"dumpState"`
}

type RateThresholdConfig struct {
	IntervalSeconds uint `json:"intervalSeconds"`
	Threshold       uint `json:"threshold"`
}

type MonitorConfig struct {
	ResponseTimeoutSeconds uint `json:"responseTimeoutSeconds"`
	// ConnectionTimeoutSeconds gives how long we may take to connect to the
	// monitor before cancelling.
	ConnectionTimeoutSeconds uint `json:"connectionTimeoutSeconds"`
	// ConnectionRetryMinWaitSeconds gives the minimum amount of time we must wait between attempts
	// to connect to the vm-monitor, regardless of whether they're successful.
	ConnectionRetryMinWaitSeconds uint `json:"connectionRetryMinWaitSeconds"`
	// ServerPort is the port that the dispatcher serves from
	ServerPort uint16 `json:"serverPort"`
	// UnhealthyAfterSilenceDurationSeconds gives the duration, in seconds, after which failing to
	// receive a successful request from the monitor indicates that it is probably unhealthy.
	UnhealthyAfterSilenceDurationSeconds uint `json:"unhealthyAfterSilenceDurationSeconds"`
	// UnhealthyStartupGracePeriodSeconds gives the duration, in seconds, after which we will no
	// longer excuse total VM monitor failures - i.e. when unhealthyAfterSilenceDurationSeconds
	// kicks in.
	UnhealthyStartupGracePeriodSeconds uint `json:"unhealthyStartupGracePeriodSeconds"`
	// MaxHealthCheckSequentialFailuresSeconds gives the duration, in seconds, after which we
	// should restart the connection to the vm-monitor if health checks aren't succeeding.
	MaxHealthCheckSequentialFailuresSeconds uint `json:"maxHealthCheckSequentialFailuresSeconds"`
	// MaxFailedRequestRate defines the maximum rate of failed monitor requests, above which
	// a VM is considered stuck.
	MaxFailedRequestRate RateThresholdConfig `json:"maxFailedRequestRate"`

	// RetryFailedRequestSeconds gives the duration, in seconds, that we must wait before retrying a
	// request that previously failed.
	RetryFailedRequestSeconds uint `json:"retryFailedRequestSeconds"`
	// RetryDeniedDownscaleSeconds gives the duration, in seconds, that we must wait before retrying
	// a downscale request that was previously denied
	RetryDeniedDownscaleSeconds uint `json:"retryDeniedDownscaleSeconds"`
	// RequestedUpscaleValidSeconds gives the duration, in seconds, that requested upscaling should
	// be respected for, before allowing re-downscaling.
	RequestedUpscaleValidSeconds uint `json:"requestedUpscaleValidSeconds"`
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
	// ComputeUnit is the desired ratio between CPU and memory that the autoscaler-agent should
	// uphold when making changes to a VM
	ComputeUnit api.Resources `json:"computeUnit"`
	// DefaultConfig gives the default scaling config, to be used if there is no configuration
	// supplied with the "autoscaling.neon.tech/config" annotation.
	DefaultConfig api.ScalingConfig `json:"defaultConfig"`
}

// MetricsConfig defines a few parameters for metrics requests to the VM
type MetricsConfig struct {
	// Port is the port that VMs are expected to provide metrics on
	Port uint16 `json:"port"`
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
	// RequestAtLeastEverySeconds gives the maximum duration we should go without attempting a
	// request to the scheduler, even if nothing's changed.
	RequestAtLeastEverySeconds uint `json:"requestAtLeastEverySeconds"`
	// RetryFailedRequestSeconds gives the duration, in seconds, that we must wait after a previous
	// failed request before making another one.
	RetryFailedRequestSeconds uint `json:"retryFailedRequestSeconds"`
	// RetryDeniedUpscaleSeconds gives the duration, in seconds, that we must wait before resending
	// a request for resources that were not approved
	RetryDeniedUpscaleSeconds uint `json:"retryDeniedUpscaleSeconds"`
	// RequestPort defines the port to access the scheduler's ✨special✨ API with
	RequestPort uint16 `json:"requestPort"`
	// MaxFailedRequestRate defines the maximum rate of failed scheduler requests, above which
	// a VM is considered stuck.
	MaxFailedRequestRate RateThresholdConfig `json:"maxFailedRequestRate"`
}

// NeonVMConfig defines a few parameters for NeonVM requests
type NeonVMConfig struct {
	// RequestTimeoutSeconds gives the timeout duration, in seconds, for VM patch requests
	RequestTimeoutSeconds uint `json:"requestTimeoutSeconds"`
	// RetryFailedRequestSeconds gives the duration, in seconds, that we must wait after a previous
	// failed request before making another one.
	RetryFailedRequestSeconds uint `json:"retryFailedRequestSeconds"`

	// MaxFailedRequestRate defines the maximum rate of failed NeonVM requests, above which
	// a VM is considered stuck.
	MaxFailedRequestRate RateThresholdConfig `json:"maxFailedRequestRate"`
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

	validateBaseBillingConfig := func(cfg *billing.BaseClientConfig, key string) {
		erc.Whenf(ec, cfg.PushEverySeconds == 0, zeroTmpl, fmt.Sprintf("%s.pushEverySeconds", key))
		erc.Whenf(ec, cfg.PushRequestTimeoutSeconds == 0, zeroTmpl, fmt.Sprintf("%s.pushRequestTimeoutSeconds", key))
		erc.Whenf(ec, cfg.MaxBatchSize == 0, zeroTmpl, fmt.Sprintf("%s.maxBatchSize", key))
	}

	erc.Whenf(ec, c.Billing.ActiveTimeMetricName == "", emptyTmpl, ".billing.activeTimeMetricName")
	erc.Whenf(ec, c.Billing.CPUMetricName == "", emptyTmpl, ".billing.cpuMetricName")
	erc.Whenf(ec, c.Billing.CollectEverySeconds == 0, zeroTmpl, ".billing.collectEverySeconds")
	erc.Whenf(ec, c.Billing.AccumulateEverySeconds == 0, zeroTmpl, ".billing.accumulateEverySeconds")
	if c.Billing.Clients.HTTP != nil {
		validateBaseBillingConfig(&c.Billing.Clients.HTTP.BaseClientConfig, ".billing.clients.http")
		erc.Whenf(ec, c.Billing.Clients.HTTP.URL == "", emptyTmpl, ".billing.clients.http.url")
	}
	if c.Billing.Clients.S3 != nil {
		validateBaseBillingConfig(&c.Billing.Clients.S3.BaseClientConfig, "billing.clients.s3")
		erc.Whenf(ec, c.Billing.Clients.S3.Bucket == "", emptyTmpl, ".billing.clients.s3.bucket")
		erc.Whenf(ec, c.Billing.Clients.S3.Region == "", emptyTmpl, ".billing.clients.s3.region")
		erc.Whenf(ec, c.Billing.Clients.S3.PrefixInBucket == "", emptyTmpl, ".billing.clients.s3.prefixInBucket")
	}
	erc.Whenf(ec, c.DumpState != nil && c.DumpState.Port == 0, zeroTmpl, ".dumpState.port")
	erc.Whenf(ec, c.DumpState != nil && c.DumpState.TimeoutSeconds == 0, zeroTmpl, ".dumpState.timeoutSeconds")
	erc.Whenf(ec, c.Metrics.Port == 0, zeroTmpl, ".metrics.port")
	erc.Whenf(ec, c.Metrics.LoadMetricPrefix == "", emptyTmpl, ".metrics.loadMetricPrefix")
	erc.Whenf(ec, c.Metrics.RequestTimeoutSeconds == 0, zeroTmpl, ".metrics.requestTimeoutSeconds")
	erc.Whenf(ec, c.Metrics.SecondsBetweenRequests == 0, zeroTmpl, ".metrics.secondsBetweenRequests")
	erc.Whenf(ec, c.Scaling.ComputeUnit.VCPU == 0, zeroTmpl, ".scaling.computeUnit.vCPUs")
	erc.Whenf(ec, c.Scaling.ComputeUnit.Mem == 0, zeroTmpl, ".scaling.computeUnit.mem")
	erc.Whenf(ec, c.NeonVM.RequestTimeoutSeconds == 0, zeroTmpl, ".scaling.requestTimeoutSeconds")
	erc.Whenf(ec, c.NeonVM.RetryFailedRequestSeconds == 0, zeroTmpl, ".scaling.retryFailedRequestSeconds")
	erc.Whenf(ec, c.NeonVM.MaxFailedRequestRate.IntervalSeconds == 0, zeroTmpl, ".neonvm.maxFailedRequestRate.intervalSeconds")
	erc.Whenf(ec, c.Monitor.ResponseTimeoutSeconds == 0, zeroTmpl, ".monitor.responseTimeoutSeconds")
	erc.Whenf(ec, c.Monitor.ConnectionTimeoutSeconds == 0, zeroTmpl, ".monitor.connectionTimeoutSeconds")
	erc.Whenf(ec, c.Monitor.ConnectionRetryMinWaitSeconds == 0, zeroTmpl, ".monitor.connectionRetryMinWaitSeconds")
	erc.Whenf(ec, c.Monitor.ServerPort == 0, zeroTmpl, ".monitor.serverPort")
	erc.Whenf(ec, c.Monitor.UnhealthyAfterSilenceDurationSeconds == 0, zeroTmpl, ".monitor.unhealthyAfterSilenceDurationSeconds")
	erc.Whenf(ec, c.Monitor.UnhealthyStartupGracePeriodSeconds == 0, zeroTmpl, ".monitor.unhealthyStartupGracePeriodSeconds")
	erc.Whenf(ec, c.Monitor.MaxHealthCheckSequentialFailuresSeconds == 0, zeroTmpl, ".monitor.maxHealthCheckSequentialFailuresSeconds")
	erc.Whenf(ec, c.Monitor.RetryFailedRequestSeconds == 0, zeroTmpl, ".monitor.retryFailedRequestSeconds")
	erc.Whenf(ec, c.Monitor.RetryDeniedDownscaleSeconds == 0, zeroTmpl, ".monitor.retryDeniedDownscaleSeconds")
	erc.Whenf(ec, c.Monitor.RequestedUpscaleValidSeconds == 0, zeroTmpl, ".monitor.requestedUpscaleValidSeconds")
	erc.Whenf(ec, c.Monitor.MaxFailedRequestRate.IntervalSeconds == 0, zeroTmpl, ".monitor.maxFailedRequestRate.intervalSeconds")
	// add all errors if there are any: https://github.com/neondatabase/autoscaling/pull/195#discussion_r1170893494
	ec.Add(c.Scaling.DefaultConfig.ValidateDefaults())
	erc.Whenf(ec, c.Scheduler.RequestPort == 0, zeroTmpl, ".scheduler.requestPort")
	erc.Whenf(ec, c.Scheduler.RequestTimeoutSeconds == 0, zeroTmpl, ".scheduler.requestTimeoutSeconds")
	erc.Whenf(ec, c.Scheduler.RequestAtLeastEverySeconds == 0, zeroTmpl, ".scheduler.requestAtLeastEverySeconds")
	erc.Whenf(ec, c.Scheduler.RetryFailedRequestSeconds == 0, zeroTmpl, ".scheduler.retryFailedRequestSeconds")
	erc.Whenf(ec, c.Scheduler.RetryDeniedUpscaleSeconds == 0, zeroTmpl, ".scheduler.retryDeniedUpscaleSeconds")
	erc.Whenf(ec, c.Scheduler.SchedulerName == "", emptyTmpl, ".scheduler.schedulerName")
	erc.Whenf(ec, c.Scheduler.MaxFailedRequestRate.IntervalSeconds == 0, zeroTmpl, ".monitor.maxFailedRequestRate.intervalSeconds")

	return ec.Resolve()
}
