package util_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/neondatabase/autoscaling/pkg/util"
)

func TestRecentCounter(t *testing.T) {
	rc := util.NewRecentCounter(4 * time.Second)
	assert.Equal(t, uint(0), rc.Get())

	rc.Inc()
	rc.Inc()
	assert.Equal(t, uint(2), rc.Get())

	time.Sleep(2 * time.Second)
	rc.Inc()
	assert.Equal(t, uint(3), rc.Get())

	time.Sleep(3 * time.Second)
	assert.Equal(t, uint(1), rc.Get())

	time.Sleep(2 * time.Second)
	assert.Equal(t, uint(0), rc.Get())
}
