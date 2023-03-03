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

func (c Client) NewBatch() *Batch { return &Batch{c: c, events: nil} }

type Batch struct {
	// Q: does this need a mutex?
	c      Client
	events []any
}

// Count returns the number of events in the batch
func (b *Batch) Count() int {
	return len(b.events)
}

func (b *Batch) idempotenize(key string) string {
	if key != "" {
		return key
	}

	return fmt.Sprintf("Host<%s>:ID<%s>:T<%s>", b.c.hostname, uuid.NewString(), time.Now().Format(time.RFC3339))

}

func (b *Batch) AddAbsoluteEvent(e AbsoluteEvent) {
	e.Type = "absolute"
	e.IdempotencyKey = b.idempotenize(e.IdempotencyKey)
	b.events = append(b.events, &e)
}

func (b *Batch) AddIncrementalEvent(e IncrementalEvent) {
	e.Type = "incremental"
	e.IdempotencyKey = b.idempotenize(e.IdempotencyKey)
	b.events = append(b.events, &e)
}

func (b *Batch) Send(ctx context.Context) error {
	if len(b.events) == 0 {
		return nil
	}

	payload, err := json.Marshal(struct {
		Events []any `json:"events"`
	}{Events: b.events})
	if err != nil {
		return err
	}

	r, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/usage_events", b.c.BaseURL), bytes.NewReader(payload))
	if err != nil {
		return err
	}

	resp, err := b.c.httpc.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// theoretically if wanted/needed, we should use an http handler that
	// does the retrying, to avoid writing that logic here.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("got code %d, posting %d events", resp.StatusCode, len(b.events))
	}

	b.events = nil
	return nil
}
