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
		PluginRequestTick:              5 * time.Second,
		PluginDeniedRetryWait:          2 * time.Second,
		MonitorDeniedDownscaleCooldown: 5 * time.Second,
		MonitorRetryWait:               3 * time.Second,
		Warn:                           func(string, ...any) {},
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
	var actions core.ActionSet
	updateActions := func() core.ActionSet {
		actions = state.NextActions(clock.Now())
		return actions
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
	a.Call(updateActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(1)),
			Target:     resForCU(2),
			Metrics:    &lastMetrics,
		},
	})
	// start the request:
	a.Do(state.Plugin().StartingRequest, clock.Now(), actions.PluginRequest.Target)
	clockTick().AssertEquals(duration("0.3s"))
	// should have nothing more to do; waiting on plugin request to come back
	a.Call(updateActions).Equals(core.ActionSet{})
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(2),
		Migrate:     nil,
		ComputeUnit: DefaultComputeUnit,
	})

	// Scheduler approval is done, now we should be making the request to NeonVM
	a.Call(updateActions).Equals(core.ActionSet{
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
			Current: resForCU(1),
			Target:  resForCU(2),
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
	a.Call(getDesiredResources, state, clock.Now()).
		Equals(resForCU(1))

	// First step in downscaling is getting approval from the vm-monitor:
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.6s")},
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(2),
			Target:  resForCU(1),
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
			Current: resForCU(2),
			Target:  resForCU(1),
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
			LastPermit: ptr(resForCU(2)),
			Target:     resForCU(1),
			Metrics:    &lastMetrics,
		},
		// shouldn't have anything to say to the other components
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), actions.PluginRequest.Target)
	clockTick().AssertEquals(duration("0.9s"))
	// should have nothing more to do; waiting on plugin request to come back
	a.Call(updateActions).Equals(core.ActionSet{})
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(1),
		Migrate:     nil,
		ComputeUnit: DefaultComputeUnit,
	})

	// Finally, check there's no leftover actions:
	a.Call(updateActions).Equals(core.ActionSet{
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

	var actions core.ActionSet
	updateActions := func() core.ActionSet {
		actions = state.NextActions(clock.Now())
		return actions
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
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current: resForCU(6),
				Target:  resForCU(cu),
			},
		})
		// Do the request:
		a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(cu))
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
		})
		clockTick()
		currentPluginWait -= clockTickDuration
		a.Do(state.Monitor().DownscaleRequestDenied, clock.Now())
	}
	// At the end, we should be waiting to retry downscaling:
	a.Call(updateActions).Equals(core.ActionSet{
		// Taken from DefaultInitialStateConfig.Core.MonitorDeniedDownscaleCooldown
		Wait: &core.ActionWait{Duration: duration("4s")},
	})

	clock.Inc(duration("4s")).AssertEquals(duration("4.7s"))
	currentPluginWait = duration("1.3s")

	// Second pass: Approve only down to 3 CU, then NeonVM & plugin requests.
	for cu := uint16(1); cu <= 3; cu += 1 {
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current: resForCU(6),
				Target:  resForCU(cu),
			},
		})
		a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(cu))
		a.Call(updateActions).Equals(core.ActionSet{
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
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(6),
			Target:  resForCU(3),
		},
	})
	// Make the request:
	a.Do(state.NeonVM().StartingRequest, time.Now(), resForCU(3))
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1s")},
	})
	clockTick().AssertEquals(duration("5.1s"))
	a.Do(state.NeonVM().RequestSuccessful, time.Now())
	// Successfully scaled down, so we should now inform the plugin. But also, we'll want to retry
	// the downscale request to vm-monitor once the retry is up:
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.8s")},
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(6)),
			Target:     resForCU(3),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.8s")},
	})
	clockTick().AssertEquals(duration("5.2s"))
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(3),
		Migrate:     nil,
		ComputeUnit: DefaultComputeUnit,
	})
	// ... And *now* there's nothing left to do but wait until downscale wait expires:
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.7s")},
	})

	// so, wait for that:
	clock.Inc(duration("3.7s")).AssertEquals(duration("8.9s"))

	// Third pass: deny all requests.
	currentPluginWait = duration("2.2s")
	for cu := uint16(1); cu < 3; cu += 1 {
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current: resForCU(3),
				Target:  resForCU(cu),
			},
		})
		a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(cu))
		a.Call(updateActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
		})
		clockTick()
		currentPluginWait -= clockTickDuration
		a.Do(state.Monitor().DownscaleRequestDenied, clock.Now())
	}
	clock.Elapsed().AssertEquals(duration("9.1s"))
	// At the end, we should be waiting to retry downscaling (but actually, the regular plugin
	// request is coming up sooner).
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("2s")},
	})
	// ... so, wait for that plugin request/response, and then wait to retry downscaling:
	clock.Inc(duration("2s")).AssertEquals(duration("11.1s"))
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("2s")}, // still want to retry vm-monitor downscaling
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(3)),
			Target:     resForCU(3),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("2s")}, // still waiting on retrying vm-monitor downscaling
	})
	clockTick().AssertEquals(duration("11.2s"))
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(3),
		Migrate:     nil,
		ComputeUnit: DefaultComputeUnit,
	})
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1.9s")}, // yep, still waiting on retrying vm-monitor downscaling
	})

	clock.Inc(duration("2s")).AssertEquals(duration("13.2s"))

	// Fourth pass: approve down to 1 CU
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.9s")}, // waiting for plugin request tick
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(3),
			Target:  resForCU(1),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(1))
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.9s")}, // still waiting on plugin
	})
	clockTick().AssertEquals(duration("13.3s"))
	a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now())
	// Still waiting on plugin request tick, but we can make a NeonVM request to enact the
	// downscaling right away !
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.8s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(3),
			Target:  resForCU(1),
		},
	})
	a.Do(state.NeonVM().StartingRequest, time.Now(), resForCU(1))
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.8s")}, // yep, still waiting on the plugin
	})
	clockTick().AssertEquals(duration("13.4s"))
	a.Do(state.NeonVM().RequestSuccessful, time.Now())
	// Successfully downscaled, so now we should inform the plugin. Not waiting on any retries.
	a.Call(updateActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit: ptr(resForCU(3)),
			Target:     resForCU(1),
			Metrics:    &metrics,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(1))
	a.Call(updateActions).Equals(core.ActionSet{
		// not waiting on anything!
	})
	clockTick().AssertEquals(duration("13.5s"))
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), api.PluginResponse{
		Permit:      resForCU(1),
		Migrate:     nil,
		ComputeUnit: DefaultComputeUnit,
	})
	// And now there's truly nothing left to do. Back to waiting on plugin request tick :)
	a.Call(updateActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("5.9s")},
	})
}
