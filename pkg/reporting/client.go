package reporting

import (
	"context"

	"go.uber.org/zap"
)

type Client[E any] struct {
	Name            string
	Base            BaseClient
	BaseConfig      BaseClientConfig
	NewBatchBuilder func() BatchBuilder[E]
}

// BaseClient is the shared lower-level interface to send the processed data somewhere.
//
// It's split into the client itself, intended to be used as a kind of persistent object, and a
// separate ClientRequest object, intended to be used only for the lifetime of a single request.
//
// See S3Client, AzureBlobClient, and HTTPClient.
type BaseClient interface {
	NewRequest() ClientRequest
}

var (
	_ BaseClient = (*S3Client)(nil)
	_ BaseClient = (*AzureClient)(nil)
	_ BaseClient = (*HTTPClient)(nil)
)

// ClientRequest is the abstract interface for a single request to send a batch of processed data.
//
// This exists as a separate interface because there are some request-scoped values that we'd like
// to include in the call to LogFields().
type ClientRequest interface {
	LogFields() zap.Field
	Send(ctx context.Context, payload []byte) SimplifiableError
}

type BaseClientConfig struct {
	PushEverySeconds          uint `json:"pushEverySeconds"`
	PushRequestTimeoutSeconds uint `json:"pushRequestTimeoutSeconds"`
	MaxBatchSize              uint `json:"maxBatchSize"`
}

// SimplifiableError is an extension of the standard 'error' interface that provides a
// safe-for-metrics string representing the error.
type SimplifiableError interface {
	error

	Simplified() string
}
