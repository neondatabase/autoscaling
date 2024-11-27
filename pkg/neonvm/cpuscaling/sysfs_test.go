package cpuscaling

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseCPURange(t *testing.T) {
	sysFsState := cpuSysfsState{}

	// Parsing range
	start, end, err := sysFsState.parseCPURange("0-1")
	assert.NoError(t, err)
	assert.Equal(t, []int{0, 1}, []int{start, end})

	// Parsing range
	start, end, err = sysFsState.parseCPURange("0-5")
	assert.NoError(t, err)
	assert.Equal(t, []int{0, 5}, []int{start, end})

	// Parsing single CPU
	start, end, err = sysFsState.parseCPURange("0")
	assert.NoError(t, err)
	assert.Equal(t, []int{0, 0}, []int{start, end})

	// Parsing single CPU
	start, end, err = sysFsState.parseCPURange("1")
	assert.NoError(t, err)
	assert.Equal(t, []int{1, 1}, []int{start, end})
}

func TestParseMultipleCPURange(t *testing.T) {
	sysFsState := cpuSysfsState{}

	// Parsing multiple range
	cpus, err := sysFsState.parseMultipleCPURange("0-1,3-5, 9")
	assert.NoError(t, err)
	assert.Equal(t, []int{0, 1, 3, 4, 5, 9}, cpus)

	// Parsing single CPU
	cpus, err = sysFsState.parseMultipleCPURange("0,1,2,3")
	assert.NoError(t, err)
	assert.Equal(t, []int{0, 1, 2, 3}, cpus)
}
