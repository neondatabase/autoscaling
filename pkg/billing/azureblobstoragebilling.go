package billing

import (
	"context"
	"fmt"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/lithammer/shortuuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"time"
)

type AzureAuthType string

const (
	// AzureAuthTypeTests is used in tests
	// It uses well-known storage account and key.
	// See https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azurite
	AzureAuthTypeTests AzureAuthType = "tests"
	// AzureAuthTypeDefault is for pods running in AKS.
	// Make sure you have provisioned Role and you have Managed Identity.
	AzureAuthTypeDefault AzureAuthType = "default"
)

type AzureAuthSharedKey struct {
	AccountName string `json:"accountName"`
	AccountKey  string `json:"accountKey"`
}

type AzureBlockStorageClientConfig struct {
	AuthType  AzureAuthType       `json:"authType"`
	SharedKey *AzureAuthSharedKey `json:"sharedKey"`
	// trackProgress is useful in tests, it's invoked when SDK sends blobs to Azure.
	// Otherwise keep empty.
	trackProgress func(bytesTransferred int64)
	// In Azure a Container is close to a bucket in AWS S3
	Container         string `json:"container"`
	PrefixInContainer string `json:"prefixInContainer"`
	// Example Endpoint: "https://MYSTORAGEACCOUNT.blob.core.windows.net/"
	Endpoint string `json:"endpoint"`
	// Use generateKey for tests.
	// Otherwise, keep empty.
	generateKey func() string
}

type AzureError struct {
	Err error
}

func (e AzureError) Error() string {
	return fmt.Sprintf("S3 error: %s", e.Err.Error())
}

func (e AzureError) Unwrap() error {
	return e.Err
}

type AzureClient struct {
	cfg AzureBlockStorageClientConfig
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
	if c.cfg.generateKey != nil {
		return c.cfg.generateKey()
	}
	// Example: prefixInContainer/year=2021/month=01/day=26/hh:mm:ssZ_{uuid}.ndjson.gz
	now := time.Now()
	id := shortuuid.New()

	filename := fmt.Sprintf("year=%d/month=%02d/day=%02d/%s_%s.ndjson.gz",
		now.Year(), now.Month(), now.Day(),
		now.Format("15:04:05Z"),
		id,
	)
	return fmt.Sprintf("%s/%s", c.cfg.PrefixInContainer, filename)
}

func (c AzureClient) send(ctx context.Context, payload []byte, traceID TraceID) error {
	_, err := c.c.UploadBuffer(ctx, c.cfg.Container, c.generateKey(), payload, &azblob.UploadBufferOptions{
		Progress: c.cfg.trackProgress,
	})
	return handleAzureError(err)
}

var _ Client = &AzureClient{}

func NewAzureBlockStorageClient(cfg AzureBlockStorageClientConfig) (*AzureClient, error) {
	// TODO: 13890 which creds to take?
	var client *azblob.Client
	clientOptions := &azblob.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Telemetry: policy.TelemetryOptions{ApplicationID: "neon-autoscaler"},
		},
	}
	switch cfg.AuthType {
	case AzureAuthTypeTests:
		shKey, err := azblob.NewSharedKeyCredential("devstoreaccount1", "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==")
		if err != nil {
			return nil, err
		}

		// https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azurite?tabs=docker-hub%2Cblob-storage#well-known-storage-account-and-key
		client, err = azblob.NewClientWithSharedKeyCredential(cfg.Endpoint, shKey, clientOptions)
		if err != nil {
			return nil, &AzureError{err}
		}
	case AzureAuthTypeDefault:
		credential, err := azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{
			ClientOptions: azcore.ClientOptions{},
		})
		if err != nil {
			return nil, err
		}
		client, err = azblob.NewClient(cfg.Endpoint, credential, clientOptions)
		if err != nil {
			return nil, &AzureError{err}
		}
	default:
		return nil, fmt.Errorf("unsupported auth type: %q", cfg.AuthType)
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
