package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type Client struct {
	BaseURL string
	httpc   *http.Client
}

func NewClient(url string, c *http.Client) Client { return Client{BaseURL: url, httpc: c} }

func (c Client) NewBatch() *Batch { return &Batch{c: c, events: nil} }

type Batch struct {
	// Q: does this need a mutex?
	c      Client
	events []json.Marshaler
}

func (b *Batch) AddAbsoluteEvent(e AbsoluteEvent) {
	b.events = append(b.events, &e)
}

func (b *Batch) AddIncrementalEvent(e IncrementalEvent) {
	b.events = append(b.events, &e)
}

func (b *Batch) Send(ctx context.Context) error {
	if len(b.events) == 0 {
		return nil
	}

	payload, err := json.Marshal(struct {
		Events []json.Marshaler `json:"events"`
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
