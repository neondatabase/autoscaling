package billing

// Management of billing clients

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lithammer/shortuuid"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/reporting"
)

type ClientsConfig struct {
	AzureBlob *AzureBlobStorageClientConfig `json:"azureBlob"`
	HTTP      *HTTPClientConfig             `json:"http"`
	S3        *S3ClientConfig               `json:"s3"`
}

type S3ClientConfig struct {
	reporting.BaseClientConfig
	reporting.S3ClientConfig
	PrefixInBucket string `json:"prefixInBucket"`
}

type AzureBlobStorageClientConfig struct {
	reporting.BaseClientConfig
	reporting.AzureBlobStorageClientConfig
	PrefixInContainer string `json:"prefixInContainer"`
}

type HTTPClientConfig struct {
	reporting.BaseClientConfig
	URL string `json:"url"`
}

type billingClient = reporting.Client[*IncrementalEvent]

func createClients(ctx context.Context, logger *zap.Logger, cfg ClientsConfig) ([]billingClient, error) {
	var clients []billingClient

	if c := cfg.HTTP; c != nil {
		client := reporting.NewHTTPClient(http.DefaultClient, reporting.HTTPClientConfig{
			URL:    fmt.Sprintf("%s/usage_events", c.URL),
			Method: http.MethodPost,
		})
		logger.Info("Created HTTP client for billing events", zap.Any("config", c))

		clients = append(clients, billingClient{
			Name:            "http",
			Base:            client,
			BaseConfig:      c.BaseClientConfig,
			NewBatchBuilder: jsonArrayBatch(reporting.NewByteBuffer), // note: NOT gzipped.
		})

	}
	if c := cfg.AzureBlob; c != nil {
		generateKey := newBlobStorageKeyGenerator(c.PrefixInContainer)
		client, err := reporting.NewAzureBlobStorageClient(c.AzureBlobStorageClientConfig, generateKey)
		if err != nil {
			return nil, fmt.Errorf("error creating Azure Blob Storage client: %w", err)
		}
		logger.Info("Created Azure Blob Storage client for billing events", zap.Any("config", c))

		clients = append(clients, billingClient{
			Name:            "azureblob",
			Base:            client,
			BaseConfig:      c.BaseClientConfig,
			NewBatchBuilder: jsonLinesBatch(reporting.NewGZIPBuffer),
		})
	}
	if c := cfg.S3; c != nil {
		generateKey := newBlobStorageKeyGenerator(c.PrefixInBucket)
		client, err := reporting.NewS3Client(ctx, c.S3ClientConfig, generateKey)
		if err != nil {
			return nil, fmt.Errorf("error creating S3 client: %w", err)
		}
		logger.Info("Created S3 client for billing events", zap.Any("config", c))

		clients = append(clients, billingClient{
			Name:            "s3",
			Base:            client,
			BaseConfig:      c.BaseClientConfig,
			NewBatchBuilder: jsonLinesBatch(reporting.NewGZIPBuffer),
		})
	}

	return clients, nil
}

func jsonArrayBatch[B reporting.IOBuffer](buf func() B) func() reporting.BatchBuilder[*IncrementalEvent] {
	return func() reporting.BatchBuilder[*IncrementalEvent] {
		return reporting.NewJSONArrayBuilder[*IncrementalEvent](buf(), "events")
	}
}

func jsonLinesBatch[B reporting.IOBuffer](buf func() B) func() reporting.BatchBuilder[*IncrementalEvent] {
	return func() reporting.BatchBuilder[*IncrementalEvent] {
		return reporting.NewJSONLinesBuilder[*IncrementalEvent](buf())
	}
}

// Returns a function to generate keys for the placement of billing events data into blob storage.
//
// Example: prefixInContainer/year=2021/month=01/day=26/hh:mm:ssZ_{uuid}.ndjson.gz
//
// NOTE: This key format is different from the one we use for scaling events, but similar to the one
// proxy/storage use.
func newBlobStorageKeyGenerator(prefix string) func() string {
	return func() string {
		now := time.Now()
		id := shortuuid.New()

		if prefix != "" {
			prefix = strings.TrimRight(prefix, "/") + "/"
		}

		return fmt.Sprintf("%syear=%d/month=%02d/day=%02d/hour=%02d/%s_%s.ndjson.gz",
			prefix,
			now.Year(), now.Month(), now.Day(), now.Hour(),
			now.Format("15:04:05Z"),
			id,
		)
	}
}
