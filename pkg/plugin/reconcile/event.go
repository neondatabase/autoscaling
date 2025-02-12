package reconcile

import (
	"fmt"
)

// EventKind is the kind of change that happened to the object that the handler is now tasked with
// responding to.
type EventKind string

const (
	EventKindAdded    EventKind = "Added"
	EventKindModified EventKind = "Modified"
	EventKindDeleted  EventKind = "Deleted"

	// EventKindEphemeral represents the combination when an addition was not handled before the
	// object was deleted, yet we still may want to process either of these.
	EventKindEphemeral EventKind = "Ephemeral"
)

// Merge returns the combination of the two events
//
// To be precise, the results are:
//
//   - Added + Modified = Added
//   - Modified + Modified = Modified
//   - Modified + Deleted = Deleted
//   - Added + Deleted = Ephemeral
//
// And Ephemeral events are expected not to be merged with anything.
//
// In all cases, the more recent state of the object is expected to be used.
func (k EventKind) Merge(other EventKind) EventKind {
	if k == EventKindEphemeral || other == EventKindEphemeral {
		panic(fmt.Sprintf("cannot merge(%s, %s) involving an ephemeral event", k, other))
	}

	// modified + anything is ok, for the most part
	if k == EventKindModified {
		return other
	} else if other == EventKindModified {
		return k
	}

	if k == EventKindAdded && other == EventKindDeleted || k == EventKindDeleted && other == EventKindAdded {
		return EventKindEphemeral
	}

	// All that's left is Added+Added and Deleted+Deleted, both of which don't make sense.
	panic(fmt.Sprintf("cannot merge(%s, %s)", k, other))
}
