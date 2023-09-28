package core_test

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
)

func Test_desiredVMState(t *testing.T) {
	cases := []struct {
		name string

		// helpers for setting fields (ish) of State:
		metrics          api.Metrics
		vmUsing          api.Resources
		requestedUpscale api.MoreResources

		// expected output from (*State).DesiredResourcesFromMetricsOrRequestedUpscaling()
		expected api.Resources
	}{
		{
			name: "BasicScaleup",
			metrics: api.Metrics{
				LoadAverage1Min:  0.30,
				LoadAverage5Min:  0.0, // unused
				MemoryUsageBytes: 0.0,
			},
			vmUsing:          api.Resources{VCPU: 250, Mem: 1},
			requestedUpscale: api.MoreResources{Cpu: false, Memory: false},

			expected: api.Resources{VCPU: 500, Mem: 2},
		},
	}

	for _, c := range cases {
		state := core.NewState(
			api.VmInfo{
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
			core.Config{
				DefaultScalingConfig: api.ScalingConfig{
					LoadAverageFractionTarget: 0.5,
					MemoryUsageFractionTarget: 0.5,
				},
				// these don't really matter, because we're not using (*State).NextActions()
				PluginRequestTick:              time.Second,
				MonitorDeniedDownscaleCooldown: time.Second,
				MonitorRetryWait:               time.Second,
				Warn:                           nil,
			},
		)

		// set the metrics
		state.UpdateMetrics(c.metrics)

		t.Run(c.name, func(t *testing.T) {
			actual := state.DesiredResourcesFromMetricsOrRequestedUpscaling()
			if actual != c.expected {
				t.Errorf("expected output %+v but got %+v", c.expected, actual)
			}
		})
	}
}
