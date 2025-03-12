package reporting_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/neondatabase/autoscaling/pkg/reporting"
)

func TestJSONArrayBuilder(t *testing.T) {
	type testType struct {
		X int
		Y int
	}

	// check the exact output of the JSONArrayBatcher
	cases := []struct {
		name         string
		nestedFields []string
		input        []testType
		output       string
	}{
		{
			name:         "empty",
			nestedFields: nil,
			input:        []testType{},
			output:       `[]`,
		},
		{
			name:         "single-element",
			nestedFields: nil,
			input: []testType{
				{X: 1, Y: 2},
			},
			output: `[{"X":1,"Y":2}]`,
		},
		{
			name:         "many-elements",
			nestedFields: nil,
			input: []testType{
				{X: 1, Y: 2},
				{X: 2, Y: 3},
				{X: 3, Y: 4},
				{X: 4, Y: 5},
			},
			output: `[{"X":1,"Y":2},{"X":2,"Y":3},{"X":3,"Y":4},{"X":4,"Y":5}]`,
		},
		{
			name:         "some-nested-1",
			nestedFields: []string{"foo"},
			input: []testType{
				{X: 1, Y: 2},
				{X: 2, Y: 3},
			},
			output: `{"foo":[{"X":1,"Y":2},{"X":2,"Y":3}]}`,
		},
		{
			name:         "some-nested-2",
			nestedFields: []string{"foo", "bar"},
			input: []testType{
				{X: 1, Y: 2},
				{X: 2, Y: 3},
			},
			output: `{"foo":{"bar":[{"X":1,"Y":2},{"X":2,"Y":3}]}}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			builder := reporting.NewJSONArrayBuilder[testType](reporting.NewByteBuffer(), c.nestedFields...)

			for _, v := range c.input {
				builder.Add(v)
			}

			output := string(builder.Finish())
			assert.Equal(t, c.output, output)
		})
	}
}
