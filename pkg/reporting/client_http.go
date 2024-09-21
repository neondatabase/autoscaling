package reporting

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type HTTPClient struct {
	client *http.Client
	cfg    HTTPClientConfig
}

type HTTPClientConfig struct {
	URL    string `json:"url"`
	Method string `json:"method"`
}

type httpRequestError struct {
	err error
}

func (e httpRequestError) Error() string {
	return fmt.Sprintf("Error making request: %s", e.err.Error())
}

func (e httpRequestError) Unwrap() error {
	return e.err
}

func (e httpRequestError) Simplified() string {
	return util.RootError(e.err).Error()
}

type httpUnexpectedStatusCodeError struct {
	statusCode int
}

func (e httpUnexpectedStatusCodeError) Error() string {
	return fmt.Sprintf("Unexpected HTTP status code %d", e.statusCode)
}

func (e httpUnexpectedStatusCodeError) Simplified() string {
	return fmt.Sprintf("HTTP code %d", e.statusCode)
}

func NewHTTPClient(client *http.Client, cfg HTTPClientConfig) HTTPClient {
	return HTTPClient{
		client: client,
		cfg:    cfg,
	}
}

func (c HTTPClient) Send(ctx context.Context, payload []byte, traceID string) SimplifiableError {
	r, err := http.NewRequestWithContext(ctx, c.cfg.Method, c.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return httpRequestError{err: err}
	}
	r.Header.Set("content-type", "application/json")
	r.Header.Set("x-trace-id", traceID)

	resp, err := c.client.Do(r)
	if err != nil {
		return httpRequestError{err: err}
	}
	defer resp.Body.Close()

	// theoretically if wanted/needed, we should use an http handler that
	// does the retrying, to avoid writing that logic here.
	if resp.StatusCode != http.StatusOK {
		return httpUnexpectedStatusCodeError{statusCode: resp.StatusCode}
	}

	return nil
}

func (c HTTPClient) LogFields() zap.Field {
	return zap.Inline(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("url", c.cfg.URL)
		enc.AddString("method", c.cfg.Method)
		return nil
	}))
}
