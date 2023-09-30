package testhelpers

import (
	"fmt"
	"time"
)

// FakeClock is a small facility that makes it easy to operation on duration since start with
// relative times.
type FakeClock struct {
	base time.Time
	now  time.Time
}

// NewFakeClock creates a new fake clock, with the initial time set to an unspecified, round number.
func NewFakeClock() *FakeClock {
	base, err := time.Parse(time.RFC3339, "2000-01-01T00:00:00Z") // a nice round number, to make things easier
	if err != nil {
		panic(err)
	}

	return &FakeClock{base: base, now: base}
}

// Now returns the current time of the clock
func (c *FakeClock) Now() time.Time {
	return c.now
}

// Inc adds duration to the current time of the clock
func (c *FakeClock) Inc(duration time.Duration) {
	if duration < 0 {
		panic(fmt.Errorf("(*FakeClock).Inc() called with negative duration %s", duration))
	}
	c.now = c.now.Add(duration)
}

// Elapsed returns the total time added (via Inc) since the clock was started
func (c *FakeClock) Elapsed() time.Duration {
	return c.now.Sub(c.base)
}
