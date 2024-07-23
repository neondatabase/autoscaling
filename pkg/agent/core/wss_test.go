package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
)

func TestEstimateTrueWorkingSetSize(t *testing.T) {
	cases := []struct {
		name     string
		cfg      core.WssEstimatorConfig
		series   []float64
		expected float64
	}{
		{
			name: "basic-plateau",
			cfg: core.WssEstimatorConfig{
				MaxAllowedIncreaseFactor: 2.0,
				InitialOffset:            9,
				WindowSize:               5,
			},
			series: []float64{
				0.1, 0.2, 0.3, 0.4, 0.5,
				0.6, 0.7, 0.8, 0.9, 1.0,
				1.0, 1.0, 1.0, 1.0, 1.0,
				1.1, 1.2, 1.3, 1.4, 1.5,
				1.6, 1.7, 1.8, 1.9, 2.0,
			},
			expected: 1.0,
		},
		{
			name: "plateau-before-init",
			cfg: core.WssEstimatorConfig{
				MaxAllowedIncreaseFactor: 2.0,
				InitialOffset:            9,
				WindowSize:               5,
			},
			series: []float64{
				0.1, 0.2, 0.3, 0.3, 0.3,
				0.3, 0.3, 0.4, 0.5, 0.6,
				0.7, 0.8, 0.9, 1.0, 1.1,
			},
			expected: 1.1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actual := core.EstimateTrueWorkingSetSize(c.series, c.cfg)
			assert.Equal(t, c.expected, actual)
		})
	}
}

func TestProjectNextHighest(t *testing.T) {
	cases := []struct {
		name       string
		projectLen float64
		series     []float64
		expected   float64
	}{
		{
			name:       "basic highest",
			projectLen: 0.5,
			series: []float64{
				0.5, 0.6, 0.7, 0.8, 0.9,
				1.0, 1.0, 1.0, 1.0, 1.0,
			},
			expected: 1.05, // 1.0 + 0.5 × (1.0 - 0.9)
		},
		{
			name:       "second highest from bigger slope but not biggest slope",
			projectLen: 2.0,
			series: []float64{
				0.0, 0.3, 0.5, 0.6,
				// projected:
				//   0.6, 0.9, 0.8, --
			},
			expected: 0.9,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actual := core.ProjectNextHighest(c.series, c.projectLen)
			assert.Equal(t, c.expected, actual)
		})
	}
}
