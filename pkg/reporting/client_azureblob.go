package reporting

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type AzureAuthSharedKey struct {
	AccountName string `json:"accountName"`
	AccountKey  string `json:"accountKey"`
}

type AzureBlobStorageClientConfig struct {
	// In Azure a Container is close to a bucket in AWS S3
	Container string `json:"container"`
	// Example Endpoint: "https://MYSTORAGEACCOUNT.blob.core.windows.net/"
	Endpoint string `json:"endpoint"`
}

type AzureClient struct {
	cfg    AzureBlobStorageClientConfig
	client *azblob.Client

	generateKey func() string
}

type AzureError struct {
	Err error
}

func (e AzureError) Error() string {
	return fmt.Sprintf("%s: %s", e.Simplified(), e.Err.Error())
}

func (e AzureError) Unwrap() error {
	return e.Err
}

func (e AzureError) Simplified() string {
	return "Azure Blob error"
}

func NewAzureBlobStorageClient(
	cfg AzureBlobStorageClientConfig,
	generateKey func() string,
) (*AzureClient, error) {
	//nolint:exhaustruct // It's part of Azure SDK
	clientOptions := &azblob.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Telemetry: policy.TelemetryOptions{ApplicationID: "neon-autoscaler"},
		},
	}

	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	client, err := azblob.NewClient(cfg.Endpoint, credential, clientOptions)
	if err != nil {
		return nil, &AzureError{err}
	}

	return NewAzureBlobStorageClientWithBaseClient(client, cfg, generateKey), nil
}

func NewAzureBlobStorageClientWithBaseClient(
	client *azblob.Client,
	cfg AzureBlobStorageClientConfig,
	generateKey func() string,
) *AzureClient {
	return &AzureClient{
		cfg:         cfg,
		client:      client,
		generateKey: generateKey,
	}
}

func (c AzureClient) LogFields() zap.Field {
	return zap.Inline(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("container", c.cfg.Container)
		// // TODO: need to make a dedicated "request" object that's aware of the path used.
		// enc.AddString("prefixInContainer", c.cfg.PrefixInContainer)
		enc.AddString("endpoint", c.cfg.Endpoint)
		return nil
	}))
}

func (c AzureClient) Send(ctx context.Context, payload []byte, traceID string) SimplifiableError {
	var err error

	key := c.generateKey()
	opts := azblob.UploadBufferOptions{} //nolint:exhaustruct // It's part of Azure SDK
	_, err = c.client.UploadBuffer(ctx, c.cfg.Container, key, payload, &opts)
	if err != nil {
		return AzureError{Err: err}
	}

	return nil
}
