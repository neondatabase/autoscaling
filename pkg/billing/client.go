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

func NewBatch[E Event[E]](client Client) *Batch[E] {
	return &Batch[E]{c: client, events: nil}
}

type Batch[E Event[E]] struct {
	// Q: does this need a mutex?
	c      Client
	events []E
}

// Count returns the number of events in the batch
func (b *Batch[E]) Count() int {
	return len(b.events)
}

// Enrich sets the event's Type and IdempotencyKey fields, so that users of this API don't need to
// manually set them
//
// Enrich is already called by (*Batch).Add, but this is used in the autoscaler-agent's billing
// implementation, which manually calls Enrich in order to log the IdempotencyKey for each event.
func (b *Batch[E]) Enrich(event E) E {
	event.typeSetter()(&event)

	key := event.idempotencyKeyGetter()(&event)
	if *key == "" {
		*key = fmt.Sprintf("Host<%s>:ID<%s>:T<%s>", b.c.hostname, uuid.NewString(), time.Now().Format(time.RFC3339))
	}

	return event
}

// Add enriches an event and adds it to the batch
func (b *Batch[E]) Add(event E) {
	b.Enrich(event) // Realistically, we're already calling Enrich before Add, but this is a good safety mechanism.
	b.events = append(b.events, event)
}

func (b *Batch[E]) Send(ctx context.Context) error {
	if len(b.events) == 0 {
		return nil
	}

	payload, err := json.Marshal(struct {
		Events []E `json:"events"`
	}{Events: b.events})
	if err != nil {
		return err
	}

	r, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/usage_events", b.c.BaseURL), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	r.Header.Set("content-type", "application/json")

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
