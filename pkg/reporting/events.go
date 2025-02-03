package reporting

import (
	"fmt"
	"time"
)

type Event interface {
	*AbsoluteEvent | *IncrementalEvent

	// eventMethods must be separate from Event so that we can assert that *AbsoluteEvent and
	// *IncrementalEvent both implement it - Go does not allow converting to a value of type Event
	// because it contains "*AbsoluteEvent | *IncrementalEvent", and such constraints can only be
	// used inside of generics.
	eventMethods
}

// eventMethods is a requirement for Event, but exists separately so that we can assert that the
// event types implement it.
//
// The reason this interface even exists in the first place is because we're not allowed to assume
// that a type E implementing Event actually has the common fields from AbsoluteEvent and
// IncrementalEvent, even though it's constrained to either of those types.
type eventMethods interface {
	setType()
	getIdempotencyKey() *string
}

var (
	_ eventMethods = (*AbsoluteEvent)(nil)
	_ eventMethods = (*IncrementalEvent)(nil)
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

// setType implements eventMethods
func (e *AbsoluteEvent) setType() {
	e.Type = "absolute"
}

// getIdempotencyKey implements eventMethods
func (e *AbsoluteEvent) getIdempotencyKey() *string {
	return &e.IdempotencyKey
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

// setType implements eventMethods
func (e *IncrementalEvent) setType() {
	e.Type = "incremental"
}

// getIdempotencyKey implements eventMethods
func (e *IncrementalEvent) getIdempotencyKey() *string {
	return &e.IdempotencyKey
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
