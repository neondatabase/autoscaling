package testhelpers

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// FakeClock is a small facility that makes it easy to operation on duration since start with
// relative times.
type FakeClock struct {
	t    *testing.T
	base time.Time
	now  time.Time
}

// NewFakeClock creates a new fake clock, with the initial time set to an unspecified, round number.
func NewFakeClock(t *testing.T) *FakeClock {
	base, err := time.Parse(time.RFC3339, "2000-01-01T00:00:00Z") // a nice round number, to make things easier
	if err != nil {
		panic(err)
	}

	return &FakeClock{t: t, base: base, now: base}
}

// Now returns the current time of the clock
func (c *FakeClock) Now() time.Time {
	return c.now
}

// Elapsed returns the total time added (via Inc) since the clock was started
func (c *FakeClock) Elapsed() Elapsed {
	return Elapsed{c.t, c.now.Sub(c.base)}
}

// Inc adds duration to the current time of the clock
func (c *FakeClock) Inc(duration time.Duration) Elapsed {
	if duration < 0 {
		panic(fmt.Errorf("(*FakeClock).Inc() called with negative duration %s", duration))
	}
	c.now = c.now.Add(duration)
	return c.Elapsed()
}

type Elapsed struct {
	t *testing.T
	time.Duration
}

func (e Elapsed) AssertEquals(expected time.Duration) {
	require.Equal(e.t, expected, e.Duration)
}
