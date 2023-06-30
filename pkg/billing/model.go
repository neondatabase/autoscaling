package billing

import (
	"time"
)

type Event[Self any] interface {
	AbsoluteEvent | IncrementalEvent

	eventMethods[Self]
}

// eventMethods exists because Go does not let you assume that a value of generic type "Foo | Bar"
// has the fields common to both Foo and Bar. So we still have to thread through these fields.
//
// The methods in this interface *could* have directly been a part of Event, but then we couldn't do
// the "implements eventMethods" assertions below (because you aren't allowed to store a value of
// type "Foo | Bar"; it can only be used as a constraint for generics).
//
// eventMethods also takes Self as a parameter because we want Event[...] to be implemented for the
// raw AbsoluteEvent and IncrementalEvent types, not pointers to them, because that would make
// things just a bit more annoying. But we still need to set field values, which we can ONLY do by
// taking the address of the field *somehow*, so we use this interface to return functions to do
// all that stuff for us.
type eventMethods[Self any] interface {
	typeSetter() func(*Self)
	idempotencyKeyGetter() func(*Self) *string
}

var (
	_ eventMethods[AbsoluteEvent]    = AbsoluteEvent{}    //nolint:exhaustruct // just checking they impl the interface
	_ eventMethods[IncrementalEvent] = IncrementalEvent{} //nolint:exhaustruct // just checking they impl the interface
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

// typeSetter implements eventMethods
func (AbsoluteEvent) typeSetter() func(*AbsoluteEvent) { //nolint:unused // linter is incorrect
	return func(e *AbsoluteEvent) { e.Type = "absolute" }
}

// idempotencyKeyGetter implements eventMethods
func (AbsoluteEvent) idempotencyKeyGetter() func(*AbsoluteEvent) *string { //nolint:unused // linter is incorrect
	return func(e *AbsoluteEvent) *string { return &e.IdempotencyKey }
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

// typeSetter implements eventMethods
func (IncrementalEvent) typeSetter() func(*IncrementalEvent) { //nolint:unused // linter is incorrect
	return func(e *IncrementalEvent) { e.Type = "incremental" }
}

// idempotencyKeyGetter implements eventMethods
func (IncrementalEvent) idempotencyKeyGetter() func(*IncrementalEvent) *string { //nolint:unused // linter is incorrect
	return func(e *IncrementalEvent) *string { return &e.IdempotencyKey }
}
