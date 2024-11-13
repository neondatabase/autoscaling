package reporting

import (
	"bytes"
	"context"
	"fmt"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// S3Client is a BaseClient for S3
type S3Client struct {
	cfg    S3ClientConfig
	client *s3.Client

	generateKey func() string
}

type S3ClientConfig struct {
	Bucket   string `json:"bucket"`
	Region   string `json:"region"`
	Endpoint string `json:"endpoint"`
}

type S3Error struct {
	Err error
}

func (e S3Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Simplified(), e.Err.Error())
}

func (e S3Error) Unwrap() error {
	return e.Err
}

func (e S3Error) Simplified() string {
	return "S3 error"
}

func NewS3Client(
	ctx context.Context,
	cfg S3ClientConfig,
	generateKey func() string,
) (*S3Client, error) {
	// Timeout in case we have hidden IO inside config creation
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	s3Config, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, S3Error{Err: err}
	}

	client := s3.NewFromConfig(s3Config, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = &cfg.Endpoint
		}
		o.UsePathStyle = true // required for minio
	})

	return &S3Client{
		cfg:    cfg,
		client: client,

		generateKey: generateKey,
	}, nil
}

// NewRequest implements BaseClient
func (c *S3Client) NewRequest() ClientRequest {
	return &s3Request{
		S3Client: c,
		key:      c.generateKey(),
	}
}

// s3Request is the implementation of ClientRequest used by S3Client
type s3Request struct {
	*S3Client
	key string
}

// LogFields implements ClientRequest
func (r *s3Request) LogFields() zap.Field {
	return zap.Inline(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("bucket", r.cfg.Bucket)
		enc.AddString("key", r.key)
		enc.AddString("region", r.cfg.Region)
		enc.AddString("endpoint", r.cfg.Endpoint)
		return nil
	}))
}

// Send implements ClientRequest
func (r *s3Request) Send(ctx context.Context, payload []byte) SimplifiableError {
	var err error

	body := bytes.NewReader(payload)
	_, err = r.client.PutObject(ctx, &s3.PutObjectInput{ //nolint:exhaustruct // AWS SDK
		Bucket: &r.cfg.Bucket,
		Key:    &r.key,
		Body:   body,
	})
	if err != nil {
		return S3Error{Err: err}
	}

	return nil
}
