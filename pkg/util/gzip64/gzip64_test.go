package gzip64_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/neondatabase/autoscaling/pkg/util/gzip64"
)

func TestBasicUsage(t *testing.T) {
	exampleInput := []byte(strings.Repeat("test string! ", 100))
	assert.Equal(t, 1300, len(exampleInput))

	encoded := gzip64.Encode(exampleInput)
	// Compressed string should be smaller:
	assert.Less(t, len(encoded), len(exampleInput))

	decoded, err := gzip64.Decode(encoded)
	assert.NoError(t, err, "decode failed")

	// result after decompressing should give the original string:
	assert.Equal(t, exampleInput, decoded)
}
