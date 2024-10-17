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

// LogFields implements BaseClient
func (c *S3Client) LogFields() zap.Field {
	return zap.Inline(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("bucket", c.cfg.Bucket)
		// // TODO: need to make a dedicated "request" object that's aware of the path used.
		// enc.AddString("prefixInBucket", c.cfg.PrefixInBucket)
		enc.AddString("region", c.cfg.Region)
		enc.AddString("endpoint", c.cfg.Endpoint)
		return nil
	}))
}

func (c *S3Client) Send(ctx context.Context, payload []byte, traceID string) SimplifiableError {
	var err error

	key := c.generateKey()
	r := bytes.NewReader(payload)
	_, err = c.client.PutObject(ctx, &s3.PutObjectInput{ //nolint:exhaustruct // AWS SDK
		Bucket: &c.cfg.Bucket,
		Key:    &key,
		Body:   r,
	})

	if err != nil {
		return S3Error{Err: err}
	}

	return nil
}
