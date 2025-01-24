package scalingevents

import (
	"context"
	"fmt"
	"time"

	"github.com/lithammer/shortuuid"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/reporting"
)

type ClientsConfig struct {
	S3 *S3ClientConfig `json:"s3"`
}

type S3ClientConfig struct {
	reporting.BaseClientConfig
	reporting.S3ClientConfig
	PrefixInBucket string `json:"prefixInBucket"`
}

type eventsClient = reporting.Client[ScalingEvent]

func createClients(ctx context.Context, logger *zap.Logger, cfg ClientsConfig) ([]eventsClient, error) {
	var clients []eventsClient

	if c := cfg.S3; c != nil {
		generateKey := newBlobStorageKeyGenerator(c.PrefixInBucket)
		client, err := reporting.NewS3Client(ctx, c.S3ClientConfig, generateKey)
		if err != nil {
			return nil, fmt.Errorf("error creating S3 client: %w", err)
		}
		logger.Info("Created S3 client for scaling events", zap.Any("config", c))

		clients = append(clients, eventsClient{
			Name:           "s3",
			Base:           client,
			BaseConfig:     c.BaseClientConfig,
			SerializeBatch: reporting.WrapSerialize[ScalingEvent](reporting.GZIPCompress, reporting.JSONLinesMarshalBatch),
		})
	}

	return clients, nil
}

// Returns a function to generate keys for the placement of scaling events data into blob storage.
//
// Example: prefix/2024/10/31/23/events_{uuid}.ndjson.gz (11pm on halloween, UTC)
//
// NOTE: This key format is different from the one we use for billing, but similar to the one proxy
// uses for its reporting.
func newBlobStorageKeyGenerator(prefix string) func() string {
	return func() string {
		now := time.Now().UTC()
		id := shortuuid.New()

		return fmt.Sprintf(
			"%s/%d/%02d/%02d/%02d/events_%s.ndjson.gz",
			prefix,
			now.Year(), now.Month(), now.Day(), now.Hour(),
			id,
		)
	}
}
