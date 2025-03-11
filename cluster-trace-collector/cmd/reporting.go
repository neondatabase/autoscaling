package main

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/reporting"
)

func setupReporting(
	ctx context.Context,
	logger *zap.Logger,
	args *CliArgs,
	reg prometheus.Registerer,
) (*reporting.EventSink[TraceEvent], error) {
	generateKey := newBlobStorageKeyGenerator(args.bucketPrefix)

	var baseClient reporting.BaseClient
	var clientName string

	switch {
	case args.s3Config != nil:
		c, err := reporting.NewS3Client(ctx, *args.s3Config, generateKey)
		if err != nil {
			return nil, fmt.Errorf("failed to set up S3 client: %w", err)
		}
		baseClient = c
		clientName = "s3"
	case args.azureBlobConfig != nil:
		c, err := reporting.NewAzureBlobStorageClient(*args.azureBlobConfig, generateKey)
		if err != nil {
			return nil, fmt.Errorf("failed to set up Azure Blob Storage client: %w", err)
		}
		baseClient = c
		clientName = "azureblob"
	default:
		panic("expected to have one client")
	}

	metrics := reporting.NewEventSinkMetrics("autoscaling_trace_collector_reporting", reg)

	client := reporting.Client[TraceEvent]{
		Name:       clientName,
		Base:       baseClient,
		BaseConfig: *args.baseConfig,
		NewBatchBuilder: func() reporting.BatchBuilder[TraceEvent] {
			return reporting.NewJSONLinesBuilder[TraceEvent](reporting.NewGZIPBuffer())
		},
	}

	sink := reporting.NewEventSink(logger.Named("reporting"), metrics, client)
	return sink, nil
}
