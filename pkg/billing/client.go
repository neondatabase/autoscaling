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

	"github.com/google/uuid"
)

type Client struct {
	BaseURL  string
	httpc    *http.Client
	hostname string
}

func NewClient(url string, c *http.Client) Client {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = fmt.Sprintf("unknown-%d", rand.Intn(1000))
	}
	return Client{BaseURL: url, httpc: c, hostname: hostname}
}

func (c Client) Hostname() string {
	return c.hostname
}

// Enrich sets the event's Type and IdempotencyKey fields, so that users of this API don't need to
// manually set them
func Enrich[E Event](hostname string, event E) E {
	event.setType()

	key := event.getIdempotencyKey()
	if *key == "" {
		*key = fmt.Sprintf("Host<%s>:ID<%s>:T<%s>", hostname, uuid.NewString(), time.Now().Format(time.RFC3339))
	}

	return event
}

// Send attempts to push the events to the remote endpoint.
//
// On failure, the error is guaranteed to be one of: JSONError, RequestError, or
// UnexpectedStatusCodeError.
func Send[E Event](ctx context.Context, client Client, events []E) error {
	if len(events) == 0 {
		return nil
	}

	payload, err := json.Marshal(struct {
		Events []E `json:"events"`
	}{Events: events})
	if err != nil {
		return JSONError{Err: err}
	}

	r, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/usage_events", client.BaseURL), bytes.NewReader(payload))
	if err != nil {
		return RequestError{Err: err}
	}
	r.Header.Set("content-type", "application/json")

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
