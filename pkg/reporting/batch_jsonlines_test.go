package reporting_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/neondatabase/autoscaling/pkg/reporting"
)

func TestJSONLinesBuilder(t *testing.T) {
	type testType struct {
		X int
		Y int
	}

	// check the exact output of the JSONArrayBatcher
	cases := []struct {
		name   string
		input  []testType
		output string
	}{
		{
			name:   "empty",
			input:  []testType{},
			output: ``,
		},
		{
			name: "single-element",
			input: []testType{
				{X: 1, Y: 2},
			},
			output: "{\"X\":1,\"Y\":2}\n",
		},
		{
			name: "many-elements",
			input: []testType{
				{X: 1, Y: 2},
				{X: 2, Y: 3},
				{X: 3, Y: 4},
				{X: 4, Y: 5},
			},
			output: "{\"X\":1,\"Y\":2}\n{\"X\":2,\"Y\":3}\n{\"X\":3,\"Y\":4}\n{\"X\":4,\"Y\":5}\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			builder := reporting.NewJSONLinesBuilder[testType](reporting.NewByteBuffer())

			for _, v := range c.input {
				builder.Add(v)
			}

			output := string(builder.Finish())
			assert.Equal(t, c.output, output)
		})
	}
}
