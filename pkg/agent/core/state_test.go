package core_test

import (
	"fmt"
	"testing"
	"time"

	"golang.org/x/exp/slices"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	helpers "github.com/neondatabase/autoscaling/pkg/agent/core/testhelpers"
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
				PluginDeniedRetryWait:          time.Second,
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
				state.Monitor().StartingDownscaleRequest(now, *c.deniedDownscale)
				state.Monitor().DownscaleRequestDenied(now)
			}

			actual, _ := state.DesiredResourcesFromMetricsOrRequestedUpscaling(now)
			if actual != c.expected {
				t.Errorf("expected output %+v but got %+v", c.expected, actual)
			}

			if !slices.Equal(c.warnings, warnings) {
				t.Errorf("expected warnings %+v but got %+v", c.warnings, warnings)
			}
		})
	}
}

var DefaultInitialStateConfig = helpers.InitialStateConfig{
	ComputeUnit:    api.Resources{VCPU: 250, Mem: 1},
	MemorySlotSize: resource.MustParse("1Gi"),

	MinCU: 1,
	MaxCU: 4,
	Core: core.Config{
		DefaultScalingConfig: api.ScalingConfig{
			LoadAverageFractionTarget: 0.5,
			MemoryUsageFractionTarget: 0.5,
		},
		PluginRequestTick:              5 * time.Second,
		PluginDeniedRetryWait:          2 * time.Second,
		MonitorDeniedDownscaleCooldown: 5 * time.Second,
		MonitorRetryWait:               3 * time.Second,
		Warn:                           func(string, ...any) {},
	},
}

// helper function to parse a duration
func duration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(fmt.Errorf("failed to parse duration: %w", err))
	}
	return d
}

func Test_NextActions(t *testing.T) {
	// Thorough checks of a relatively simple flow
	t.Run("BasicScaleupAndDownFlow", func(t *testing.T) {
		a := helpers.NewAssert(t)

		clock := helpers.NewFakeClock(t)
		state := helpers.CreateInitialState(
			DefaultInitialStateConfig,
			helpers.WithStoredWarnings(a.StoredWarnings()),
		)

		var actions core.ActionSet
		updateActions := func() core.ActionSet {
			actions = state.NextActions(clock.Now())
			return actions
		}

		clockTick := func() helpers.Elapsed {
			return clock.Inc(100 * time.Millisecond)
		}

		state.Plugin().NewScheduler()
		state.Monitor().Active(true)

		// Send initial scheduler request:
		a.WithWarnings("Can't determine desired resources because compute unit hasn't been set yet").
			Call(updateActions).
			Equals(core.ActionSet{
				PluginRequest: &core.ActionPluginRequest{
					LastPermit: nil,
					Target:     api.Resources{VCPU: 250, Mem: 1},
					Metrics:    nil,
				},
			})
		a.Do(state.Plugin().StartingRequest, clock.Now(), actions.PluginRequest.Target)
		clockTick().AssertEquals(duration("0.1s"))
		a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
			Permit:      actions.PluginRequest.Target,
			Migrate:     nil,
			ComputeUnit: api.Resources{VCPU: 250, Mem: 1},
		})

		clockTick().AssertEquals(duration("0.2s"))
		lastMetrics := api.Metrics{
			LoadAverage1Min:  0.3,
			LoadAverage5Min:  0.0, // unused
			MemoryUsageBytes: 0.0,
		}
		a.Do(state.UpdateMetrics, lastMetrics)
		// double-check that we agree about the desired resources
		a.Call(state.DesiredResourcesFromMetricsOrRequestedUpscaling, clock.Now()).
			Equals(api.Resources{VCPU: 500, Mem: 2}, helpers.Nil[*time.Duration]())

		// Now that the initial scheduler request is done, and we have metrics that indicate
		// scale-up would be a good idea, we should be contacting the scheduler to get approval.
		a.Call(updateActions).Equals(core.ActionSet{
			PluginRequest: &core.ActionPluginRequest{
				LastPermit: &api.Resources{VCPU: 250, Mem: 1},
				Target:     api.Resources{VCPU: 500, Mem: 2},
				Metrics:    &lastMetrics,
			},
		})
		// start the request:
		a.Do(state.Plugin().StartingRequest, clock.Now(), actions.PluginRequest.Target)
		clockTick().AssertEquals(duration("0.3s"))
		// should have nothing more to do; waiting on plugin request to come back
		a.Call(updateActions).Equals(core.ActionSet{})
		a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
			Permit:      api.Resources{VCPU: 500, Mem: 2},
			Migrate:     nil,
			ComputeUnit: api.Resources{VCPU: 250, Mem: 1},
		})

		// Scheduler approval is done, now we should be making the request to NeonVM
		a.Call(updateActions).Equals(core.ActionSet{
			// expected to make a scheduler request every 5s; it's been 100ms since the last one, so
			// if the NeonVM request didn't come back in time, we'd need to get woken up to start
			// the next scheduler request.
			Wait: &core.ActionWait{Duration: duration("4.9s")},
			NeonVMRequest: &core.ActionNeonVMRequest{
				Current: api.Resources{VCPU: 250, Mem: 1},
				Target:  api.Resources{VCPU: 500, Mem: 2},
			},
		})
		// start the request:
		a.Do(state.NeonVM().StartingRequest, clock.Now(), actions.NeonVMRequest.Target)
		clockTick().AssertEquals(duration("0.4s"))
		// should have nothing more to do; waiting on NeonVM request to come back
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("4.8s")},
		})
		a.Do(state.NeonVM().RequestSuccessful, clock.Now())

		// NeonVM change is done, now we should finish by notifying the vm-monitor
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("4.8s")}, // same as previous, clock hasn't changed
			MonitorUpscale: &core.ActionMonitorUpscale{
				Current: api.Resources{VCPU: 250, Mem: 1},
				Target:  api.Resources{VCPU: 500, Mem: 2},
			},
		})
		// start the request:
		a.Do(state.Monitor().StartingUpscaleRequest, clock.Now(), actions.MonitorUpscale.Target)
		clockTick().AssertEquals(duration("0.5s"))
		// should have nothing more to do; waiting on vm-monitor request to come back
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("4.7s")},
		})
		a.Do(state.Monitor().UpscaleRequestSuccessful, clock.Now())

		// And now, double-check that there's no sneaky follow-up actions before we change the
		// metrics
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("4.7s")}, // same as previous, clock hasn't changed
		})

		// ---- Scaledown !!! ----

		clockTick().AssertEquals(duration("0.6s"))

		// Set metrics back so that desired resources should now be zero
		lastMetrics = api.Metrics{
			LoadAverage1Min:  0.0,
			LoadAverage5Min:  0.0, // unused
			MemoryUsageBytes: 0.0,
		}
		a.Do(state.UpdateMetrics, lastMetrics)
		// double-check that we agree about the new desired resources
		a.Call(state.DesiredResourcesFromMetricsOrRequestedUpscaling, clock.Now()).
			Equals(api.Resources{VCPU: 250, Mem: 1}, helpers.Nil[*time.Duration]())

		// First step in downscaling is getting approval from the vm-monitor:
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("4.6s")},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current: api.Resources{VCPU: 500, Mem: 2},
				Target:  api.Resources{VCPU: 250, Mem: 1},
			},
		})
		a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), actions.MonitorDownscale.Target)
		clockTick().AssertEquals(duration("0.7s"))
		// should have nothing more to do; waiting on vm-monitor request to come back
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("4.5s")},
		})
		a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())

		// After getting approval from the vm-monitor, we make the request to NeonVM to carry it out
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("4.5s")}, // same as previous, clock hasn't changed
			NeonVMRequest: &core.ActionNeonVMRequest{
				Current: api.Resources{VCPU: 500, Mem: 2},
				Target:  api.Resources{VCPU: 250, Mem: 1},
			},
		})
		a.Do(state.NeonVM().StartingRequest, clock.Now(), actions.NeonVMRequest.Target)
		clockTick().AssertEquals(duration("0.8s"))
		// should have nothing more to do; waiting on NeonVM request to come back
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("4.4s")},
		})
		a.Do(state.NeonVM().RequestSuccessful, clock.Now())

		// Request to NeonVM completed, it's time to inform the scheduler plugin:
		a.Call(updateActions).Equals(core.ActionSet{
			PluginRequest: &core.ActionPluginRequest{
				LastPermit: &api.Resources{VCPU: 500, Mem: 2},
				Target:     api.Resources{VCPU: 250, Mem: 1},
				Metrics:    &lastMetrics,
			},
			// shouldn't have anything to say to the other components
		})
		a.Do(state.Plugin().StartingRequest, clock.Now(), actions.PluginRequest.Target)
		clockTick().AssertEquals(duration("0.9s"))
		// should have nothing more to do; waiting on plugin request to come back
		a.Call(updateActions).Equals(core.ActionSet{})
		a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
			Permit:      api.Resources{VCPU: 250, Mem: 1},
			Migrate:     nil,
			ComputeUnit: api.Resources{VCPU: 250, Mem: 1},
		})

		// Finally, check there's no leftover actions:
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("4.9s")}, // request that just finished was started 100ms ago
		})
	})
}
