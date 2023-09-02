package agent_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/neondatabase/autoscaling/pkg/agent"
	"github.com/neondatabase/autoscaling/pkg/api"
)

func Test_desiredVMState(t *testing.T) {
	cases := []struct {
		name string

		// helpers for setting fields of atomicUpdateState:
		metrics          api.Metrics
		vmUsing          api.Resources
		lastApproved     api.Resources
		requestedUpscale api.MoreResources

		// expected output from (*atomicUpdateState).desiredVMState(allowDecrease)
		expected      api.Resources
		allowDecrease bool
	}{
		{
			name: "BasicScaleup",
			metrics: api.Metrics{
				LoadAverage1Min:  0.30,
				LoadAverage5Min:  0.0, // unused
				MemoryUsageBytes: 0.0,
			},
			vmUsing:          api.Resources{VCPU: 250, Mem: 1},
			lastApproved:     api.Resources{VCPU: 0, Mem: 0}, // unused
			requestedUpscale: api.MoreResources{Cpu: false, Memory: false},

			expected:      api.Resources{VCPU: 500, Mem: 2},
			allowDecrease: true,
		},
	}

	for _, c := range cases {
		state := agent.AtomicUpdateState{
			ComputeUnit:      api.Resources{VCPU: 250, Mem: 1},
			Metrics:          c.metrics,
			LastApproved:     c.lastApproved,
			RequestedUpscale: c.requestedUpscale,
			Config: api.ScalingConfig{
				LoadAverageFractionTarget: 0.5,
				MemoryUsageFractionTarget: 0.5,
			},
			VM: api.VmInfo{
				Name:      "test",
				Namespace: "test",
				Cpu: api.VmCpuInfo{
					Min: 250,
					Use: c.vmUsing.VCPU,
					Max: 1000,
				},
				Mem: api.VmMemInfo{
					SlotSize: resource.NewQuantity(1<<30 /* 1 Gi */, resource.BinarySI), // unused, doesn't actually matter.
					Min:      1,
					Use:      c.vmUsing.Mem,
					Max:      4,
				},
				// remaining fields are also unused:
				ScalingConfig:  nil,
				AlwaysMigrate:  false,
				ScalingEnabled: true,
			},
		}

		t.Run(c.name, func(t *testing.T) {
			actual := state.DesiredVMState(c.allowDecrease)
			if actual != c.expected {
				t.Errorf("expected output %+v but got %+v", c.expected, actual)
			}
		})
	}
}
