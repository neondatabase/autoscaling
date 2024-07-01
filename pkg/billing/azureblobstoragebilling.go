package billing

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
	// Files will be created with name starting with PrefixInContainer
	PrefixInContainer string `json:"prefixInContainer"`
	// Example Endpoint: "https://MYSTORAGEACCOUNT.blob.core.windows.net/"
	Endpoint string `json:"endpoint"`

	//
	// Unexported attributes follow this comment.
	//

	// Use generateKey for tests.
	// Otherwise, keep empty.
	generateKey func() string
	// Use getClient for tests.
	// Otherwise keep empty.
	getClient func() (*azblob.Client, error)
}

type AzureError struct {
	Err error
}

func (e AzureError) Error() string {
	return fmt.Sprintf("Azure Blob error: %s", e.Err.Error())
}

func (e AzureError) Unwrap() error {
	return e.Err
}

type AzureClient struct {
	cfg AzureBlobStorageClientConfig
	c   *azblob.Client
}

func (c AzureClient) LogFields() zap.Field {
	return zap.Inline(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("container", c.cfg.Container)
		enc.AddString("prefixInContainer", c.cfg.PrefixInContainer)
		enc.AddString("endpoint", c.cfg.Endpoint)
		return nil
	}))
}

func (c AzureClient) generateKey() string {
	return c.cfg.generateKey()
}

func (c AzureClient) send(ctx context.Context, payload []byte, _ TraceID) error {
	payload, err := compress(payload)
	if err != nil {
		return err
	}
	_, err = c.c.UploadBuffer(ctx, c.cfg.Container, c.generateKey(), payload,
		&azblob.UploadBufferOptions{}, //nolint:exhaustruct // It's part of Azure SDK
	)
	return handleAzureError(err)
}

func defaultGenerateKey(cfg AzureBlobStorageClientConfig) func() string {
	return func() string {
		return keyTemplate(cfg.PrefixInContainer)
	}
}

func defaultGetClient(cfg AzureBlobStorageClientConfig) func() (*azblob.Client, error) {
	return func() (*azblob.Client, error) {
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
		return client, nil
	}
}

func NewAzureBlobStorageClient(cfg AzureBlobStorageClientConfig) (*AzureClient, error) {
	var client *azblob.Client

	if cfg.generateKey == nil {
		cfg.generateKey = defaultGenerateKey(cfg)
	}

	if cfg.getClient == nil {
		cfg.getClient = defaultGetClient(cfg)
	}
	client, err := cfg.getClient()
	if err != nil {
		return nil, err
	}

	return &AzureClient{
		cfg: cfg,
		c:   client,
	}, nil
}

func handleAzureError(err error) error {
	if err == nil {
		return nil
	}
	return AzureError{err}
}
