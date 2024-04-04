package v1_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/api/resource"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

func TestSwapInfoBackwardsCompatibility(t *testing.T) {
	cases := []struct {
		input  string
		parsed vmv1.SwapInfo
	}{
		// Backwards compatibility test
		{
			input: `"1Gi"`,
			parsed: vmv1.SwapInfo{
				Size:       resource.MustParse("1Gi"),
				SkipSwapon: nil,
			},
		},
		// Simple test for current output format
		{
			input: `{"size": "3Gi", "skipSwapon": true}`,
			parsed: vmv1.SwapInfo{
				Size:       resource.MustParse("3Gi"),
				SkipSwapon: &[]bool{true}[0],
			},
		},
	}

	type wrapper struct {
		Swap *vmv1.SwapInfo `json:"swap,omitempty"`
	}

	for _, c := range cases {
		input := []byte(fmt.Sprint(`{"swap":`, c.input, `}`))
		var parsedWrapper wrapper
		if !assert.Nil(t, json.Unmarshal(input, &parsedWrapper), "unmarshaling failed for input: %s", input) {
			continue
		}

		assert.Equal(t, parsedWrapper.Swap, &c.parsed)
	}
}
