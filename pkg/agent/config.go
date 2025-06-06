package agent

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/tychoish/fun/erc"

	"github.com/neondatabase/autoscaling/pkg/agent/billing"
	"github.com/neondatabase/autoscaling/pkg/agent/scalingevents"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/reporting"
)

type Config struct {
	RefreshStateIntervalSeconds uint `json:"refereshStateIntervalSeconds"`

	Billing       billing.Config       `json:"billing"`
	ScalingEvents scalingevents.Config `json:"scalingEvents"`

	Scaling   ScalingConfig    `json:"scaling"`
	Metrics   MetricsConfig    `json:"metrics"`
	Scheduler SchedulerConfig  `json:"scheduler"`
	Monitor   MonitorConfig    `json:"monitor"`
	NeonVM    NeonVMConfig     `json:"neonvm"`
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
	System MetricsSourceConfig `json:"system"`
	LFC    MetricsSourceConfig `json:"lfc"`
}

type MetricsSourceConfig struct {
	// Port is the port that VMs are expected to provide the metrics on
	//
	// For system metrics, vm-builder installs vector (from vector.dev) to expose them on port 9100.
	Port uint16 `json:"port"`
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

	validateBaseReportingConfig := func(cfg *reporting.BaseClientConfig, key string) {
		erc.Whenf(ec, cfg.PushEverySeconds == 0, zeroTmpl, fmt.Sprintf("%s.pushEverySeconds", key))
		erc.Whenf(ec, cfg.PushRequestTimeoutSeconds == 0, zeroTmpl, fmt.Sprintf("%s.pushRequestTimeoutSeconds", key))
		erc.Whenf(ec, cfg.MaxBatchSize == 0, zeroTmpl, fmt.Sprintf("%s.maxBatchSize", key))
	}
	validateS3ReportingConfig := func(cfg *reporting.S3ClientConfig, key string) {
		erc.Whenf(ec, cfg.Bucket == "", emptyTmpl, fmt.Sprintf(".%s.bucket", key))
		erc.Whenf(ec, cfg.Region == "", emptyTmpl, fmt.Sprintf(".%s.region", key))
	}
	validateAzureBlobReportingConfig := func(cfg *reporting.AzureBlobStorageClientConfig, key string) {
		erc.Whenf(ec, cfg.Endpoint == "", emptyTmpl, fmt.Sprintf(".%s.endpoint", key))
		erc.Whenf(ec, cfg.Container == "", emptyTmpl, fmt.Sprintf("%s.container", key))
	}

	erc.Whenf(ec, c.Billing.ActiveTimeMetricName == "", emptyTmpl, ".billing.activeTimeMetricName")
	erc.Whenf(ec, c.Billing.CPUMetricName == "", emptyTmpl, ".billing.cpuMetricName")
	erc.Whenf(ec, c.Billing.CollectEverySeconds == 0, zeroTmpl, ".billing.collectEverySeconds")
	erc.Whenf(ec, c.Billing.AccumulateEverySeconds == 0, zeroTmpl, ".billing.accumulateEverySeconds")
	if c.Billing.Clients.AzureBlob != nil {
		validateBaseReportingConfig(&c.Billing.Clients.AzureBlob.BaseClientConfig, ".billing.clients.azureBlob")
		validateAzureBlobReportingConfig(&c.Billing.Clients.AzureBlob.AzureBlobStorageClientConfig, ".billing.clients.azureBlob")
		erc.Whenf(ec, c.Billing.Clients.AzureBlob.PrefixInContainer == "", emptyTmpl, ".billing.clients.azureBlob.prefixInContainer")
	}
	if c.Billing.Clients.HTTP != nil {
		validateBaseReportingConfig(&c.Billing.Clients.HTTP.BaseClientConfig, ".billing.clients.http")
		erc.Whenf(ec, c.Billing.Clients.HTTP.URL == "", emptyTmpl, ".billing.clients.http.url")
	}
	if c.Billing.Clients.S3 != nil {
		validateBaseReportingConfig(&c.Billing.Clients.S3.BaseClientConfig, "billing.clients.s3")
		validateS3ReportingConfig(&c.Billing.Clients.S3.S3ClientConfig, ".billing.clients.s3")
	}

	erc.Whenf(ec, c.ScalingEvents.CUMultiplier == 0, zeroTmpl, ".scalingEvents.cuMultiplier")
	erc.Whenf(ec, c.ScalingEvents.RereportThreshold == 0, zeroTmpl, ".scalingEvents.rereportThreshold")
	erc.Whenf(ec, c.ScalingEvents.RegionName == "", emptyTmpl, ".scalingEvents.regionName")
	if c.ScalingEvents.Clients.AzureBlob != nil {
		validateBaseReportingConfig(&c.ScalingEvents.Clients.AzureBlob.BaseClientConfig, ".scalingEvents.clients.azureBlob")
		validateAzureBlobReportingConfig(&c.ScalingEvents.Clients.AzureBlob.AzureBlobStorageClientConfig, ".scalingEvents.clients.azureBlob")
		erc.Whenf(ec, c.ScalingEvents.Clients.AzureBlob.PrefixInContainer == "", emptyTmpl, ".scalingEvents.clients.azureBlob.prefixInContainer")
	}
	if c.ScalingEvents.Clients.S3 != nil {
		validateBaseReportingConfig(&c.ScalingEvents.Clients.S3.BaseClientConfig, "scalingEvents.clients.s3")
		validateS3ReportingConfig(&c.ScalingEvents.Clients.S3.S3ClientConfig, ".scalingEvents.clients.s3")
		erc.Whenf(ec, c.ScalingEvents.Clients.S3.PrefixInBucket == "", emptyTmpl, ".scalingEvents.clients.s3.prefixInBucket")
	}

	erc.Whenf(ec, c.DumpState != nil && c.DumpState.Port == 0, zeroTmpl, ".dumpState.port")
	erc.Whenf(ec, c.DumpState != nil && c.DumpState.TimeoutSeconds == 0, zeroTmpl, ".dumpState.timeoutSeconds")

	validateMetricsConfig := func(cfg MetricsSourceConfig, key string) {
		erc.Whenf(ec, cfg.Port == 0, zeroTmpl, fmt.Sprintf(".metrics.%s.port", key))
		erc.Whenf(ec, cfg.RequestTimeoutSeconds == 0, zeroTmpl, fmt.Sprintf(".metrics.%s.requestTimeoutSeconds", key))
		erc.Whenf(ec, cfg.SecondsBetweenRequests == 0, zeroTmpl, fmt.Sprintf(".metrics.%s.secondsBetweenRequests", key))
	}
	validateMetricsConfig(c.Metrics.System, "system")
	validateMetricsConfig(c.Metrics.LFC, "lfc")
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
