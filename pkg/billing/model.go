package billing

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

type AbsoluteEvent struct {
	IdempotencyKey string    `json:"idempotency_key"`
	MetricName     string    `json:"metric"`
	Type           string    `json:"type"`
	TenantID       string    `json:"tenant_id"`
	TimelineID     string    `json:"timeline_id"`
	Time           time.Time `json:"time"`
	Value          int       `json:"value"`
}

type IncrementalEvent struct {
	IdempotencyKey string    `json:"idempotency_key"`
	MetricName     string    `json:"metric"`
	Type           string    `json:"type"`
	EndpointID     string    `json:"endpoint_id"`
	StartTime      time.Time `json:"start_time"`
	StopTime       time.Time `json:"stop_time"`
	Value          int       `json:"value"`
}

type enrichable interface {
	setEventType()
	idempotencyKey() *string
}

// Enrich sets the event's Type and IdempotencyKey fields, so that users of this API don't need to
// manually set them
//
// Enrich is already called by (*Batch).Add, but this is used in the autoscaler-agent's billing
// implementation, which manually calls Enrich in order to log the IdempotencyKey for each event.
func Enrich[E enrichable](batch *Batch, event E) E {
	event.setEventType()

	key := event.idempotencyKey()
	if *key == "" {
		*key = fmt.Sprintf("Host<%s>:ID<%s>:T<%s>", batch.c.hostname, uuid.NewString(), time.Now().Format(time.RFC3339))
	}

	return event
}

// setEventType implements enrichable
func (e *AbsoluteEvent) setEventType() {
	e.Type = "absolute"
}

// idempotencyKey implements enrichable
func (e *AbsoluteEvent) idempotencyKey() *string {
	return &e.IdempotencyKey
}

// setEventType implements enrichable
func (e *IncrementalEvent) setEventType() {
	e.Type = "incremental"
}

// idempotencyKey implements enrichable
func (e *IncrementalEvent) idempotencyKey() *string {
	return &e.IdempotencyKey
}
