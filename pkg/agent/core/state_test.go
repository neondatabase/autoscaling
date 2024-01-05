package core_test

import (
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/exp/slices"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	helpers "github.com/neondatabase/autoscaling/pkg/agent/core/testhelpers"
	"github.com/neondatabase/autoscaling/pkg/api"
)

func Test_DesiredResourcesFromMetricsOrRequestedUpscaling(t *testing.T) {
	slotSize := api.Bytes(1 << 30 /* 1 Gi */)

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
			vmUsing:           api.Resources{VCPU: 250, Mem: 1 * slotSize},
			schedulerApproved: api.Resources{VCPU: 250, Mem: 1 * slotSize},
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   nil,

			expected: api.Resources{VCPU: 500, Mem: 2 * slotSize},
			warnings: nil,
		},
		{
			name: "MismatchedApprovedNoScaledown",
			metrics: api.Metrics{
				LoadAverage1Min:  0.0, // ordinarily would like to scale down
				LoadAverage5Min:  0.0,
				MemoryUsageBytes: 0.0,
			},
			vmUsing:           api.Resources{VCPU: 250, Mem: 2 * slotSize},
			schedulerApproved: api.Resources{VCPU: 250, Mem: 2 * slotSize},
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   &api.Resources{VCPU: 250, Mem: 1 * slotSize},

			// need to scale up because vmUsing is mismatched and otherwise we'd be scaling down.
			expected: api.Resources{VCPU: 500, Mem: 2 * slotSize},
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
			vmUsing:           api.Resources{VCPU: 1000, Mem: 5 * slotSize}, // note: mem greater than maximum. It can happen when scaling bounds change
			schedulerApproved: api.Resources{VCPU: 1000, Mem: 5 * slotSize}, // unused
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   &api.Resources{VCPU: 1000, Mem: 4 * slotSize},

			expected: api.Resources{VCPU: 1000, Mem: 5 * slotSize},
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
					SlotSize: slotSize,
					Min:      1,
					Use:      uint16(c.vmUsing.Mem / slotSize),
					Max:      4,
				},
				// remaining fields are also unused:
				ScalingConfig:  nil,
				AlwaysMigrate:  false,
				ScalingEnabled: true,
			},
			core.Config{
				ComputeUnit: api.Resources{VCPU: 250, Mem: 1 * slotSize},
				DefaultScalingConfig: api.ScalingConfig{
					LoadAverageFractionTarget: 0.5,
					MemoryUsageFractionTarget: 0.5,
				},
				// these don't really matter, because we're not using (*State).NextActions()
				NeonVMRetryWait:                    time.Second,
				PluginRequestTick:                  time.Second,
				PluginRetryWait:                    time.Second,
				PluginDeniedRetryWait:              time.Second,
				MonitorDeniedDownscaleCooldown:     time.Second,
				MonitorRequestedUpscaleValidPeriod: time.Second,
				MonitorRetryWait:                   time.Second,
				Log: core.LogConfig{
					Info: nil,
					Warn: func(msg string, fields ...zap.Field) {
						warnings = append(warnings, msg)
					},
				},
			},
		)

		t.Run(c.name, func(t *testing.T) {
			// set the metrics
			state.UpdateMetrics(c.metrics)

			now := time.Now()

			// set lastApproved by simulating a scheduler request/response
			state.Plugin().StartingRequest(now, c.schedulerApproved)
			err := state.Plugin().RequestSuccessful(now, api.PluginResponse{
				Permit:      c.schedulerApproved,
				Migrate:     nil,
				ComputeUnit: nil,
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

var DefaultComputeUnit = api.Resources{VCPU: 250, Mem: 1 << 30 /* 1 Gi */}

var DefaultInitialStateConfig = helpers.InitialStateConfig{
	VM: helpers.InitialVmInfoConfig{
		ComputeUnit:    DefaultComputeUnit,
		MemorySlotSize: 1 << 30, /* 1 Gi */

		MinCU: 1,
		MaxCU: 4,
	},
	Core: core.Config{
		ComputeUnit: DefaultComputeUnit,
		DefaultScalingConfig: api.ScalingConfig{
			LoadAverageFractionTarget: 0.5,
			MemoryUsageFractionTarget: 0.5,
		},
		NeonVMRetryWait:                    5 * time.Second,
		PluginRequestTick:                  5 * time.Second,
		PluginRetryWait:                    3 * time.Second,
		PluginDeniedRetryWait:              2 * time.Second,
		MonitorDeniedDownscaleCooldown:     5 * time.Second,
		MonitorRequestedUpscaleValidPeriod: 10 * time.Second,
		MonitorRetryWait:                   3 * time.Second,
		Log: core.LogConfig{
			Info: nil,
			Warn: nil,
		},
	},
}

func getDesiredResources(state *core.State, now time.Time) api.Resources {
	res, _ := state.DesiredResourcesFromMetricsOrRequestedUpscaling(now)
	return res
}

func doInitialPluginRequest(
	a helpers.Assert,
	state *core.State,
	clock *helpers.FakeClock,
	requestTime time.Duration,
	metrics *api.Metrics,
	resources api.Resources,
) {
	a.Call(state.NextActions, clock.Now()).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: nil,
			Target:     resources,
			Metrics:    metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resources)
	clock.Inc(requestTime)
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resources,
		Migrate:     nil,
		ComputeUnit: nil,
	})
}

// helper function to parse a duration
func duration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(fmt.Errorf("failed to parse duration: %w", err))
	}
	return d
}

func ptr[T any](t T) *T {
	return &t
}

// Thorough checks of a relatively simple flow - scaling from 1 CU to 2 CU and back down.
func TestBasicScaleUpAndDownFlow(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)
	clockTick := func() helpers.Elapsed {
		return clock.Inc(100 * time.Millisecond)
	}
	resForCU := DefaultComputeUnit.Mul

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithTestingLogfWarnings(t),
	)
	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	state.Monitor().Active(true)

	// Send initial scheduler request:
	doInitialPluginRequest(a, state, clock, duration("0.1s"), nil, resForCU(1))

	// Set metrics
	clockTick().AssertEquals(duration("0.2s"))
	lastMetrics := api.Metrics{
		LoadAverage1Min:  0.3,
		LoadAverage5Min:  0.0, // unused
		MemoryUsageBytes: 0.0,
	}
	a.Do(state.UpdateMetrics, lastMetrics)
	// double-check that we agree about the desired resources
	a.Call(getDesiredResources, state, clock.Now()).
		Equals(resForCU(2))

	// Now that the initial scheduler request is done, and we have metrics that indicate
	// scale-up would be a good idea, we should be contacting the scheduler to get approval.
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(1)),
			Target:     resForCU(2),
			Metrics:    &lastMetrics,
		},
	})
	// start the request:
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	clockTick().AssertEquals(duration("0.3s"))
	// should have nothing more to do; waiting on plugin request to come back
	a.Call(nextActions).Equals(core.ActionSet{})
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(2),
		Migrate:     nil,
		ComputeUnit: nil,
	})

	// Scheduler approval is done, now we should be making the request to NeonVM
	a.Call(nextActions).Equals(core.ActionSet{
		// expected to make a scheduler request every 5s; it's been 100ms since the last one, so
		// if the NeonVM request didn't come back in time, we'd need to get woken up to start
		// the next scheduler request.
		Wait: &core.ActionWait{Duration: duration("4.9s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(1),
			Target:  resForCU(2),
		},
	})
	// start the request:
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
	clockTick().AssertEquals(duration("0.4s"))
	// should have nothing more to do; waiting on NeonVM request to come back
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")},
	})
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())

	// NeonVM change is done, now we should finish by notifying the vm-monitor
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")}, // same as previous, clock hasn't changed
		MonitorUpscale: &core.ActionMonitorUpscale{
			Current: resForCU(1),
			Target:  resForCU(2),
		},
	})
	// start the request:
	a.Do(state.Monitor().StartingUpscaleRequest, clock.Now(), resForCU(2))
	clockTick().AssertEquals(duration("0.5s"))
	// should have nothing more to do; waiting on vm-monitor request to come back
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.7s")},
	})
	a.Do(state.Monitor().UpscaleRequestSuccessful, clock.Now())

	// And now, double-check that there's no sneaky follow-up actions before we change the
	// metrics
	a.Call(nextActions).Equals(core.ActionSet{
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
	a.Call(getDesiredResources, state, clock.Now()).
		Equals(resForCU(1))

	// First step in downscaling is getting approval from the vm-monitor:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.6s")},
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(2),
			Target:  resForCU(1),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(1))
	clockTick().AssertEquals(duration("0.7s"))
	// should have nothing more to do; waiting on vm-monitor request to come back
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.5s")},
	})
	a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())

	// After getting approval from the vm-monitor, we make the request to NeonVM to carry it out
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.5s")}, // same as previous, clock hasn't changed
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(2),
			Target:  resForCU(1),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(1))
	clockTick().AssertEquals(duration("0.8s"))
	// should have nothing more to do; waiting on NeonVM request to come back
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.4s")},
	})
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())

	// Request to NeonVM completed, it's time to inform the scheduler plugin:
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(2)),
			Target:     resForCU(1),
			Metrics:    &lastMetrics,
		},
		// shouldn't have anything to say to the other components
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(1))
	clockTick().AssertEquals(duration("0.9s"))
	// should have nothing more to do; waiting on plugin request to come back
	a.Call(nextActions).Equals(core.ActionSet{})
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(1),
		Migrate:     nil,
		ComputeUnit: nil,
	})

	// Finally, check there's no leftover actions:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")}, // request that just finished was started 100ms ago
	})
}

// Test that in a stable state, requests to the plugin happen exactly every Config.PluginRequestTick
func TestPeriodicPluginRequest(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
	)

	state.Monitor().Active(true)

	metrics := api.Metrics{
		LoadAverage1Min:  0.0,
		LoadAverage5Min:  0.0,
		MemoryUsageBytes: 0.0,
	}
	resources := DefaultComputeUnit

	a.Do(state.UpdateMetrics, metrics)

	base := duration("0s")
	clock.Elapsed().AssertEquals(base)

	clockTick := duration("100ms")
	reqDuration := duration("50ms")
	reqEvery := DefaultInitialStateConfig.Core.PluginRequestTick
	endTime := duration("20s")

	doInitialPluginRequest(a, state, clock, clockTick, &metrics, resources)

	for clock.Elapsed().Duration < endTime {
		timeSinceScheduledRequest := (clock.Elapsed().Duration - base) % reqEvery

		if timeSinceScheduledRequest != 0 {
			timeUntilNextRequest := reqEvery - timeSinceScheduledRequest
			a.Call(state.NextActions, clock.Now()).Equals(core.ActionSet{
				Wait: &core.ActionWait{Duration: timeUntilNextRequest},
			})
			clock.Inc(clockTick)
		} else {
			a.Call(state.NextActions, clock.Now()).Equals(core.ActionSet{
				PluginRequest: &core.ActionPluginRequest{
					LastPermit: &resources,
					Target:     resources,
					Metrics:    &metrics,
				},
			})
			a.Do(state.Plugin().StartingRequest, clock.Now(), resources)
			a.Call(state.NextActions, clock.Now()).Equals(core.ActionSet{})
			clock.Inc(reqDuration)
			a.Call(state.NextActions, clock.Now()).Equals(core.ActionSet{})
			a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
				Permit:      resources,
				Migrate:     nil,
				ComputeUnit: nil,
			})
			clock.Inc(clockTick - reqDuration)
		}
	}
}

// Checks that when downscaling is denied, we both (a) try again with higher resources, or (b) wait
// to retry if there aren't higher resources to try with.
func TestDeniedDownscalingIncreaseAndRetry(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)
	clockTickDuration := duration("0.1s")
	clockTick := func() {
		clock.Inc(clockTickDuration)
	}
	resForCU := DefaultComputeUnit.Mul

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithMinMaxCU(1, 8),
		helpers.WithCurrentCU(6), // NOTE: Start at 6 CU, so we're trying to scale down immediately.
		helpers.WithConfigSetting(func(c *core.Config) {
			// values close to the default, so request timing works out a little better.
			c.PluginRequestTick = duration("7s")
			c.MonitorDeniedDownscaleCooldown = duration("4s")
		}),
	)

	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	state.Monitor().Active(true)

	doInitialPluginRequest(a, state, clock, duration("0.1s"), nil, resForCU(6))

	// Set metrics
	clockTick()
	metrics := api.Metrics{
		LoadAverage1Min:  0.0,
		LoadAverage5Min:  0.0,
		MemoryUsageBytes: 0.0,
	}
	a.Do(state.UpdateMetrics, metrics)
	// double-check that we agree about the desired resources
	a.Call(getDesiredResources, state, clock.Now()).
		Equals(resForCU(1))

	// Broadly the idea here is that we should be trying to request downscaling from the vm-monitor,
	// and retrying with progressively higher values until either we get approved, or we run out of
	// options, at which point we should wait until later to re-request downscaling.
	//
	// This behavior results in linear retry passes.
	//
	// For this test, we:
	// 1. Deny any request in the first pass
	// 2. Approve only down to 3 CU on the second pass
	//    a. triggers NeonVM request
	//    b. triggers plugin request
	// 3. Deny all requests in the third pass (i.e. stay at 3 CU)
	// 4. Approve down to 1 CU on the fourth pass
	//    a. triggers NeonVM request
	//    b. triggers plugin request
	//
	// ----
	//
	// First pass: deny downscaling.
	clock.Elapsed()

	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("6.8s")},
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(6),
			Target:  resForCU(5),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(5))
	clockTick()
	a.Do(state.Monitor().DownscaleRequestDenied, clock.Now())

	// At the end, we should be waiting to retry downscaling:
	a.Call(nextActions).Equals(core.ActionSet{
		// Taken from DefaultInitialStateConfig.Core.MonitorDeniedDownscaleCooldown
		Wait: &core.ActionWait{Duration: duration("4.0s")},
	})

	clock.Inc(duration("4s"))
	currentPluginWait := duration("2.7s")

	// Second pass: Approve only down to 3 CU, then NeonVM & plugin requests.
	for cu := uint16(5); cu >= 2; cu -= 1 {
		var expectedNeonVMRequest *core.ActionNeonVMRequest
		if cu < 5 {
			expectedNeonVMRequest = &core.ActionNeonVMRequest{
				Current: resForCU(6),
				Target:  resForCU(cu + 1),
			}
		}

		a.Call(nextActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current: resForCU(cu + 1),
				Target:  resForCU(cu),
			},
			NeonVMRequest: expectedNeonVMRequest,
		})
		a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(cu))
		a.Call(nextActions).Equals(core.ActionSet{
			Wait:          &core.ActionWait{Duration: currentPluginWait},
			NeonVMRequest: expectedNeonVMRequest,
		})
		clockTick()
		currentPluginWait -= clockTickDuration
		if cu >= 3 /* allow down to 3 */ {
			a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())
		} else {
			a.Do(state.Monitor().DownscaleRequestDenied, clock.Now())
		}
	}
	// At this point, waiting 3.7s for next attempt to downscale below 3 CU (last request was
	// successful, but the one before it wasn't), and 0.8s for plugin tick.
	// Also, because downscaling was approved, we should want to make a NeonVM request to do that.
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("2.3s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(6),
			Target:  resForCU(3),
		},
	})
	// Make the request:
	a.Do(state.NeonVM().StartingRequest, time.Now(), resForCU(3))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("2.3s")},
	})
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, time.Now())
	// Successfully scaled down, so we should now inform the plugin. But also, we'll want to retry
	// the downscale request to vm-monitor once the retry is up:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.9s")},
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(6)),
			Target:     resForCU(3),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.9s")},
	})
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(3),
		Migrate:     nil,
		ComputeUnit: nil,
	})
	// ... And *now* there's nothing left to do but wait until downscale wait expires:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.8s")},
	})

	// so, wait for that:
	clock.Inc(duration("3.8s"))

	// Third pass: deny requested downscaling.
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.1s")},
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(3),
			Target:  resForCU(2),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Do(state.Monitor().DownscaleRequestDenied, clock.Now())
	// At the end, we should be waiting to retry downscaling (but actually, the regular plugin
	// request is coming up sooner).
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.0s")},
	})
	// ... so, wait for that plugin request/response, and then wait to retry downscaling:
	clock.Inc(duration("3s"))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1s")}, // still want to retry vm-monitor downscaling
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(3)),
			Target:     resForCU(3),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1s")}, // still waiting on retrying vm-monitor downscaling
	})
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(3),
		Migrate:     nil,
		ComputeUnit: nil,
	})
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("0.9s")}, // yep, still waiting on retrying vm-monitor downscaling
	})

	clock.Inc(duration("0.9s"))

	// Fourth pass: approve down to 1 CU - wait to do the NeonVM requests until the end
	currentPluginWait = duration("6.0s")
	for cu := uint16(2); cu >= 1; cu -= 1 {
		var expectedNeonVMRequest *core.ActionNeonVMRequest
		if cu < 2 {
			expectedNeonVMRequest = &core.ActionNeonVMRequest{
				Current: resForCU(3),
				Target:  resForCU(cu + 1),
			}
		}

		a.Call(nextActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current: resForCU(cu + 1),
				Target:  resForCU(cu),
			},
			NeonVMRequest: expectedNeonVMRequest,
		})
		a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(cu))
		a.Call(nextActions).Equals(core.ActionSet{
			Wait:          &core.ActionWait{Duration: currentPluginWait},
			NeonVMRequest: expectedNeonVMRequest,
		})
		clockTick()
		currentPluginWait -= clockTickDuration
		a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())
	}
	// Still waiting on plugin request tick, but we can make a NeonVM request to enact the
	// downscaling right away !
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("5.8s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(3),
			Target:  resForCU(1),
		},
	})
	a.Do(state.NeonVM().StartingRequest, time.Now(), resForCU(1))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("5.8s")}, // yep, still waiting on the plugin
	})
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, time.Now())
	// Successfully downscaled, so now we should inform the plugin. Not waiting on any retries.
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(3)),
			Target:     resForCU(1),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(1))
	a.Call(nextActions).Equals(core.ActionSet{
		// not waiting on anything!
	})
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(1),
		Migrate:     nil,
		ComputeUnit: nil,
	})
	// And now there's truly nothing left to do. Back to waiting on plugin request tick :)
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("6.9s")},
	})
}

// Checks that we scale up in a timely manner when the vm-monitor requests it, and don't request
// downscaling until the time expires.
func TestRequestedUpscale(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)
	clockTick := func() {
		clock.Inc(100 * time.Millisecond)
	}
	resForCU := DefaultComputeUnit.Mul

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithConfigSetting(func(c *core.Config) {
			c.MonitorRequestedUpscaleValidPeriod = duration("6s") // Override this for consistency
		}),
	)
	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	state.Monitor().Active(true)

	// Send initial scheduler request:
	doInitialPluginRequest(a, state, clock, duration("0.1s"), nil, resForCU(1))

	// Set metrics
	clockTick()
	lastMetrics := api.Metrics{
		LoadAverage1Min:  0.0,
		LoadAverage5Min:  0.0, // unused
		MemoryUsageBytes: 0.0,
	}
	a.Do(state.UpdateMetrics, lastMetrics)

	// Check we're not supposed to do anything
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")},
	})

	// Have the vm-monitor request upscaling:
	a.Do(state.Monitor().UpscaleRequested, clock.Now(), api.MoreResources{Cpu: false, Memory: true})
	// First need to check with the scheduler plugin to get approval for upscaling:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("6s")}, // if nothing else happens, requested upscale expires.
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(1)),
			Target:     resForCU(2),
			Metrics:    &lastMetrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("5.9s")}, // same waiting for requested upscale expiring
	})
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(2),
		Migrate:     nil,
		ComputeUnit: nil,
	})

	// After approval from the scheduler plugin, now need to make NeonVM request:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")}, // plugin tick wait is earlier than requested upscale expiration
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(1),
			Target:  resForCU(2),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())

	// Finally, tell the vm-monitor that it got upscaled:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")}, // still waiting on plugin tick
		MonitorUpscale: &core.ActionMonitorUpscale{
			Current: resForCU(1),
			Target:  resForCU(2),
		},
	})
	a.Do(state.Monitor().StartingUpscaleRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Do(state.Monitor().UpscaleRequestSuccessful, clock.Now())

	// After everything, we should be waiting on both:
	// (a) scheduler plugin tick (4.7s remaining), and
	// (b) vm-monitor requested upscaling expiring (5.7s remaining)
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.7s")},
	})

	// Do the routine scheduler plugin request. Still waiting 1s for vm-monitor request expiration
	clock.Inc(duration("4.7s"))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1s")},
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(2)),
			Target:     resForCU(2),
			Metrics:    &lastMetrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("0.9s")}, // waiting for requested upscale expiring
	})
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(2),
		Migrate:     nil,
		ComputeUnit: nil,
	})

	// Still should just be waiting on vm-monitor upscale expiring
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("0.9s")},
	})
	clock.Inc(duration("0.9s"))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4s")}, // now, waiting on plugin request tick
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(2),
			Target:  resForCU(1),
		},
	})
}

// Checks that if we get new metrics partway through downscaling, then we pivot back to upscaling
// without further requests in furtherance of downscaling.
//
// For example, if we pivot during the NeonVM request to do the downscaling, then the request to to
// the scheduler plugin should never be made, because we decided against downscaling.
func TestDownscalePivotBack(t *testing.T) {
	a := helpers.NewAssert(t)
	var clock *helpers.FakeClock

	clockTickDuration := duration("0.1s")
	clockTick := func() helpers.Elapsed {
		return clock.Inc(clockTickDuration)
	}
	halfClockTick := func() helpers.Elapsed {
		return clock.Inc(clockTickDuration / 2)
	}
	resForCU := DefaultComputeUnit.Mul

	var state *core.State
	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	initialMetrics := api.Metrics{
		LoadAverage1Min:  0.0,
		LoadAverage5Min:  0.0,
		MemoryUsageBytes: 0.0,
	}
	newMetrics := api.Metrics{
		LoadAverage1Min:  0.3,
		LoadAverage5Min:  0.0,
		MemoryUsageBytes: 0.0,
	}

	steps := []struct {
		pre  func(pluginWait *time.Duration, midRequest func())
		post func(pluginWait *time.Duration)
	}{
		// vm-monitor requests:
		{
			pre: func(pluginWait *time.Duration, midRequest func()) {
				t.Log(" > start vm-monitor downscale")
				a.Call(nextActions).Equals(core.ActionSet{
					Wait: &core.ActionWait{Duration: *pluginWait},
					MonitorDownscale: &core.ActionMonitorDownscale{
						Current: resForCU(2),
						Target:  resForCU(1),
					},
				})
				a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(1))
				halfClockTick()
				midRequest()
				halfClockTick()
				*pluginWait -= clockTickDuration
				t.Log(" > finish vm-monitor downscale")
				a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())
			},
			post: func(pluginWait *time.Duration) {
				t.Log(" > start vm-monitor upscale")
				a.Call(nextActions).Equals(core.ActionSet{
					Wait: &core.ActionWait{Duration: *pluginWait},
					MonitorUpscale: &core.ActionMonitorUpscale{
						Current: resForCU(1),
						Target:  resForCU(2),
					},
				})
				a.Do(state.Monitor().StartingUpscaleRequest, clock.Now(), resForCU(2))
				clockTick()
				*pluginWait -= clockTickDuration
				t.Log(" > finish vm-monitor upscale")
				a.Do(state.Monitor().UpscaleRequestSuccessful, clock.Now())
			},
		},
		// NeonVM requests
		{
			pre: func(pluginWait *time.Duration, midRequest func()) {
				t.Log(" > start NeonVM downscale")
				a.Call(nextActions).Equals(core.ActionSet{
					Wait: &core.ActionWait{Duration: *pluginWait},
					NeonVMRequest: &core.ActionNeonVMRequest{
						Current: resForCU(2),
						Target:  resForCU(1),
					},
				})
				a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(1))
				halfClockTick()
				midRequest()
				halfClockTick()
				*pluginWait -= clockTickDuration
				t.Log(" > finish NeonVM downscale")
				a.Do(state.NeonVM().RequestSuccessful, clock.Now())
			},
			post: func(pluginWait *time.Duration) {
				t.Log(" > start NeonVM upscale")
				a.Call(nextActions).Equals(core.ActionSet{
					Wait: &core.ActionWait{Duration: *pluginWait},
					NeonVMRequest: &core.ActionNeonVMRequest{
						Current: resForCU(1),
						Target:  resForCU(2),
					},
				})
				a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
				clockTick()
				*pluginWait -= clockTickDuration
				t.Log(" > finish NeonVM upscale")
				a.Do(state.NeonVM().RequestSuccessful, clock.Now())
			},
		},
		// plugin requests
		{
			pre: func(pluginWait *time.Duration, midRequest func()) {
				t.Log(" > start plugin downscale")
				a.Call(nextActions).Equals(core.ActionSet{
					PluginRequest: &core.ActionPluginRequest{
						LastPermit: ptr(resForCU(2)),
						Target:     resForCU(1),
						Metrics:    &initialMetrics,
					},
				})
				a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(1))
				halfClockTick()
				midRequest()
				halfClockTick()
				*pluginWait = duration("4.9s") // reset because we just made a request
				t.Log(" > finish plugin downscale")
				a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
					Permit:      resForCU(1),
					Migrate:     nil,
					ComputeUnit: nil,
				})
			},
			post: func(pluginWait *time.Duration) {
				t.Log(" > start plugin upscale")
				a.Call(nextActions).Equals(core.ActionSet{
					PluginRequest: &core.ActionPluginRequest{
						LastPermit: ptr(resForCU(1)),
						Target:     resForCU(2),
						Metrics:    &newMetrics,
					},
				})
				a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
				clockTick()
				*pluginWait = duration("4.9s") // reset because we just made a request
				t.Log(" > finish plugin upscale")
				a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
					Permit:      resForCU(2),
					Migrate:     nil,
					ComputeUnit: nil,
				})
			},
		},
	}

	for i := 0; i < len(steps); i++ {
		t.Logf("iter(%d)", i)

		// Initial setup
		clock = helpers.NewFakeClock(t)
		state = helpers.CreateInitialState(
			DefaultInitialStateConfig,
			helpers.WithStoredWarnings(a.StoredWarnings()),
			helpers.WithMinMaxCU(1, 3),
			helpers.WithCurrentCU(2),
		)

		state.Monitor().Active(true)

		doInitialPluginRequest(a, state, clock, duration("0.1s"), nil, resForCU(2))

		clockTick().AssertEquals(duration("0.2s"))
		pluginWait := duration("4.8s")

		a.Do(state.UpdateMetrics, initialMetrics)
		// double-check that we agree about the desired resources
		a.Call(getDesiredResources, state, clock.Now()).
			Equals(resForCU(1))

		for j := 0; j <= i; j++ {
			midRequest := func() {}
			if j == i {
				// at the midpoint, start backtracking by setting the metrics
				midRequest = func() {
					t.Log(" > > updating metrics mid-request")
					a.Do(state.UpdateMetrics, newMetrics)
					a.Call(getDesiredResources, state, clock.Now()).
						Equals(resForCU(2))
				}
			}

			steps[j].pre(&pluginWait, midRequest)
		}

		for j := i; j >= 0; j-- {
			steps[j].post(&pluginWait)
		}
	}
}

// Checks that if the VM's min/max bounds change so that the maximum is below the current and
// desired usage, we try to downscale
func TestBoundsChangeRequiresDownsale(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)
	clockTick := func() {
		clock.Inc(100 * time.Millisecond)
	}
	resForCU := DefaultComputeUnit.Mul

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithMinMaxCU(1, 3),
		helpers.WithCurrentCU(2),
	)
	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	state.Monitor().Active(true)

	// Send initial scheduler request:
	doInitialPluginRequest(a, state, clock, duration("0.1s"), nil, resForCU(2))

	clockTick()

	// Set metrics so the desired resources are still 2 CU
	metrics := api.Metrics{
		LoadAverage1Min:  0.3,
		LoadAverage5Min:  0.0,
		MemoryUsageBytes: 0.0,
	}
	a.Do(state.UpdateMetrics, metrics)
	// Check that we agree about desired resources
	a.Call(getDesiredResources, state, clock.Now()).
		Equals(resForCU(2))
	// Check we've got nothing to do yet
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")},
	})

	clockTick()

	// Update the VM to set min=max=1 CU
	a.Do(state.UpdatedVM, helpers.CreateVmInfo(
		DefaultInitialStateConfig.VM,
		helpers.WithCurrentCU(2),
		helpers.WithMinMaxCU(1, 1),
	))

	// We should be making a vm-monitor downscaling request
	// TODO: In the future, we should have a "force-downscale" alternative so the vm-monitor doesn't
	// get to deny the downscaling.
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.7s")},
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(2),
			Target:  resForCU(1),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(1))
	clockTick()
	a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())
	// Do NeonVM request for that downscaling
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.6s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(2),
			Target:  resForCU(1),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(1))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())
	// Do plugin request for that downscaling:
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(2)),
			Target:     resForCU(1),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(1))
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(1),
		Migrate:     nil,
		ComputeUnit: nil,
	})
	// And then, we shouldn't need to do anything else:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")},
	})
}

// Checks that if the VM's min/max bounds change so that the minimum is above the current and
// desired usage, we try to upscale
func TestBoundsChangeRequiresUpscale(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)
	clockTick := func() {
		clock.Inc(100 * time.Millisecond)
	}
	resForCU := DefaultComputeUnit.Mul

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithMinMaxCU(1, 3),
		helpers.WithCurrentCU(2),
	)
	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	state.Monitor().Active(true)

	// Send initial scheduler request:
	doInitialPluginRequest(a, state, clock, duration("0.1s"), nil, resForCU(2))

	clockTick()

	// Set metrics so the desired resources are still 2 CU
	metrics := api.Metrics{
		LoadAverage1Min:  0.3,
		LoadAverage5Min:  0.0,
		MemoryUsageBytes: 0.0,
	}
	a.Do(state.UpdateMetrics, metrics)
	// Check that we agree about desired resources
	a.Call(getDesiredResources, state, clock.Now()).
		Equals(resForCU(2))
	// Check we've got nothing to do yet
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")},
	})

	clockTick()

	// Update the VM to set min=max=3 CU
	a.Do(state.UpdatedVM, helpers.CreateVmInfo(
		DefaultInitialStateConfig.VM,
		helpers.WithCurrentCU(2),
		helpers.WithMinMaxCU(3, 3),
	))

	// We should be making a plugin request to get upscaling:
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(2)),
			Target:     resForCU(3),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(3),
		Migrate:     nil,
		ComputeUnit: nil,
	})
	// Do NeonVM request for the upscaling
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(2),
			Target:  resForCU(3),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(3))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())
	// Do vm-monitor upscale request
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")},
		MonitorUpscale: &core.ActionMonitorUpscale{
			Current: resForCU(2),
			Target:  resForCU(3),
		},
	})
	a.Do(state.Monitor().StartingUpscaleRequest, clock.Now(), resForCU(3))
	clockTick()
	a.Do(state.Monitor().UpscaleRequestSuccessful, clock.Now())
	// And then, we shouldn't need to do anything else:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.7s")},
	})
}

// Checks that failed requests to the scheduler plugin and NeonVM API will be retried after a delay
func TestFailedRequestRetry(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)
	clockTick := func() {
		clock.Inc(100 * time.Millisecond)
	}
	resForCU := DefaultComputeUnit.Mul

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithMinMaxCU(1, 2),
		helpers.WithCurrentCU(1),
		helpers.WithConfigSetting(func(c *core.Config) {
			// Override values for consistency and ease of use
			c.PluginRetryWait = duration("2s")
			c.NeonVMRetryWait = duration("3s")
			c.MonitorRetryWait = duration("1s")
		}),
	)
	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	state.Monitor().Active(true)

	// Send initial scheduler request
	doInitialPluginRequest(a, state, clock, duration("0.1s"), nil, resForCU(1))

	// Set metrics so that we should be trying to upscale
	clockTick()
	metrics := api.Metrics{
		LoadAverage1Min:  0.3,
		LoadAverage5Min:  0.0, // unused
		MemoryUsageBytes: 0.0,
	}
	a.Do(state.UpdateMetrics, metrics)

	// We should be asking the scheduler for upscaling
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(1)),
			Target:     resForCU(2),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	// On request failure, we retry after Config.PluginRetryWait
	a.Do(state.Plugin().RequestFailed, clock.Now())
	a.
		WithWarnings("Wanted to make a request to the scheduler plugin, but previous request failed too recently").
		Call(nextActions).
		Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("2s")},
		})
	clock.Inc(duration("2s"))
	// ... and then retry:
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(1)),
			Target:     resForCU(2),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(2),
		Migrate:     nil,
		ComputeUnit: nil,
	})

	// Now, after plugin request is successful, we should be making a request to NeonVM.
	// We'll have that request fail the first time as well:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")}, // plugin request tick
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(1),
			Target:  resForCU(2),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	// On request failure, we retry after Config.NeonVMRetryWait
	a.Do(state.NeonVM().RequestFailed, clock.Now())
	a.
		WithWarnings("Wanted to make a request to NeonVM API, but recent request failed too recently").
		Call(nextActions).
		Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("3s")}, // NeonVM retry wait is less than current plugin request tick (4.8s remaining)
		})
	clock.Inc(duration("3s"))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1.8s")}, // plugin request tick
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(1),
			Target:  resForCU(2),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())

	// And then finally, we should be looking to inform the vm-monitor about this upscaling.
	// Let's have that fail the first time as well.
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1.7s")}, // plugin request tick
		MonitorUpscale: &core.ActionMonitorUpscale{
			Current: resForCU(1),
			Target:  resForCU(2),
		},
	})
	a.Do(state.Monitor().StartingUpscaleRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Do(state.Monitor().UpscaleRequestFailed, clock.Now())
	// On request failure, we retry after Config.MonitorRetryWait
	a.
		WithWarnings("Wanted to send vm-monitor upscale request, but failed too recently").
		Call(nextActions).
		Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("1s")}, // monitor retry wait is less than current plugin request tick (1.6s remaining)
		})
	clock.Inc(duration("1s"))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("0.6s")},
		MonitorUpscale: &core.ActionMonitorUpscale{
			Current: resForCU(1),
			Target:  resForCU(2),
		},
	})
	a.Do(state.Monitor().StartingUpscaleRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Do(state.Monitor().UpscaleRequestSuccessful, clock.Now())
}

// Checks that failed requests are not assumed to have *not* taken effect, e.g. if a downscaling
// request to the scheduler fails, we must assume that it *could have* succeeded.
//
// See also: https://github.com/neondatabase/autoscaling/issues/680
func TestFailuresNotAssumedSuccessful(t *testing.T) {
	// General strategy:
	// At a high level, we're scaling up from 1->2 CU, then down from 2->1 CU.
	// At each step, we'll have *some* kind of request failure that causes a delay in continuing the
	// scaling, while *also* preventing reverting the scaling until the failure is resolved.
	// So, immediately after the request failure, we update the metrics so we *would* scale the
	// other direction - and check that nothing is attempted.
	//
	// To double-check, we *also* check that we *would* have been able to reverse if the request
	// hadn't failed, by doing that first.
	//
	// ---
	//
	// Here's the process for scale-up:
	// 1. Update metrics so desired is 2 CU
	// 2. Plugin request (1->2)
	//   2a. Do request (1->2)
	//   2b. Succeed
	//   2c. Update metrics so desired is 1 CU
	//   2d. Do request (2->1)
	//   2e. Succeed
	//   2f. Update metrics so desired is 2 CU
	//   2g. Do request (1->2)
	//   2h. Fail
	//   2i. Update metrics so desired is 1 CU
	//   2j. Check: We *would* need to make explicit request back down
	//   2k. Update metrics so desired is 2 CU
	//   2l. Do request (1->2)
	//   2m. Succeed
	// 3. NeonVM request (1->2)
	//   3a. Do request (1->2)
	//   3b. Succeed
	//   3c. Update metrics so desired is 1 CU
	//   3d. Do request (2->1)
	//   3e. Succeed
	//   3f. Update metrics so desired is 2 CU
	//   3g. Do request (1->2)
	//   3h. Fail
	//   3i. Update metrics so desired is 1 CU
	//   3j. Check: We *would* need to make explicit request back down (can't contact plugin)
	//   3k. Update metrics so desired is 2 CU
	//   3l. Do request (1->2)
	//   3m. Succeed
	// 4. vm-monitor request (1->2)
	//   ... etc etc. Roughly the same as the previous two.
	//
	// A lot of this is duplicated and follows the same form. So, we try to deduplicate below.

	type scalingCallbacks struct {
		kind           string
		action         func(metrics api.Metrics, start, end api.Resources) core.ActionSet
		requestStart   func(helpers.Assert, *core.State, time.Time, core.ActionSet)
		requestSuccess func(helpers.Assert, *core.State, time.Time, api.Resources, api.Resources)
		requestFailed  func(helpers.Assert, *core.State, time.Time, api.Resources, api.Resources)
	}

	type scriptStep func(scalingCallbacks, helpers.Assert, *core.State, *api.Metrics, *helpers.FakeClock)

	scriptConditional := func(steps map[string]scriptStep) scriptStep {
		return func(cb scalingCallbacks, a helpers.Assert, s *core.State, m *api.Metrics, c *helpers.FakeClock) {
			if step, ok := steps[cb.kind]; ok {
				step(cb, a, s, m, c)
			} else if step, ok := steps["else"]; ok {
				step(cb, a, s, m, c)
			} else {
				panic(fmt.Errorf("no handler for callbacks kind %q", cb.kind))
			}
		}
	}
	scriptWithWarnings := func(step scriptStep, warnings map[string][]string) scriptStep {
		return func(cb scalingCallbacks, a helpers.Assert, s *core.State, m *api.Metrics, c *helpers.FakeClock) {
			step(cb, a.WithWarnings(warnings[cb.kind]...), s, m, c)
		}
	}
	scriptClockInc := func(d string) scriptStep {
		return func(_ scalingCallbacks, _ helpers.Assert, _ *core.State, _ *api.Metrics, c *helpers.FakeClock) {
			c.Inc(duration(d))
		}
	}
	scriptUpdateMetrics := func(metrics api.Metrics) scriptStep {
		return func(_ scalingCallbacks, _ helpers.Assert, s *core.State, m *api.Metrics, _ *helpers.FakeClock) {
			s.UpdateMetrics(metrics)
			*m = metrics
		}
	}
	nextActions := func(s *core.State, t time.Time, override bool) (_ core.ActionSet, wait bool) {
		actionSet := s.NextActions(t)
		wait = actionSet.Wait != nil
		if override {
			actionSet.Wait = nil
		}
		return actionSet, wait
	}
	resForCU := DefaultComputeUnit.Mul
	actionSetForRequest := func(cb scalingCallbacks, m api.Metrics, startCU, endCU uint16, dur ...string) core.ActionSet {
		actionSet := cb.action(m, resForCU(startCU), resForCU(endCU))
		if len(dur) != 0 {
			actionSet.Wait = &core.ActionWait{Duration: duration(dur[0])}
		}
		return actionSet
	}
	scriptCheckMustWait := func(dur string) scriptStep {
		return func(_ scalingCallbacks, a helpers.Assert, s *core.State, _ *api.Metrics, c *helpers.FakeClock) {
			a.Call(s.NextActions, c.Now()).Equals(core.ActionSet{
				Wait: &core.ActionWait{Duration: duration(dur)},
			})
		}
	}
	scriptRequestStart := func(startCU, endCU uint16, wait bool, dur ...string) scriptStep {
		return func(cb scalingCallbacks, a helpers.Assert, s *core.State, m *api.Metrics, c *helpers.FakeClock) {
			actionSet := actionSetForRequest(cb, *m, startCU, endCU, dur...)
			a.Call(nextActions, s, c.Now(), len(dur) == 0).Equals(actionSet, wait)
			cb.requestStart(a, s, c.Now(), actionSet)
		}
	}
	scriptCheckDesiredRequest := func(startCU, endCU uint16, wait bool, dur ...string) scriptStep {
		return func(cb scalingCallbacks, a helpers.Assert, s *core.State, m *api.Metrics, c *helpers.FakeClock) {
			actionSet := actionSetForRequest(cb, *m, startCU, endCU, dur...)
			a.Call(nextActions, s, c.Now(), len(dur) == 0).Equals(actionSet, wait)
		}
	}
	scriptRequestSuccess := func(startCU, endCU uint16) scriptStep {
		return func(cb scalingCallbacks, a helpers.Assert, s *core.State, m *api.Metrics, c *helpers.FakeClock) {
			cb.requestSuccess(a, s, c.Now(), resForCU(startCU), resForCU(endCU))
		}
	}
	scriptRequestFailed := func(startCU, endCU uint16) scriptStep {
		return func(cb scalingCallbacks, a helpers.Assert, s *core.State, m *api.Metrics, c *helpers.FakeClock) {
			cb.requestFailed(a, s, c.Now(), resForCU(startCU), resForCU(endCU))
		}
	}

	pluginCallbacks := scalingCallbacks{
		kind: "plugin",
		action: func(metrics api.Metrics, start, end api.Resources) core.ActionSet {
			return core.ActionSet{
				PluginRequest: &core.ActionPluginRequest{
					LastPermit: &start,
					Target:     end,
					Metrics:    &metrics,
				},
			}
		},
		requestStart: func(a helpers.Assert, s *core.State, t time.Time, act core.ActionSet) {
			a.Do(s.Plugin().StartingRequest, t, act.PluginRequest.Target)
		},
		requestSuccess: func(a helpers.Assert, s *core.State, t time.Time, _, end api.Resources) {
			a.NoError(s.Plugin().RequestSuccessful, t, api.PluginResponse{
				Permit:      end,
				Migrate:     nil,
				ComputeUnit: nil,
			})
		},
		requestFailed: func(a helpers.Assert, s *core.State, t time.Time, _, _ api.Resources) {
			a.Do(s.Plugin().RequestFailed, t)
		},
	}
	neonvmCallbacks := scalingCallbacks{
		kind: "neonvm",
		action: func(metrics api.Metrics, start, end api.Resources) core.ActionSet {
			return core.ActionSet{
				NeonVMRequest: &core.ActionNeonVMRequest{
					Current: start,
					Target:  end,
				},
			}
		},
		requestStart: func(a helpers.Assert, s *core.State, t time.Time, act core.ActionSet) {
			a.Do(s.NeonVM().StartingRequest, t, act.NeonVMRequest.Target)
		},
		requestSuccess: func(a helpers.Assert, s *core.State, t time.Time, _, _ api.Resources) {
			a.Do(s.NeonVM().RequestSuccessful, t)
		},
		requestFailed: func(a helpers.Assert, s *core.State, t time.Time, _, _ api.Resources) {
			a.Do(s.NeonVM().RequestFailed, t)
		},
	}
	monitorCallbacks := scalingCallbacks{
		kind: "monitor",
		action: func(metrics api.Metrics, start, end api.Resources) core.ActionSet {
			var act core.ActionSet
			if end.HasFieldGreaterThan(start) /* upscaling */ {
				act.MonitorUpscale = &core.ActionMonitorUpscale{
					Current: start,
					Target:  end,
				}
			} else /* downscaling */ {
				act.MonitorDownscale = &core.ActionMonitorDownscale{
					Current: start,
					Target:  end,
				}
			}
			return act
		},
		requestStart: func(a helpers.Assert, s *core.State, t time.Time, act core.ActionSet) {
			if act.MonitorUpscale != nil {
				a.Do(s.Monitor().StartingUpscaleRequest, t, act.MonitorUpscale.Target)
			} else if act.MonitorDownscale != nil {
				a.Do(s.Monitor().StartingDownscaleRequest, t, act.MonitorDownscale.Target)
			} else {
				panic("expected either MonitorUpscale or MonitorDownscale")
			}
		},
		requestSuccess: func(a helpers.Assert, s *core.State, t time.Time, start, end api.Resources) {
			if end.HasFieldGreaterThan(start) /* upscaling */ {
				a.Do(s.Monitor().UpscaleRequestSuccessful, t)
			} else /* downscaling */ {
				a.Do(s.Monitor().DownscaleRequestAllowed, t)
			}
		},
		requestFailed: func(a helpers.Assert, s *core.State, t time.Time, start, end api.Resources) {
			if end.HasFieldGreaterThan(start) /* upscaling */ {
				a.Do(s.Monitor().UpscaleRequestFailed, t)
			} else /* downscaling */ {
				a.Do(s.Monitor().DownscaleRequestFailed, t)
			}
		},
	}

	// metrics for 1 CU
	loMetrics := api.Metrics{
		LoadAverage1Min:  0.0,
		LoadAverage5Min:  0.0, // unused
		MemoryUsageBytes: 0.0,
	}
	// metrics for 2 CU
	hiMetrics := api.Metrics{
		LoadAverage1Min:  0.3,
		LoadAverage5Min:  0.0, // unused
		MemoryUsageBytes: 0.0,
	}

	configOverride := helpers.WithConfigSetting(func(c *core.Config) {
		// Override values for consistency and clarity
		c.PluginRetryWait = duration("2s")
		c.NeonVMRetryWait = duration("2s")
		c.MonitorRetryWait = duration("2s")
		c.PluginRequestTick = duration("100s") // make very long, so we don't have to worry about it
	})

	clockTick := scriptClockInc("0.1s")
	scaleupFlow := []scriptStep{
		scriptUpdateMetrics(hiMetrics),
		// scale up
		scriptConditional(map[string]scriptStep{
			"plugin": scriptRequestStart(1, 2, false),
			"else":   scriptRequestStart(1, 2, true),
		}),
		clockTick,
		scriptRequestSuccess(1, 2),
		clockTick,
		// scale back down
		scriptUpdateMetrics(loMetrics),
		scriptConditional(map[string]scriptStep{
			"plugin": scriptRequestStart(2, 1, false),
			"else":   scriptRequestStart(2, 1, true),
		}),
		clockTick,
		scriptRequestSuccess(2, 1),
		clockTick,
		// scale up, fail request.
		scriptUpdateMetrics(hiMetrics),
		scriptConditional(map[string]scriptStep{
			"plugin": scriptRequestStart(1, 2, false),
			"else":   scriptRequestStart(1, 2, true),
		}),
		clockTick,
		scriptRequestFailed(1, 2),
		clockTick,
		// check we would need to explicitly scale down if metrics change
		scriptUpdateMetrics(loMetrics),
		scriptWithWarnings(
			scriptCheckMustWait("1.9s"),
			map[string][]string{
				"plugin":  {"Wanted to make a request to the scheduler plugin, but previous request failed too recently"},
				"neonvm":  {"Wanted to make a request to NeonVM API, but recent request failed too recently"},
				"monitor": {"Wanted to send vm-monitor downscale request, but failed too recently"},
			},
		),
		scriptClockInc("1.9s"),
		scriptConditional(map[string]scriptStep{
			"plugin":  scriptCheckDesiredRequest(1, 1, false),
			"neonvm":  scriptCheckDesiredRequest(1, 1, true),
			"monitor": scriptCheckDesiredRequest(2, 1, true),
		}),
		scriptUpdateMetrics(hiMetrics),
		// succeed scale-up
		scriptConditional(map[string]scriptStep{
			"plugin": scriptRequestStart(1, 2, false),
			"else":   scriptRequestStart(1, 2, true),
		}),
		clockTick,
		scriptRequestSuccess(1, 2),
		clockTick,
	}
	scaledownFlow := []scriptStep{
		scriptUpdateMetrics(loMetrics),
		// scale down
		scriptConditional(map[string]scriptStep{
			"plugin": scriptRequestStart(2, 1, false),
			"else":   scriptRequestStart(2, 1, true),
		}),
		clockTick,
		scriptRequestSuccess(2, 1),
		clockTick,
		// scale back up
		scriptUpdateMetrics(hiMetrics),
		scriptConditional(map[string]scriptStep{
			"plugin": scriptRequestStart(1, 2, false),
			"else":   scriptRequestStart(1, 2, true),
		}),
		clockTick,
		scriptRequestSuccess(1, 2),
		clockTick,
		// scale down, fail request
		scriptUpdateMetrics(loMetrics),
		scriptConditional(map[string]scriptStep{
			"plugin": scriptRequestStart(2, 1, false),
			"else":   scriptRequestStart(2, 1, true),
		}),
		clockTick,
		scriptRequestFailed(2, 1),
		clockTick,
		// check we would need to explicitly scale up if metrics change
		scriptUpdateMetrics(hiMetrics),
		scriptWithWarnings(
			scriptCheckMustWait("1.9s"),
			map[string][]string{
				"plugin":  {"Wanted to make a request to the scheduler plugin, but previous request failed too recently"},
				"neonvm":  {"Wanted to make a request to NeonVM API, but recent request failed too recently"},
				"monitor": {"Wanted to send vm-monitor upscale request, but failed too recently"},
			},
		),
		scriptClockInc("1.9s"),
		scriptConditional(map[string]scriptStep{
			"plugin":  scriptCheckDesiredRequest(1, 2, false),
			"neonvm":  scriptCheckDesiredRequest(2, 2, true),
			"monitor": scriptCheckDesiredRequest(1, 2, true),
		}),
		scriptUpdateMetrics(loMetrics),
		// succeed scale-down
		scriptConditional(map[string]scriptStep{
			"plugin": scriptRequestStart(1, 1, false),
			"else":   scriptRequestStart(2, 1, true),
		}),
		clockTick,
		scriptRequestSuccess(2, 1),
		clockTick,
	}

	// --- Actually run the test ---

	a := helpers.NewAssert(t)
	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithMinMaxCU(1, 2),
		helpers.WithCurrentCU(1),
		configOverride,
	)

	clock := helpers.NewFakeClock(t)

	state.Monitor().Active(true)

	// Set metrics so initially we're doing nothing.
	lastMetrics := loMetrics
	a.Do(state.UpdateMetrics, lastMetrics)

	// Send initial scheduler request
	doInitialPluginRequest(a, state, clock, duration("0.1s"), &lastMetrics, resForCU(1))

	// --- Run scale-up ---
	scaleupCallbacks := []scalingCallbacks{pluginCallbacks, neonvmCallbacks, monitorCallbacks}
	for _, cb := range scaleupCallbacks {
		for stepi := range scaleupFlow {
			t.Logf("scale-up / %s / %d of %d", cb.kind, stepi, len(scaleupFlow))
			scaleupFlow[stepi](cb, a, state, &lastMetrics, clock)
		}
	}

	// --- Run scale-down ---
	scaledownCallbacks := []scalingCallbacks{monitorCallbacks, neonvmCallbacks, pluginCallbacks}
	for _, cb := range scaledownCallbacks {
		for stepi := range scaledownFlow {
			t.Logf("scale-down / %s / %d of %d", cb.kind, stepi, len(scaleupFlow))
			scaledownFlow[stepi](cb, a, state, &lastMetrics, clock)
		}
	}
}

// Checks that when metrics are updated during the downscaling process, between the NeonVM request
// and plugin request, we keep those processes mostly separate, without interference between them.
//
// This is distilled from a bug found on staging that resulted in faulty requests to the plugin.
func TestMetricsConcurrentUpdatedDuringDownscale(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)
	clockTick := func() {
		clock.Inc(100 * time.Millisecond)
	}
	resForCU := DefaultComputeUnit.Mul

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		// NOTE: current CU is greater than max CU. This is in line with what happens when
		// unassigned pooled VMs created by the control plane are first assigned and endpoint and
		// must immediately scale down.
		helpers.WithMinMaxCU(1, 2),
		helpers.WithCurrentCU(3),
	)
	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	// Send initial scheduler request - without the monitor active, so we're stuck at 4 CU for now
	a.
		WithWarnings("Wanted to send vm-monitor downscale request, but there's no active connection").
		Call(state.NextActions, clock.Now()).
		Equals(core.ActionSet{
			PluginRequest: &core.ActionPluginRequest{
				LastPermit: nil,
				Target:     resForCU(3),
				Metrics:    nil,
			},
		})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(3),
		Migrate:     nil,
		ComputeUnit: nil,
	})

	clockTick()

	// Monitor's now active, so we should be asking it for downscaling.
	// We don't yet have metrics though, so we only want to downscale as much as is required.
	state.Monitor().Active(true)
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")},
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(3),
			Target:  resForCU(2),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(2))

	// In the middle of the vm-monitor request, update the metrics so that now the desired resource
	// usage is actually 1 CU
	clockTick()
	// the actual metrics we got in the actual logs
	metrics := api.Metrics{
		LoadAverage1Min:  0.0,
		LoadAverage5Min:  0.0,
		MemoryUsageBytes: 150589570, // 143.6 MiB
	}
	a.Do(state.UpdateMetrics, metrics)

	// nothing to do yet, until the existing vm-monitor request finishes
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.7s")}, // plugin request tick wait
	})

	clockTick()

	// When the vm-monitor request finishes, we want to both
	// (a) request additional downscaling from vm-monitor, and
	// (b) make a NeonVM request for the initially approved downscaling
	a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.6s")}, // plugin request tick wait
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(3),
			Target:  resForCU(2),
		},
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(2),
			Target:  resForCU(1),
		},
	})
	// Start both requests. The vm-monitor request will finish first, but after that we'll just be
	// waiting on the NeonVM request (and then redoing a follow-up for more downscaling).
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(1))

	clockTick()

	a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())
	a.
		WithWarnings(
			"Wanted to make a request to NeonVM API, but there's already NeonVM request (for different resources) ongoing",
		).
		Call(nextActions).
		Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("4.5s")}, // plugin request tick wait
		})

	clockTick()

	a.Do(state.NeonVM().RequestSuccessful, clock.Now())
	a.
		Call(nextActions).
		Equals(core.ActionSet{
			// At this point in the original logs from staging, the intended request to the plugin was
			// incorrectly for 1 CU, rather than 2 CU. So, the rest of this test case is mostly just
			// rounding out the rest of the scale-down routine.
			PluginRequest: &core.ActionPluginRequest{
				LastPermit: ptr(resForCU(3)),
				Target:     resForCU(2),
				Metrics:    &metrics,
			},
			NeonVMRequest: &core.ActionNeonVMRequest{
				Current: resForCU(2),
				Target:  resForCU(1),
			},
		})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(1))

	clockTick()

	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(2),
		Migrate:     nil,
		ComputeUnit: nil,
	})
	// Still waiting for NeonVM request to complete
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")}, // plugin request tick wait
	})

	clockTick()

	// After the NeonVM request finishes, all that we have left to do is inform the plugin of the
	// final downscaling.
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(2)),
			Target:     resForCU(1),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(1))

	clockTick()

	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(1),
		Migrate:     nil,
		ComputeUnit: nil,
	})
	// Nothing left to do
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")}, // plugin request tick wait
	})
}
