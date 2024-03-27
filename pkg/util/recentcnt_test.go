package util

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRecentCounter(t *testing.T) {
	ts := time.Date(2020, time.March, 1, 0, 0, 0, 0, time.UTC)

	rc := NewRecentCounter(4 * time.Second)
	assert.Equal(t, uint(0), rc.get(ts))

	rc.inc(ts)
	rc.inc(ts)
	assert.Equal(t, uint(2), rc.get(ts))

	rc.inc(ts.Add(2 * time.Second))
	assert.Equal(t, uint(3), rc.get(ts.Add(2*time.Second)))

	assert.Equal(t, uint(1), rc.get(ts.Add(5*time.Second)))

	assert.Equal(t, uint(0), rc.get(ts.Add(7*time.Second)))
}
