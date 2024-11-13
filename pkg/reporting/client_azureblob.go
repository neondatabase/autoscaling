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

// NewRequest implements BaseClient
func (c AzureClient) NewRequest() ClientRequest {
	return &azureRequest{
		AzureClient: c,
		key:         c.generateKey(),
	}
}

// azureRequest is the implementation of ClientRequest used by AzureClient
type azureRequest struct {
	AzureClient
	key string
}

// LogFields implements ClientRequest
func (r *azureRequest) LogFields() zap.Field {
	return zap.Inline(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("container", r.cfg.Container)
		enc.AddString("key", r.key)
		enc.AddString("endpoint", r.cfg.Endpoint)
		return nil
	}))
}

// Send implements ClientRequest
func (r *azureRequest) Send(ctx context.Context, payload []byte) SimplifiableError {
	var err error

	opts := azblob.UploadBufferOptions{} //nolint:exhaustruct // It's part of Azure SDK
	_, err = r.client.UploadBuffer(ctx, r.cfg.Container, r.key, payload, &opts)
	if err != nil {
		return AzureError{Err: err}
	}

	return nil
}
