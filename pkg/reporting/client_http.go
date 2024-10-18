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

// NewRequest implements BaseClient
func (c HTTPClient) NewRequest(traceID string) ClientRequest {
	return &httpRequest{
		HTTPClient: c,
		traceID:    traceID,
	}
}

// httpRequest is the implementation of ClientRequest used by HTTPClient
type httpRequest struct {
	HTTPClient
	traceID string
}

// Send implements ClientRequest
func (r *httpRequest) Send(ctx context.Context, payload []byte) SimplifiableError {
	req, err := http.NewRequestWithContext(ctx, r.cfg.Method, r.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return httpRequestError{err: err}
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-trace-id", r.traceID)

	resp, err := r.client.Do(req)
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

// LogFields implements ClientRequest
func (r *httpRequest) LogFields() zap.Field {
	return zap.Inline(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("url", r.cfg.URL)
		enc.AddString("method", r.cfg.Method)
		return nil
	}))
}
