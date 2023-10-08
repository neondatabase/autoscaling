package core_test

import (
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
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
				PluginRequestTick:                  time.Second,
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

var DefaultComputeUnit = api.Resources{VCPU: 250, Mem: 1}

var DefaultInitialStateConfig = helpers.InitialStateConfig{
	ComputeUnit:    DefaultComputeUnit,
	MemorySlotSize: resource.MustParse("1Gi"),

	MinCU: 1,
	MaxCU: 4,
	Core: core.Config{
		DefaultScalingConfig: api.ScalingConfig{
			LoadAverageFractionTarget: 0.5,
			MemoryUsageFractionTarget: 0.5,
		},
		PluginRequestTick:                  5 * time.Second,
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
	computeUnit api.Resources,
	metrics *api.Metrics,
	resources api.Resources,
) {
	a.WithWarnings("Can't determine desired resources because compute unit hasn't been set yet").
		Call(state.NextActions, clock.Now()).
		Equals(core.ActionSet{
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
		ComputeUnit: computeUnit,
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

	state.Plugin().NewScheduler()
	state.Monitor().Active(true)

	// Send initial scheduler request:
	doInitialPluginRequest(a, state, clock, duration("0.1s"), DefaultComputeUnit, nil, resForCU(1))

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
		ComputeUnit: DefaultComputeUnit,
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
		ComputeUnit: DefaultComputeUnit,
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

	state.Plugin().NewScheduler()
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

	doInitialPluginRequest(a, state, clock, clockTick, DefaultComputeUnit, &metrics, resources)

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
				ComputeUnit: DefaultComputeUnit,
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
	clockTick := func() helpers.Elapsed {
		return clock.Inc(clockTickDuration)
	}
	resForCU := DefaultComputeUnit.Mul

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithMinMaxCU(1, 8),
		helpers.WithInitialCU(6), // NOTE: Start at 6 CU, so we're trying to scale down immediately.
		helpers.WithConfigSetting(func(c *core.Config) {
			// values close to the default, so request timing works out a little better.
			c.PluginRequestTick = duration("6s")
			c.MonitorDeniedDownscaleCooldown = duration("4s")
		}),
	)

	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	state.Plugin().NewScheduler()
	state.Monitor().Active(true)

	doInitialPluginRequest(a, state, clock, duration("0.1s"), DefaultComputeUnit, nil, resForCU(6))

	// Set metrics
	clockTick().AssertEquals(duration("0.2s"))
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
	// 1. Deny all requests in the first pass
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
	// First pass: deny all requests. Should try from 1 to 5 CU.
	clock.Elapsed().AssertEquals(duration("0.2s"))
	currentPluginWait := duration("5.8s")
	for cu := uint16(1); cu <= 5; cu += 1 {
		a.Call(nextActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current: resForCU(6),
				Target:  resForCU(cu),
			},
		})
		// Do the request:
		a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(cu))
		a.Call(nextActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
		})
		clockTick()
		currentPluginWait -= clockTickDuration
		a.Do(state.Monitor().DownscaleRequestDenied, clock.Now())
	}
	// At the end, we should be waiting to retry downscaling:
	a.Call(nextActions).Equals(core.ActionSet{
		// Taken from DefaultInitialStateConfig.Core.MonitorDeniedDownscaleCooldown
		Wait: &core.ActionWait{Duration: duration("4s")},
	})

	clock.Inc(duration("4s")).AssertEquals(duration("4.7s"))
	currentPluginWait = duration("1.3s")

	// Second pass: Approve only down to 3 CU, then NeonVM & plugin requests.
	for cu := uint16(1); cu <= 3; cu += 1 {
		a.Call(nextActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current: resForCU(6),
				Target:  resForCU(cu),
			},
		})
		a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(cu))
		a.Call(nextActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
		})
		clockTick()
		currentPluginWait -= clockTickDuration
		if cu < 3 /* deny up to 3 */ {
			a.Do(state.Monitor().DownscaleRequestDenied, clock.Now())
		} else {
			a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())
		}
	}
	// At this point, waiting 3.9s for next attempt to downscale below 3 CU (last request was
	// successful, but the one before it wasn't), and 1s for plugin tick.
	// Also, because downscaling was approved, we should want to make a NeonVM request to do that.
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(6),
			Target:  resForCU(3),
		},
	})
	// Make the request:
	a.Do(state.NeonVM().StartingRequest, time.Now(), resForCU(3))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1s")},
	})
	clockTick().AssertEquals(duration("5.1s"))
	a.Do(state.NeonVM().RequestSuccessful, time.Now())
	// Successfully scaled down, so we should now inform the plugin. But also, we'll want to retry
	// the downscale request to vm-monitor once the retry is up:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.8s")},
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(6)),
			Target:     resForCU(3),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.8s")},
	})
	clockTick().AssertEquals(duration("5.2s"))
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(3),
		Migrate:     nil,
		ComputeUnit: DefaultComputeUnit,
	})
	// ... And *now* there's nothing left to do but wait until downscale wait expires:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.7s")},
	})

	// so, wait for that:
	clock.Inc(duration("3.7s")).AssertEquals(duration("8.9s"))

	// Third pass: deny all requests.
	currentPluginWait = duration("2.2s")
	for cu := uint16(1); cu < 3; cu += 1 {
		a.Call(nextActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current: resForCU(3),
				Target:  resForCU(cu),
			},
		})
		a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(cu))
		a.Call(nextActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
		})
		clockTick()
		currentPluginWait -= clockTickDuration
		a.Do(state.Monitor().DownscaleRequestDenied, clock.Now())
	}
	clock.Elapsed().AssertEquals(duration("9.1s"))
	// At the end, we should be waiting to retry downscaling (but actually, the regular plugin
	// request is coming up sooner).
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("2s")},
	})
	// ... so, wait for that plugin request/response, and then wait to retry downscaling:
	clock.Inc(duration("2s")).AssertEquals(duration("11.1s"))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("2s")}, // still want to retry vm-monitor downscaling
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(3)),
			Target:     resForCU(3),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("2s")}, // still waiting on retrying vm-monitor downscaling
	})
	clockTick().AssertEquals(duration("11.2s"))
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(3),
		Migrate:     nil,
		ComputeUnit: DefaultComputeUnit,
	})
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1.9s")}, // yep, still waiting on retrying vm-monitor downscaling
	})

	clock.Inc(duration("2s")).AssertEquals(duration("13.2s"))

	// Fourth pass: approve down to 1 CU
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.9s")}, // waiting for plugin request tick
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(3),
			Target:  resForCU(1),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(1))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.9s")}, // still waiting on plugin
	})
	clockTick().AssertEquals(duration("13.3s"))
	a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())
	// Still waiting on plugin request tick, but we can make a NeonVM request to enact the
	// downscaling right away !
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.8s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(3),
			Target:  resForCU(1),
		},
	})
	a.Do(state.NeonVM().StartingRequest, time.Now(), resForCU(1))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.8s")}, // yep, still waiting on the plugin
	})
	clockTick().AssertEquals(duration("13.4s"))
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
	clockTick().AssertEquals(duration("13.5s"))
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(1),
		Migrate:     nil,
		ComputeUnit: DefaultComputeUnit,
	})
	// And now there's truly nothing left to do. Back to waiting on plugin request tick :)
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("5.9s")},
	})
}

// Checks that we scale up in a timely manner when the vm-monitor requests it, and don't request
// downscaling until the time expires.
func TestRequestedUpscale(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)
	clockTick := func() helpers.Elapsed {
		return clock.Inc(100 * time.Millisecond)
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

	state.Plugin().NewScheduler()
	state.Monitor().Active(true)

	// Send initial scheduler request:
	doInitialPluginRequest(a, state, clock, duration("0.1s"), DefaultComputeUnit, nil, resForCU(1))

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
	state.Debug(true)
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
		ComputeUnit: DefaultComputeUnit,
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
		ComputeUnit: DefaultComputeUnit,
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
					ComputeUnit: DefaultComputeUnit,
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
					ComputeUnit: DefaultComputeUnit,
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
			helpers.WithInitialCU(2),
		)

		state.Plugin().NewScheduler()
		state.Monitor().Active(true)

		doInitialPluginRequest(a, state, clock, duration("0.1s"), DefaultComputeUnit, nil, resForCU(2))

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

// Checks that if we're disconnected from the scheduler plugin, we're able to downscale and
// re-upscale back to the last allocation before disconnection, but not beyond that.
// Also checks that when we reconnect, we first *inform* the scheduler plugin of the current
// resource allocation, and *then* send a follow-up request asking for additional resources.
func TestSchedulerDownscaleReupscale(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)
	clockTick := func() helpers.Elapsed {
		return clock.Inc(100 * time.Millisecond)
	}
	resForCU := DefaultComputeUnit.Mul

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithMinMaxCU(1, 3),
		helpers.WithInitialCU(2),
	)
	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	state.Plugin().NewScheduler()
	state.Monitor().Active(true)

	// Send initial scheduler request:
	doInitialPluginRequest(a, state, clock, duration("0.1s"), DefaultComputeUnit, nil, resForCU(2))

	clockTick()

	// Set metrics
	a.Do(state.UpdateMetrics, api.Metrics{
		LoadAverage1Min:  0.3, // <- means desired scale = 2
		LoadAverage5Min:  0.0, // unused
		MemoryUsageBytes: 0.0,
	})
	// Check we're not supposed to do anything
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")},
	})

	clockTick()

	// Record the scheduler as disconnected
	state.Plugin().SchedulerGone()
	// ... and check that there's nothing we can do:
	state.Debug(true)
	a.Call(nextActions).Equals(core.ActionSet{})

	clockTick()

	// First:
	// 1. Change the metrics so we want to downscale to 1 CU
	// 2. Request downscaling from the vm-monitor
	// 3. Do the NeonVM request
	// 4. But *don't* do the request to the scheduler plugin (because it's not there)
	a.Do(state.UpdateMetrics, api.Metrics{
		LoadAverage1Min:  0.0,
		LoadAverage5Min:  0.0,
		MemoryUsageBytes: 0.0,
	})
	// Check that we agree about desired resources
	a.Call(getDesiredResources, state, clock.Now()).
		Equals(resForCU(1))
	// Do vm-monitor request:
	a.Call(nextActions).Equals(core.ActionSet{
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(2),
			Target:  resForCU(1),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(1))
	clockTick()
	a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())
	// Do the NeonVM request
	a.Call(nextActions).Equals(core.ActionSet{
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(2),
			Target:  resForCU(1),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(1))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())

	// Now the current state reflects the desired state, so there shouldn't be anything else we need
	// to do or wait on.
	a.Call(nextActions).Equals(core.ActionSet{})

	// Next:
	// 1. Change the metrics so we want to upscale to 3 CU
	// 2. Can't do the scheduler plugin request (not active), but we previously got a permit for 2 CU
	// 3. Do the NeonVM request for 2 CU (3 isn't approved)
	// 4. Do vm-monitor upscale request for 2 CU
	lastMetrics := api.Metrics{
		LoadAverage1Min:  0.5,
		LoadAverage5Min:  0.0,
		MemoryUsageBytes: 0.0,
	}
	a.Do(state.UpdateMetrics, lastMetrics)
	a.Call(getDesiredResources, state, clock.Now()).
		Equals(resForCU(3))
	// Do NeonVM request
	a.Call(nextActions).Equals(core.ActionSet{
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(1),
			Target:  resForCU(2),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())
	// Do vm-monitor request
	a.Call(nextActions).Equals(core.ActionSet{
		MonitorUpscale: &core.ActionMonitorUpscale{
			Current: resForCU(1),
			Target:  resForCU(2),
		},
	})
	a.Do(state.Monitor().StartingUpscaleRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Do(state.Monitor().UpscaleRequestSuccessful, clock.Now())

	// Nothing left to do in the meantime, because again, the current state reflects the desired
	// state (at least, given that the we can't request anything from the scheduler plugin)

	// Finally:
	// 1. Update the state so we can now communicate with the scheduler plugin
	// 2. Make an initial request to the plugin to inform it of *current* resources
	// 3. Make another request to the plugin to request up to 3 CU
	// We could test after that too, but this should be enough.
	a.Do(state.Plugin().NewScheduler)
	// Initial request: informative about current usage
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(2)),
			Target:     resForCU(2),
			Metrics:    &lastMetrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(2),
		Migrate:     nil,
		ComputeUnit: DefaultComputeUnit,
	})
	// Follow-up request: request additional resources
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(2)),
			Target:     resForCU(3),
			Metrics:    &lastMetrics,
		},
	})
}
