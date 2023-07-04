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
	"github.com/lithammer/shortuuid"
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

type TraceID string

func (c Client) GenerateTraceID() TraceID {
	return TraceID(shortuuid.New())
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
func Send[E Event](ctx context.Context, client Client, traceID TraceID, events []E) error {
	if len(events) == 0 {
		return nil
	}

	payload, err := json.Marshal(struct {
		Events []E `json:"events"`
	}{Events: events})
	if err != nil {
		return err
	}

	r, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/usage_events", client.BaseURL), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	r.Header.Set("content-type", "application/json")
	r.Header.Set("x-trace-id", string(traceID))

	resp, err := client.httpc.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// theoretically if wanted/needed, we should use an http handler that
	// does the retrying, to avoid writing that logic here.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("got code %d, posting %d events", resp.StatusCode, len(events))
	}

	return nil
}
