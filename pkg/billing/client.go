package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"

	"github.com/lithammer/shortuuid"
)

type Client struct {
	URL   string
	httpc *http.Client
}

var hostname string

func init() {
	var err error
	hostname, err = os.Hostname()
	if err != nil {
		hostname = fmt.Sprintf("unknown-%d", rand.Intn(1000))
	}
}

// GetHostname returns the hostname to be used for enriching billing events (see Enrich())
//
// This function MUST NOT be run before init has finished.
func GetHostname() string {
	return hostname
}

func NewClient(url string, c *http.Client) Client {
	return Client{URL: fmt.Sprintf("%s/usage_events", url), httpc: c}
}

type TraceID string

func (c Client) GenerateTraceID() TraceID {
	return TraceID(shortuuid.New())
}

// Enrich sets the event's Type and IdempotencyKey fields, so that users of this API don't need to
// manually set them
func Enrich[E Event](now time.Time, hostname string, countInBatch, batchSize int, event E) E {
	event.setType()

	// RFC3339 with microsecond precision. Possible to get collisions with millis, nanos are extra.
	// And everything's in UTC, so there's no sense including the offset.
	formattedTime := now.In(time.UTC).Format("2006-01-02T15:04:05.999999Z")

	key := event.getIdempotencyKey()
	if *key == "" {
		*key = fmt.Sprintf("%s-%s-%d/%d", formattedTime, hostname, countInBatch, batchSize)
	}

	return event
}

// Send attempts to push the events to the remote endpoint.
//
// On failure, the error is guaranteed to be one of: JSONError, RequestError, or
// UnexpectedStatusCodeError.
func Send[E Event](ctx context.Context, client Client, traceID TraceID, events []E) error {
	if len(events) == 0 {
		return nil
	}

	payload, err := json.Marshal(struct {
		Events []E `json:"events"`
	}{Events: events})
	if err != nil {
		return JSONError{Err: err}
	}

	r, err := http.NewRequestWithContext(ctx, http.MethodPost, client.URL, bytes.NewReader(payload))
	if err != nil {
		return RequestError{Err: err}
	}
	r.Header.Set("content-type", "application/json")
	r.Header.Set("x-trace-id", string(traceID))

	resp, err := client.httpc.Do(r)
	if err != nil {
		return RequestError{Err: err}
	}
	defer resp.Body.Close()

	// theoretically if wanted/needed, we should use an http handler that
	// does the retrying, to avoid writing that logic here.
	if resp.StatusCode != http.StatusOK {
		return UnexpectedStatusCodeError{StatusCode: resp.StatusCode}
	}

	return nil
}

type JSONError struct {
	Err error
}

func (e JSONError) Error() string {
	return fmt.Sprintf("Error marshaling events: %s", e.Err.Error())
}

func (e JSONError) Unwrap() error {
	return e.Err
}

type RequestError struct {
	Err error
}

func (e RequestError) Error() string {
	return fmt.Sprintf("Error making request: %s", e.Err.Error())
}

func (e RequestError) Unwrap() error {
	return e.Err
}

type UnexpectedStatusCodeError struct {
	StatusCode int
}

func (e UnexpectedStatusCodeError) Error() string {
	return fmt.Sprintf("Unexpected HTTP status code %d", e.StatusCode)
}
