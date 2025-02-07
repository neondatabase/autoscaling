package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lithammer/shortuuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/neondatabase/autoscaling/pkg/reporting"
	"github.com/neondatabase/autoscaling/pkg/util"
)

func main() {
	logConfig := zap.NewProductionConfig()
	logConfig.Sampling = nil                // Disable sampling, which the production config enables by default.
	logConfig.Level.SetLevel(zap.InfoLevel) // Only "info" level and above (i.e. not debug logs)
	logger := zap.Must(logConfig.Build()).Named("cluster-trace-collector")
	defer logger.Sync() //nolint:errcheck // what are we gonna do, log something about it?

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()

	args, err := getCli()
	if err != nil {
		logger.Fatal("could not get args from CLI", zap.Error(err))
	}

	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		logger.Fatal("could not get k8s client config", zap.Error(err))
	}
	k8sClient, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		logger.Fatal("could not create k8s client", zap.Error(err))
	}

	reg := prometheus.NewRegistry()

	if err := util.StartPrometheusMetricsServer(ctx, logger.Named("prometheus"), 9100, reg); err != nil {
		logger.Fatal("failed to start prometheus metrics server", zap.Error(err))
	}

	eventSink, err := setupReporting(ctx, logger, args, reg)
	if err != nil {
		logger.Fatal("failed to set up event reporting", zap.Error(err))
	}

	err = startK8sWatches(ctx, logger, k8sClient, reg, eventSink, args.keepPodLabels, args.keepNodeLabels)
	if err != nil {
		logger.Fatal("failed to start k8s watches", zap.Error(err))
	}

	// wait to be canceled by SIGTERM
	<-ctx.Done()
}

func newBlobStorageKeyGenerator(prefix string) func() string {
	return func() string {
		now := time.Now().UTC()
		id := shortuuid.New()

		return fmt.Sprintf(
			"%s/%d/%02d/%02d/%02d_events_%s.ndjson.gz",
			prefix,
			now.Year(), now.Month(), now.Day(),
			now.Hour(), id,
		)
	}
}

type CliArgs struct {
	// bucketPrefix is the prefix to use for files written to the S3 bucket or Azure Blob Storage
	// container.
	bucketPrefix string
	// baseConfig is the shared configuration for event reporting
	baseConfig *reporting.BaseClientConfig

	// s3Config and AzureBlobConfig configure events are written to S3 / Azure Blob Storage.
	// Exactly one of these must be present.
	s3Config        *reporting.S3ClientConfig
	azureBlobConfig *reporting.AzureBlobStorageClientConfig

	keepPodLabels  []string
	keepNodeLabels []string
}

func getCli() (*CliArgs, error) {
	args := &CliArgs{
		bucketPrefix:    "",
		baseConfig:      nil,
		s3Config:        nil,
		azureBlobConfig: nil,
		keepPodLabels:   []string{},
		keepNodeLabels:  []string{},
	}

	flag.StringVar(
		&args.bucketPrefix, "bucket-prefix", "",
		"Sets the prefix to use for files written to the S3 bucket or Azure Blob Storage container",
	)
	flag.Func("base-config", "Sets the base configuration for event reporting", func(v string) error {
		var cfg reporting.BaseClientConfig
		if err := json.Unmarshal([]byte(v), &cfg); err != nil {
			return fmt.Errorf("falied to unmarshal json: %w", err)
		}
		args.baseConfig = &cfg
		return nil
	})
	flag.Func("s3-config", "Configures writing events to S3", func(v string) error {
		var cfg reporting.S3ClientConfig
		if err := json.Unmarshal([]byte(v), &cfg); err != nil {
			return fmt.Errorf("falied to unmarshal json: %w", err)
		}
		args.s3Config = &cfg
		return nil
	})
	flag.Func("azure-blob-config", "Configures writing events to Azure Blob Storage", func(v string) error {
		var cfg reporting.AzureBlobStorageClientConfig
		if err := json.Unmarshal([]byte(v), &cfg); err != nil {
			return fmt.Errorf("falied to unmarshal json: %w", err)
		}
		args.azureBlobConfig = &cfg
		return nil
	})
	flag.Func("keep-pod-labels", "Comma-separated list of Pod labels to preserve in events", func(v string) error {
		if v != "" {
			args.keepPodLabels = strings.Split(v, ",")
		}
		return nil
	})
	flag.Func("keep-node-labels", "Comma-separated list of Node labels to preserve in events", func(v string) error {
		if v != "" {
			args.keepNodeLabels = strings.Split(v, ",")
		}
		return nil
	})

	flag.Parse()

	var errs []error

	if args.bucketPrefix == "" {
		errs = append(errs, errors.New("missing required argument '--bucket-prefix'"))
	}
	if args.baseConfig == nil {
		errs = append(errs, errors.New("missing required argument '--base-config"))
	}
	if args.s3Config == nil && args.azureBlobConfig == nil {
		errs = append(errs, errors.New(
			"missing bucket configuration, must have '--s3-config' or '--azure-blob-config'",
		))
	} else if args.s3Config != nil && args.azureBlobConfig != nil {
		errs = append(errs, errors.New(
			"conflicting arguments '--s3-config' and '--azure-blob-config', must have just one",
		))
	}

	if len(errs) != 0 {
		return nil, errors.Join(errs...)
	}

	return args, nil
}
