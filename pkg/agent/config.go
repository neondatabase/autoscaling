package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/tychoish/fun/erc"

	"github.com/neondatabase/autoscaling/pkg/agent/billing"
	"github.com/neondatabase/autoscaling/pkg/agent/scalingevents"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/reporting"
	"github.com/neondatabase/autoscaling/pkg/util/duration"
)

type Config struct {
	RefreshStateInterval duration.Duration `json:"refreshStateInterval"`

	Billing       billing.Config       `json:"billing"`
	ScalingEvents scalingevents.Config `json:"scalingEvents"`

	Scaling   ScalingConfig    `json:"scaling"`
	Metrics   MetricsConfig    `json:"metrics"`
	Scheduler SchedulerConfig  `json:"scheduler"`
	Monitor   MonitorConfig    `json:"monitor"`
	NeonVM    NeonVMConfig     `json:"neonvm"`
	DumpState *DumpStateConfig `json:"dumpState"`
	
	DeadlockCheckerDelay duration.Duration `json:"deadlockCheckerDelay"`
	DeadlockCheckerTimeout duration.Duration `json:"deadlockCheckerTimeout"`
}

type RateThresholdConfig struct {
	IntervalSeconds uint `json:"intervalSeconds"`
	Threshold       uint `json:"threshold"`
}

type MonitorConfig struct {
	ResponseTimeout duration.Duration `json:"responseTimeout"`
	// monitor before cancelling.
	ConnectionTimeout duration.Duration `json:"connectionTimeout"`
	// to connect to the vm-monitor, regardless of whether they're successful.
	ConnectionRetryMinWait duration.Duration `json:"connectionRetryMinWait"`
	// ServerPort is the port that the dispatcher serves from
	ServerPort uint16 `json:"serverPort"`
	// receive a successful request from the monitor indicates that it is probably unhealthy.
	UnhealthyAfterSilenceDuration duration.Duration `json:"unhealthyAfterSilenceDuration"`
	// kicks in.
	UnhealthyStartupGracePeriod duration.Duration `json:"unhealthyStartupGracePeriod"`
	// should restart the connection to the vm-monitor if health checks aren't succeeding.
	MaxHealthCheckSequentialFailures duration.Duration `json:"maxHealthCheckSequentialFailures"`
	// MaxFailedRequestRate defines the maximum rate of failed monitor requests, above which
	// a VM is considered stuck.
	MaxFailedRequestRate RateThresholdConfig `json:"maxFailedRequestRate"`

	// request that previously failed.
	RetryFailedRequest duration.Duration `json:"retryFailedRequest"`
	// a downscale request that was previously denied
	RetryDeniedDownscale duration.Duration `json:"retryDeniedDownscale"`
	// be respected for, before allowing re-downscaling.
	RequestedUpscaleValid duration.Duration `json:"requestedUpscaleValid"`
	HealthCheckInterval duration.Duration `json:"healthCheckInterval"`
}

// DumpStateConfig configures the endpoint to dump all internal state
type DumpStateConfig struct {
	// Port is the port to serve on
	Port uint16 `json:"port"`
	// internal state.
	Timeout duration.Duration `json:"timeout"`
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
	RequestTimeout duration.Duration `json:"requestTimeout"`
	RequestInterval duration.Duration `json:"requestInterval"`
}

// SchedulerConfig defines a few parameters for scheduler requests
type SchedulerConfig struct {
	// SchedulerName is the name of the scheduler we're expecting to communicate with.
	//
	// Any VMs that don't have a matching Spec.SchedulerName will not be autoscaled.
	SchedulerName string `json:"schedulerName"`
	//
	// If zero, requests will have no timeout.
	RequestTimeout duration.Duration `json:"requestTimeout"`
	// request to the scheduler, even if nothing's changed.
	RequestAtLeastEvery duration.Duration `json:"requestAtLeastEvery"`
	// failed request before making another one.
	RetryFailedRequest duration.Duration `json:"retryFailedRequest"`
	// a request for resources that were not approved
	RetryDeniedUpscale duration.Duration `json:"retryDeniedUpscale"`
	// RequestPort defines the port to access the scheduler's ✨special✨ API with
	RequestPort uint16 `json:"requestPort"`
	// MaxFailedRequestRate defines the maximum rate of failed scheduler requests, above which
	// a VM is considered stuck.
	MaxFailedRequestRate RateThresholdConfig `json:"maxFailedRequestRate"`
	RetryWatchIntervals duration.RandomDuration `json:"retryWatchIntervals"`
	RetryRelistIntervals duration.RandomDuration `json:"retryRelistIntervals"`
}

// NeonVMConfig defines a few parameters for NeonVM requests
type NeonVMConfig struct {
	RequestTimeout duration.Duration `json:"requestTimeout"`
	// failed request before making another one.
	RetryFailedRequest duration.Duration `json:"retryFailedRequest"`

	// MaxFailedRequestRate defines the maximum rate of failed NeonVM requests, above which
	// a VM is considered stuck.
	MaxFailedRequestRate RateThresholdConfig `json:"maxFailedRequestRate"`
}

func ReadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("error opening config file %q: %w", path, err)
	}

	defer file.Close()
	var config Config
	jsonDecoder := json.NewDecoder(file)
	jsonDecoder.DisallowUnknownFields()
	if err = jsonDecoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("error decoding JSON config in %q: %w", path, err)
	}

	if err = config.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
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
	erc.Whenf(ec, c.DumpState != nil && c.DumpState.Timeout.Seconds == 0, zeroTmpl, ".dumpState.timeout.seconds")

	validateMetricsConfig := func(cfg MetricsSourceConfig, key string) {
		erc.Whenf(ec, cfg.Port == 0, zeroTmpl, fmt.Sprintf(".metrics.%s.port", key))
		erc.Whenf(ec, cfg.RequestTimeout.Seconds == 0, zeroTmpl, fmt.Sprintf(".metrics.%s.requestTimeout.seconds", key))
		erc.Whenf(ec, cfg.RequestInterval.Seconds == 0, zeroTmpl, fmt.Sprintf(".metrics.%s.requestInterval.seconds", key))
	}
	validateMetricsConfig(c.Metrics.System, "system")
	validateMetricsConfig(c.Metrics.LFC, "lfc")
	erc.Whenf(ec, c.Scaling.ComputeUnit.VCPU == 0, zeroTmpl, ".scaling.computeUnit.vCPUs")
	erc.Whenf(ec, c.Scaling.ComputeUnit.Mem == 0, zeroTmpl, ".scaling.computeUnit.mem")
	erc.Whenf(ec, c.NeonVM.RequestTimeout.Seconds == 0, zeroTmpl, ".neonvm.requestTimeout.seconds")
	erc.Whenf(ec, c.NeonVM.RetryFailedRequest.Seconds == 0, zeroTmpl, ".neonvm.retryFailedRequest.seconds")
	erc.Whenf(ec, c.NeonVM.MaxFailedRequestRate.IntervalSeconds == 0, zeroTmpl, ".neonvm.maxFailedRequestRate.intervalSeconds")
	erc.Whenf(ec, c.Monitor.ResponseTimeout.Seconds == 0, zeroTmpl, ".monitor.responseTimeout.seconds")
	erc.Whenf(ec, c.Monitor.ConnectionTimeout.Seconds == 0, zeroTmpl, ".monitor.connectionTimeout.seconds")
	erc.Whenf(ec, c.Monitor.ConnectionRetryMinWait.Seconds == 0, zeroTmpl, ".monitor.connectionRetryMinWait.seconds")
	erc.Whenf(ec, c.Monitor.ServerPort == 0, zeroTmpl, ".monitor.serverPort")
	erc.Whenf(ec, c.Monitor.UnhealthyAfterSilenceDuration.Seconds == 0, zeroTmpl, ".monitor.unhealthyAfterSilenceDuration.seconds")
	erc.Whenf(ec, c.Monitor.UnhealthyStartupGracePeriod.Seconds == 0, zeroTmpl, ".monitor.unhealthyStartupGracePeriod.seconds")
	erc.Whenf(ec, c.Monitor.MaxHealthCheckSequentialFailures.Seconds == 0, zeroTmpl, ".monitor.maxHealthCheckSequentialFailures.seconds")
	erc.Whenf(ec, c.Monitor.RetryFailedRequest.Seconds == 0, zeroTmpl, ".monitor.retryFailedRequest.seconds")
	erc.Whenf(ec, c.Monitor.RetryDeniedDownscale.Seconds == 0, zeroTmpl, ".monitor.retryDeniedDownscale.seconds")
	erc.Whenf(ec, c.Monitor.RequestedUpscaleValid.Seconds == 0, zeroTmpl, ".monitor.requestedUpscaleValid.seconds")
	erc.Whenf(ec, c.Monitor.HealthCheckInterval.Seconds == 0, zeroTmpl, ".monitor.healthCheckInterval.seconds")
	erc.Whenf(ec, c.Monitor.MaxFailedRequestRate.IntervalSeconds == 0, zeroTmpl, ".monitor.maxFailedRequestRate.intervalSeconds")
	// add all errors if there are any: https://github.com/neondatabase/autoscaling/pull/195#discussion_r1170893494
	ec.Add(c.Scaling.DefaultConfig.ValidateDefaults())
	erc.Whenf(ec, c.Scheduler.RequestPort == 0, zeroTmpl, ".scheduler.requestPort")
	erc.Whenf(ec, c.Scheduler.RequestTimeout.Seconds == 0, zeroTmpl, ".scheduler.requestTimeout.seconds")
	erc.Whenf(ec, c.Scheduler.RequestAtLeastEvery.Seconds == 0, zeroTmpl, ".scheduler.requestAtLeastEvery.seconds")
	erc.Whenf(ec, c.Scheduler.RetryFailedRequest.Seconds == 0, zeroTmpl, ".scheduler.retryFailedRequest.seconds")
	erc.Whenf(ec, c.Scheduler.RetryDeniedUpscale.Seconds == 0, zeroTmpl, ".scheduler.retryDeniedUpscale.seconds")
	if err := c.Scheduler.RetryWatchIntervals.Validate(); err != nil {
		ec.Add(fmt.Errorf(".scheduler.retryWatchIntervals: %w", err))
	}
	if err := c.Scheduler.RetryRelistIntervals.Validate(); err != nil {
		ec.Add(fmt.Errorf(".scheduler.retryRelistIntervals: %w", err))
	}
	erc.Whenf(ec, c.Scheduler.SchedulerName == "", emptyTmpl, ".scheduler.schedulerName")
	erc.Whenf(ec, c.Scheduler.MaxFailedRequestRate.IntervalSeconds == 0, zeroTmpl, ".monitor.maxFailedRequestRate.intervalSeconds")
	erc.Whenf(ec, c.RefreshStateInterval.Seconds == 0, zeroTmpl, ".refreshStateInterval.seconds")
	erc.Whenf(ec, c.DeadlockCheckerDelay.Seconds == 0, zeroTmpl, ".deadlockCheckerDelay.seconds")
	erc.Whenf(ec, c.DeadlockCheckerTimeout.Seconds == 0, zeroTmpl, ".deadlockCheckerTimeout.seconds")

	return ec.Resolve()
}
