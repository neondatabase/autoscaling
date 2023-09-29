package core_test

import (
	"fmt"
	"testing"
	"time"

	"golang.org/x/exp/slices"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
)

func Test_DesiredResourcesFromMetricsOrRequestedUpscaling(t *testing.T) {
	cases := []struct {
		name string

		// helpers for setting fields (ish) of State:
		metrics           api.Metrics
		vmUsing           api.Resources
		schedulerApproved api.Resources
		requestedUpscale  api.MoreResources
		deniedDownscale   *api.Resources

		// expected output from (*State).DesiredResourcesFromMetricsOrRequestedUpscaling()
		expected api.Resources
		warnings []string
	}{
		{
			name: "BasicScaleup",
			metrics: api.Metrics{
				LoadAverage1Min:  0.30,
				LoadAverage5Min:  0.0, // unused
				MemoryUsageBytes: 0.0,
			},
			vmUsing:           api.Resources{VCPU: 250, Mem: 1},
			schedulerApproved: api.Resources{VCPU: 250, Mem: 1},
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   nil,

			expected: api.Resources{VCPU: 500, Mem: 2},
			warnings: nil,
		},
		{
			name: "MismatchedApprovedNoScaledown",
			metrics: api.Metrics{
				LoadAverage1Min:  0.0, // ordinarily would like to scale down
				LoadAverage5Min:  0.0,
				MemoryUsageBytes: 0.0,
			},
			vmUsing:           api.Resources{VCPU: 250, Mem: 2},
			schedulerApproved: api.Resources{VCPU: 250, Mem: 2},
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   &api.Resources{VCPU: 250, Mem: 1},

			// need to scale up because vmUsing is mismatched and otherwise we'd be scaling down.
			expected: api.Resources{VCPU: 500, Mem: 2},
			warnings: nil,
		},
		{
			// ref https://github.com/neondatabase/autoscaling/issues/512
			name: "MismatchedApprovedNoScaledownButVMAtMaximum",
			metrics: api.Metrics{
				LoadAverage1Min:  0.0, // ordinarily would like to scale down
				LoadAverage5Min:  0.0,
				MemoryUsageBytes: 0.0,
			},
			vmUsing:           api.Resources{VCPU: 1000, Mem: 5}, // note: mem greater than maximum. It can happen when scaling bounds change
			schedulerApproved: api.Resources{VCPU: 1000, Mem: 5}, // unused
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   &api.Resources{VCPU: 1000, Mem: 4},

			expected: api.Resources{VCPU: 1000, Mem: 5},
			warnings: []string{
				"Can't decrease desired resources to within VM maximum because of vm-monitor previously denied downscale request",
			},
		},
	}

	for _, c := range cases {
		warnings := []string{}

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
				Warn: func(format string, args ...any) {
					warnings = append(warnings, fmt.Sprintf(format, args...))
				},
			},
		)

		computeUnit := api.Resources{VCPU: 250, Mem: 1}

		t.Run(c.name, func(t *testing.T) {
			// set the metrics
			state.UpdateMetrics(c.metrics)

			now := time.Now()

			// set the compute unit and lastApproved by simulating a scheduler request/response
			state.Plugin().NewScheduler()
			state.Plugin().StartingRequest(now, c.schedulerApproved)
			err := state.Plugin().RequestSuccessful(now, api.PluginResponse{
				Permit:      c.schedulerApproved,
				Migrate:     nil,
				ComputeUnit: computeUnit,
			})
			if err != nil {
				t.Errorf("state.Plugin().RequestSuccessful() failed: %s", err)
				return
			}

			// set deniedDownscale (if needed) by simulating a vm-monitor request/response
			if c.deniedDownscale != nil {
				state.Monitor().Reset()
				state.Monitor().Active(true)
				state.Monitor().StartingDownscaleRequest(now)
				state.Monitor().DownscaleRequestDenied(now, c.vmUsing, *c.deniedDownscale)
			}

			actual := state.DesiredResourcesFromMetricsOrRequestedUpscaling(now)
			if actual != c.expected {
				t.Errorf("expected output %+v but got %+v", c.expected, actual)
			}

			if !slices.Equal(c.warnings, warnings) {
				t.Errorf("expected warnings %+v but got %+v", c.warnings, warnings)
			}
		})
	}
}
