package billing

import (
	"fmt"
	"math/rand"
	"os"
	"time"
)

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
