package core_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/exp/slices"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/agent/core/revsource"
	helpers "github.com/neondatabase/autoscaling/pkg/agent/core/testhelpers"
	"github.com/neondatabase/autoscaling/pkg/api"
)

func Test_DesiredResourcesFromMetricsOrRequestedUpscaling(t *testing.T) {
	slotSize := api.Bytes(1 << 30 /* 1 Gi */)

	cases := []struct {
		name string

		// helpers for setting fields (ish) of State:
		systemMetrics     *core.SystemMetrics
		lfcMetrics        *core.LFCMetrics
		enableLFCMetrics  bool
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
			systemMetrics: &core.SystemMetrics{
				LoadAverage1Min:   0.30,
				LoadAverage5Min:   0.0,
				MemoryUsageBytes:  0.0,
				MemoryCachedBytes: 0.0,
			},
			lfcMetrics:        nil,
			enableLFCMetrics:  false,
			vmUsing:           api.Resources{VCPU: 250, Mem: 1 * slotSize},
			schedulerApproved: api.Resources{VCPU: 250, Mem: 1 * slotSize},
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   nil,

			expected: api.Resources{VCPU: 500, Mem: 2 * slotSize},
			warnings: nil,
		},
		{
			name: "MismatchedApprovedNoScaledown",
			systemMetrics: &core.SystemMetrics{
				LoadAverage1Min:   0.0, // ordinarily would like to scale down
				LoadAverage5Min:   0.0,
				MemoryUsageBytes:  0.0,
				MemoryCachedBytes: 0.0,
			},
			lfcMetrics:        nil,
			enableLFCMetrics:  false,
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
			systemMetrics: &core.SystemMetrics{
				LoadAverage1Min:   0.0, // ordinarily would like to scale down
				LoadAverage5Min:   0.0,
				MemoryUsageBytes:  0.0,
				MemoryCachedBytes: 0.0,
			},
			lfcMetrics:        nil,
			enableLFCMetrics:  false,
			vmUsing:           api.Resources{VCPU: 1000, Mem: 5 * slotSize}, // note: mem greater than maximum. It can happen when scaling bounds change
			schedulerApproved: api.Resources{VCPU: 1000, Mem: 5 * slotSize}, // unused
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   &api.Resources{VCPU: 1000, Mem: 4 * slotSize},

			expected: api.Resources{VCPU: 1000, Mem: 5 * slotSize},
			warnings: []string{
				"Can't decrease desired resources to within VM maximum because of vm-monitor previously denied downscale request",
			},
		},
		{
			name: "BasicLFCScaleup",
			systemMetrics: &core.SystemMetrics{
				LoadAverage1Min:   0.0,
				LoadAverage5Min:   0.0,
				MemoryUsageBytes:  0.0,
				MemoryCachedBytes: 0.0,
			},
			lfcMetrics: &core.LFCMetrics{
				CacheHitsTotal:   0.0, // unused
				CacheMissesTotal: 0.0, // unused
				CacheWritesTotal: 0.0, // unused

				ApproximateworkingSetSizeBuckets: []float64{
					// each value is number of 8KiB pages; compute units should be sized to hold
					// working set as <= 75% of RAM.
					// 3 CU works out to working set size between 197k and 295k pages (2-3 CU).
					// Window size is 5m and min wait before scaledown is also 5m.
					// Also note projectLen is 0.5.
					0, 15000, 30000, 40000, 50000,
					150000, 175000, 180000, 185000, 190000, // 180 < 197, but projection from 50 -> 150 = 200, > 197
					250000, 300000, 350000, 375000, 400000, // <- slope too steep compared to before; cutoff is line above.
					415000, 425000, 430000, 435000, 435000,
				},
			},
			enableLFCMetrics:  true,
			vmUsing:           api.Resources{VCPU: 250, Mem: 1 * slotSize},
			schedulerApproved: api.Resources{VCPU: 250, Mem: 1 * slotSize},
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   nil,

			expected: api.Resources{VCPU: 750, Mem: 3 * slotSize},
			warnings: nil,
		},
		{
			// Same system metrics as BasicScaleup, but we're waiting on lfc metrics. HOWEVER we can
			// still scale up if load average dictates it.
			name: "CanScaleUpWithoutExpectedLFCMetrics",
			systemMetrics: &core.SystemMetrics{
				LoadAverage1Min:   0.30,
				LoadAverage5Min:   0.0,
				MemoryUsageBytes:  0.0,
				MemoryCachedBytes: 0.0,
			},
			lfcMetrics:        nil,
			enableLFCMetrics:  true,
			vmUsing:           api.Resources{VCPU: 250, Mem: 1 * slotSize},
			schedulerApproved: api.Resources{VCPU: 250, Mem: 1 * slotSize},
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   nil,

			expected: api.Resources{VCPU: 500, Mem: 2 * slotSize},
			warnings: []string{
				"Making scaling decision without all required metrics available",
			},
		},
		{
			// We're waiting on metrics to be able to make an informed decision, BUT if we're out of
			// bounds we can still action that.
			name: "CanScaleToBoundsWithoutExpectedMetrics",
			systemMetrics: &core.SystemMetrics{
				LoadAverage1Min:   0.30,
				LoadAverage5Min:   0.0,
				MemoryUsageBytes:  0.0,
				MemoryCachedBytes: 0.0,
			},
			lfcMetrics:        nil,
			enableLFCMetrics:  true,
			vmUsing:           api.Resources{VCPU: 750, Mem: 5 * slotSize}, // max is 4; 5 > 4.
			schedulerApproved: api.Resources{VCPU: 750, Mem: 5 * slotSize}, // unused
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   nil,

			expected: api.Resources{VCPU: 750, Mem: 4 * slotSize},
			warnings: []string{
				"Making scaling decision without all required metrics available",
			},
		},
		{
			// Similar to CanScaleUpWithoutExpectedLFCMetrics, but for system metrics instead.
			// We use the same LFC metrics as from BasicLFCScaleup, so we're still allowed to scale
			// up.
			name:          "CanScaleUpWithoutExpectedSystemMetrics",
			systemMetrics: nil,
			lfcMetrics: &core.LFCMetrics{
				CacheHitsTotal:   0.0, // unused
				CacheMissesTotal: 0.0, // unused
				CacheWritesTotal: 0.0, // unused

				ApproximateworkingSetSizeBuckets: []float64{
					0, 15000, 30000, 40000, 50000,
					150000, 175000, 180000, 185000, 190000,
					250000, 300000, 350000, 375000, 400000,
					415000, 425000, 430000, 435000, 435000,
				},
			},
			enableLFCMetrics:  true,
			vmUsing:           api.Resources{VCPU: 250, Mem: 1 * slotSize},
			schedulerApproved: api.Resources{VCPU: 250, Mem: 1 * slotSize},
			requestedUpscale:  api.MoreResources{Cpu: false, Memory: false},
			deniedDownscale:   nil,

			expected: api.Resources{VCPU: 750, Mem: 3 * slotSize},
			warnings: []string{
				"Making scaling decision without all required metrics available",
			},
		},
	}

	for _, c := range cases {
		warnings := []string{}

		makeVM := func() api.VmInfo {
			return api.VmInfo{
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
				Config: api.VmConfig{
					AutoMigrationEnabled: false,
					AlwaysMigrate:        false,
					ScalingEnabled:       true,
					ScalingConfig:        nil,
				},
				CurrentRevision: nil,
			}
		}
		makeStateConfig := func(enableLFCMetrics bool) core.Config {
			return core.Config{
				ComputeUnit: api.Resources{VCPU: 250, Mem: 1 * slotSize},
				DefaultScalingConfig: api.ScalingConfig{
					LoadAverageFractionTarget:        lo.ToPtr(0.5),
					MemoryUsageFractionTarget:        lo.ToPtr(0.5),
					MemoryTotalFractionTarget:        lo.ToPtr(0.9),
					EnableLFCMetrics:                 lo.ToPtr(enableLFCMetrics),
					LFCToMemoryRatio:                 lo.ToPtr(0.75),
					LFCWindowSizeMinutes:             lo.ToPtr(5),
					LFCMinWaitBeforeDownscaleMinutes: lo.ToPtr(5),
					CPUStableZoneRatio:               lo.ToPtr(0.0),
					CPUMixedZoneRatio:                lo.ToPtr(0.0),
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
				RevisionSource: revsource.NewRevisionSource(0, nil),
				ObservabilityCallbacks: core.ObservabilityCallbacks{
					PluginLatency:  nil,
					MonitorLatency: nil,
					NeonVMLatency:  nil,
				},
			}
		}

		t.Run(c.name, func(t *testing.T) {
			state := core.NewState(makeVM(), makeStateConfig(c.enableLFCMetrics))

			// set the metrics
			if c.systemMetrics != nil {
				state.UpdateSystemMetrics(*c.systemMetrics)
			}
			if c.lfcMetrics != nil {
				state.UpdateLFCMetrics(*c.lfcMetrics)
			}

			now := time.Now()

			// set lastApproved by simulating a scheduler request/response
			state.Plugin().StartingRequest(now, c.schedulerApproved)
			err := state.Plugin().RequestSuccessful(now, vmv1.ZeroRevision.WithTime(now), api.PluginResponse{
				Permit:  c.schedulerApproved,
				Migrate: nil,
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
				state.Monitor().DownscaleRequestDenied(now, vmv1.ZeroRevision.WithTime(now))
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
			LoadAverageFractionTarget:        lo.ToPtr(0.5),
			MemoryUsageFractionTarget:        lo.ToPtr(0.5),
			MemoryTotalFractionTarget:        lo.ToPtr(0.9),
			EnableLFCMetrics:                 lo.ToPtr(false),
			LFCToMemoryRatio:                 lo.ToPtr(0.75),
			LFCWindowSizeMinutes:             lo.ToPtr(5),
			LFCMinWaitBeforeDownscaleMinutes: lo.ToPtr(15),
			CPUStableZoneRatio:               lo.ToPtr(0.0),
			CPUMixedZoneRatio:                lo.ToPtr(0.0),
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
		RevisionSource: &helpers.NilRevisionSource{},
		ObservabilityCallbacks: core.ObservabilityCallbacks{
			PluginLatency:  nil,
			MonitorLatency: nil,
			NeonVMLatency:  nil,
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
	nextActionsAssert := a
	if metrics == nil {
		nextActionsAssert = nextActionsAssert.WithWarnings("Making scaling decision without all required metrics available")
	}

	rev := vmv1.ZeroRevision.WithTime(clock.Now())
	nextActionsAssert.Call(state.NextActions, clock.Now()).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit:     nil,
			Target:         resources,
			Metrics:        metrics,
			TargetRevision: rev,
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resources)
	clock.Inc(requestTime)
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), rev, api.PluginResponse{
		Permit:  resources,
		Migrate: nil,
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

type latencyObserver struct {
	t            *testing.T
	observations []struct {
		latency time.Duration
		flags   vmv1.Flag
	}
}

func (a *latencyObserver) observe(latency time.Duration, flags vmv1.Flag) {
	a.observations = append(a.observations, struct {
		latency time.Duration
		flags   vmv1.Flag
	}{latency, flags})
}

func (a *latencyObserver) assert(latency time.Duration, flags vmv1.Flag) {
	require.NotEmpty(a.t, a.observations)
	assert.Equal(a.t, latency, a.observations[0].latency)
	assert.Equal(a.t, flags, a.observations[0].flags)
	a.observations = a.observations[1:]
}

// assertEmpty should be called in defer
func (a *latencyObserver) assertEmpty() {
	assert.Empty(a.t, a.observations)
}

// Thorough checks of a relatively simple flow - scaling from 1 CU to 2 CU and back down.
func TestBasicScaleUpAndDownFlow(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)
	clockTick := func() helpers.Elapsed {
		return clock.Inc(100 * time.Millisecond)
	}
	expectedRevision := helpers.NewExpectedRevision(clock.Now)
	resForCU := DefaultComputeUnit.Mul

	latencyObserver := &latencyObserver{t: t, observations: nil}
	defer latencyObserver.assertEmpty()
	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithTestingLogfWarnings(t),
		helpers.WithConfigSetting(func(c *core.Config) {
			c.RevisionSource = revsource.NewRevisionSource(0, latencyObserver.observe)
		}),
	)
	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	state.Monitor().Active(true)

	// Send initial scheduler request:
	doInitialPluginRequest(a, state, clock, duration("0.1s"), nil, resForCU(1))

	// Set metrics
	clockTick().AssertEquals(duration("0.2s"))
	lastMetrics := core.SystemMetrics{
		LoadAverage1Min:   0.3,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  0.0,
		MemoryCachedBytes: 0.0,
	}
	a.Do(state.UpdateSystemMetrics, lastMetrics)
	// double-check that we agree about the desired resources
	a.Call(getDesiredResources, state, clock.Now()).
		Equals(resForCU(2))

	// Now that the initial scheduler request is done, and we have metrics that indicate
	// scale-up would be a good idea.
	expectedRevision.Value = 1
	expectedRevision.Flags = revsource.Upscale

	// We should be contacting the scheduler to get approval.
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit:     lo.ToPtr(resForCU(1)),
			Target:         resForCU(2),
			Metrics:        lo.ToPtr(lastMetrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	// start the request:
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	clockTick().AssertEquals(duration("0.3s"))
	// should have nothing more to do; waiting on plugin request to come back
	a.Call(nextActions).Equals(core.ActionSet{})
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(2),
		Migrate: nil,
	})

	// Scheduler approval is done, now we should be making the request to NeonVM
	a.Call(nextActions).Equals(core.ActionSet{
		// expected to make a scheduler request every 5s; it's been 100ms since the last one, so
		// if the NeonVM request didn't come back in time, we'd need to get woken up to start
		// the next scheduler request.
		Wait: &core.ActionWait{Duration: duration("4.9s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current:        resForCU(1),
			Target:         resForCU(2),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	// start the request:
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
	clockTick().AssertEquals(duration("0.4s"))
	// should have nothing more to do; waiting on NeonVM request to come back
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")},
	})

	// Until NeonVM is successful, we won't see any observations.
	latencyObserver.assertEmpty()

	// Now NeonVM request is done.
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())
	a.Do(state.UpdatedVM, helpers.CreateVmInfo(
		DefaultInitialStateConfig.VM,
		helpers.WithCurrentRevision(expectedRevision.WithTime()),
	))

	// And we see the latency. We started at 0.2s and finished at 0.4s
	latencyObserver.assert(duration("0.2s"), revsource.Upscale)

	// NeonVM change is done, now we should finish by notifying the vm-monitor
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")}, // same as previous, clock hasn't changed
		MonitorUpscale: &core.ActionMonitorUpscale{
			Current:        resForCU(1),
			Target:         resForCU(2),
			TargetRevision: expectedRevision.WithTime(),
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

	expectedRevision.Value += 1
	expectedRevision.Flags = revsource.Downscale

	// Set metrics back so that desired resources should now be zero
	lastMetrics = core.SystemMetrics{
		LoadAverage1Min:   0.0,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  0.0,
		MemoryCachedBytes: 0.0,
	}
	a.Do(state.UpdateSystemMetrics, lastMetrics)
	// double-check that we agree about the new desired resources
	a.Call(getDesiredResources, state, clock.Now()).
		Equals(resForCU(1))

	// First step in downscaling is getting approval from the vm-monitor:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.6s")},
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current:        resForCU(2),
			Target:         resForCU(1),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(1))
	clockTick().AssertEquals(duration("0.7s"))
	// should have nothing more to do; waiting on vm-monitor request to come back
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.5s")},
	})
	a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now(), expectedRevision.WithTime())

	// After getting approval from the vm-monitor, we make the request to NeonVM to carry it out
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.5s")}, // same as previous, clock hasn't changed
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current:        resForCU(2),
			Target:         resForCU(1),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(1))
	clockTick().AssertEquals(duration("0.8s"))
	// should have nothing more to do; waiting on NeonVM request to come back
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.4s")},
	})
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())

	// Request to NeonVM is successful, but let's wait one more tick for
	// NeonVM to pick up the changes and apply those.
	clockTick().AssertEquals(duration("0.9s"))

	// This means that the NeonVM has applied the changes.
	a.Do(state.UpdatedVM, helpers.CreateVmInfo(
		DefaultInitialStateConfig.VM,
		helpers.WithCurrentRevision(expectedRevision.WithTime()),
	))

	// We started at 0.6s and finished at 0.9s.
	latencyObserver.assert(duration("0.3s"), revsource.Downscale)

	// Request to NeonVM completed, it's time to inform the scheduler plugin:
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit:     lo.ToPtr(resForCU(2)),
			Target:         resForCU(1),
			Metrics:        lo.ToPtr(lastMetrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
		// shouldn't have anything to say to the other components
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(1))
	clockTick().AssertEquals(duration("1s"))
	// should have nothing more to do; waiting on plugin request to come back
	a.Call(nextActions).Equals(core.ActionSet{})
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(1),
		Migrate: nil,
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
	expectedRevision := helpers.NewExpectedRevision(clock.Now)

	latencyObserver := &latencyObserver{t: t, observations: nil}
	defer latencyObserver.assertEmpty()

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithConfigSetting(func(c *core.Config) {
			// This time, we will test plugin latency
			c.ObservabilityCallbacks.PluginLatency = latencyObserver.observe
			c.RevisionSource = revsource.NewRevisionSource(0, nil)
		}),
	)

	state.Monitor().Active(true)

	metrics := core.SystemMetrics{
		LoadAverage1Min:   0.0,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  0.0,
		MemoryCachedBytes: 0.0,
	}
	resources := DefaultComputeUnit

	a.Do(state.UpdateSystemMetrics, metrics)

	base := duration("0s")
	clock.Elapsed().AssertEquals(base)

	clockTick := duration("100ms")
	reqDuration := duration("50ms")
	reqEvery := DefaultInitialStateConfig.Core.PluginRequestTick
	endTime := duration("20s")

	doInitialPluginRequest(a, state, clock, clockTick, lo.ToPtr(metrics.ToAPI()), resources)

	for clock.Elapsed().Duration < endTime {
		timeSinceScheduledRequest := (clock.Elapsed().Duration - base) % reqEvery

		if timeSinceScheduledRequest != 0 {
			timeUntilNextRequest := reqEvery - timeSinceScheduledRequest
			a.Call(state.NextActions, clock.Now()).Equals(core.ActionSet{
				Wait: &core.ActionWait{Duration: timeUntilNextRequest},
			})
			clock.Inc(clockTick)
		} else {
			target := expectedRevision.WithTime()
			a.Call(state.NextActions, clock.Now()).Equals(core.ActionSet{
				PluginRequest: &core.ActionPluginRequest{
					LastPermit:     &resources,
					Target:         resources,
					Metrics:        lo.ToPtr(metrics.ToAPI()),
					TargetRevision: target,
				},
			})
			a.Do(state.Plugin().StartingRequest, clock.Now(), resources)
			a.Call(state.NextActions, clock.Now()).Equals(core.ActionSet{})
			clock.Inc(reqDuration)
			a.Call(state.NextActions, clock.Now()).Equals(core.ActionSet{})
			a.NoError(state.Plugin().RequestSuccessful, clock.Now(), target, api.PluginResponse{
				Permit:  resources,
				Migrate: nil,
			})
			clock.Inc(clockTick - reqDuration)
		}
	}
}

// In this test agent wants to upscale from 1 CU to 4 CU, but the plugin only allows 3 CU.
// Agent upscales to 3 CU, then tries to upscale to 4 CU again.
func TestPartialUpscaleThenFull(t *testing.T) {
	a := helpers.NewAssert(t)
	clock := helpers.NewFakeClock(t)
	clockTickDuration := duration("0.1s")
	clockTick := func() {
		clock.Inc(clockTickDuration)
	}
	expectedRevision := helpers.NewExpectedRevision(clock.Now)
	scalingLatencyObserver := &latencyObserver{t: t, observations: nil}
	defer scalingLatencyObserver.assertEmpty()

	pluginLatencyObserver := &latencyObserver{t: t, observations: nil}
	defer pluginLatencyObserver.assertEmpty()

	resForCU := DefaultComputeUnit.Mul

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithMinMaxCU(1, 4),
		helpers.WithCurrentCU(1),
		helpers.WithConfigSetting(func(c *core.Config) {
			c.RevisionSource = revsource.NewRevisionSource(0, scalingLatencyObserver.observe)
			c.ObservabilityCallbacks.PluginLatency = pluginLatencyObserver.observe
		}),
	)

	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	state.Monitor().Active(true)

	doInitialPluginRequest(a, state, clock, duration("0.1s"), nil, resForCU(1))

	// Set metrics
	clockTick()
	metrics := core.SystemMetrics{
		LoadAverage1Min:   1.0,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  12345678,
		MemoryCachedBytes: 0.0,
	}
	a.Do(state.UpdateSystemMetrics, metrics)

	// double-check that we agree about the desired resources
	a.Call(getDesiredResources, state, clock.Now()).
		Equals(resForCU(4))

	// Upscaling to 4 CU
	expectedRevision.Value = 1
	expectedRevision.Flags = revsource.Upscale
	targetRevision := expectedRevision.WithTime()
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit:     lo.ToPtr(resForCU(1)),
			Target:         resForCU(4),
			Metrics:        lo.ToPtr(metrics.ToAPI()),
			TargetRevision: targetRevision,
		},
	})

	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(4))
	clockTick()
	a.Call(nextActions).Equals(core.ActionSet{})
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), targetRevision, api.PluginResponse{
		Permit:  resForCU(3),
		Migrate: nil,
	})

	pluginLatencyObserver.assert(duration("0.1s"), revsource.Upscale)

	// NeonVM request
	a.
		WithWarnings("Wanted to make a request to the scheduler plugin, but previous request for more resources was denied too recently").
		Call(nextActions).
		Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("1.9s")},
			NeonVMRequest: &core.ActionNeonVMRequest{
				Current:        resForCU(1),
				Target:         resForCU(3),
				TargetRevision: expectedRevision.WithTime(),
			},
		})

	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(3))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())
	clockTick()
	a.Do(state.UpdatedVM, helpers.CreateVmInfo(
		DefaultInitialStateConfig.VM,
		helpers.WithCurrentCU(3),
		helpers.WithCurrentRevision(expectedRevision.WithTime()),
	))
	scalingLatencyObserver.assert(duration("0.3s"), revsource.Upscale)

	clock.Inc(duration("2s"))

	// Upscaling to 4 CU
	expectedRevision.Value += 1
	targetRevision = expectedRevision.WithTime()
	a.Call(nextActions).Equals(core.ActionSet{
		MonitorUpscale: &core.ActionMonitorUpscale{
			Current:        resForCU(1),
			Target:         resForCU(3),
			TargetRevision: expectedRevision.WithTime(),
		},
		PluginRequest: &core.ActionPluginRequest{
			LastPermit:     lo.ToPtr(resForCU(3)),
			Target:         resForCU(4),
			Metrics:        lo.ToPtr(metrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
	})

	a.Do(state.Monitor().StartingUpscaleRequest, clock.Now(), resForCU(3))
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(4))
	clockTick()
	a.Do(state.Monitor().UpscaleRequestSuccessful, clock.Now())
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), targetRevision, api.PluginResponse{
		Permit:  resForCU(4),
		Migrate: nil,
	})
	pluginLatencyObserver.assert(duration("0.1s"), revsource.Upscale)
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current:        resForCU(3),
			Target:         resForCU(4),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(4))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())
	vmInfo := helpers.CreateVmInfo(
		DefaultInitialStateConfig.VM,
		helpers.WithCurrentCU(4),
		helpers.WithCurrentRevision(expectedRevision.WithTime()),
	)
	clockTick()
	a.Do(state.UpdatedVM, vmInfo)

	scalingLatencyObserver.assert(duration("0.2s"), revsource.Upscale)
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
	expectedRevision := helpers.NewExpectedRevision(clock.Now)
	latencyObserver := &latencyObserver{t: t, observations: nil}
	defer latencyObserver.assertEmpty()
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
	metrics := core.SystemMetrics{
		LoadAverage1Min:   0.0,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  0.0,
		MemoryCachedBytes: 0.0,
	}
	a.Do(state.UpdateSystemMetrics, metrics)
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
			Current:        resForCU(6),
			Target:         resForCU(5),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(5))
	clockTick()
	a.Do(state.Monitor().DownscaleRequestDenied, clock.Now(), expectedRevision.WithTime())

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
				Current:        resForCU(6),
				Target:         resForCU(cu + 1),
				TargetRevision: expectedRevision.WithTime(),
			}
		}

		a.Call(nextActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current:        resForCU(cu + 1),
				Target:         resForCU(cu),
				TargetRevision: expectedRevision.WithTime(),
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
			a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now(), expectedRevision.WithTime())
		} else {
			a.Do(state.Monitor().DownscaleRequestDenied, clock.Now(), expectedRevision.WithTime())
		}
	}
	// At this point, waiting 3.7s for next attempt to downscale below 3 CU (last request was
	// successful, but the one before it wasn't), and 0.8s for plugin tick.
	// Also, because downscaling was approved, we should want to make a NeonVM request to do that.
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("2.3s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current:        resForCU(6),
			Target:         resForCU(3),
			TargetRevision: expectedRevision.WithTime(),
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
			LastPermit:     lo.ToPtr(resForCU(6)),
			Target:         resForCU(3),
			Metrics:        lo.ToPtr(metrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("3.9s")},
	})
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(3),
		Migrate: nil,
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
			Current:        resForCU(3),
			Target:         resForCU(2),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Do(state.Monitor().DownscaleRequestDenied, clock.Now(), expectedRevision.WithTime())
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
			LastPermit:     lo.ToPtr(resForCU(3)),
			Target:         resForCU(3),
			Metrics:        lo.ToPtr(metrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1s")}, // still waiting on retrying vm-monitor downscaling
	})
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(3),
		Migrate: nil,
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
				Current:        resForCU(3),
				Target:         resForCU(cu + 1),
				TargetRevision: expectedRevision.WithTime(),
			}
		}

		a.Call(nextActions).Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: currentPluginWait},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current:        resForCU(cu + 1),
				Target:         resForCU(cu),
				TargetRevision: expectedRevision.WithTime(),
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
		a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now(), expectedRevision.WithTime())
	}
	// Still waiting on plugin request tick, but we can make a NeonVM request to enact the
	// downscaling right away !
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("5.8s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current:        resForCU(3),
			Target:         resForCU(1),
			TargetRevision: expectedRevision.WithTime(),
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
			LastPermit:     lo.ToPtr(resForCU(3)),
			Target:         resForCU(1),
			Metrics:        lo.ToPtr(metrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(1))
	a.Call(nextActions).Equals(core.ActionSet{
		// not waiting on anything!
	})
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(1),
		Migrate: nil,
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
	expectedRevision := helpers.NewExpectedRevision(clock.Now)
	resForCU := DefaultComputeUnit.Mul

	latencyObserver := &latencyObserver{t: t, observations: nil}
	defer latencyObserver.assertEmpty()
	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithConfigSetting(func(c *core.Config) {
			c.RevisionSource = revsource.NewRevisionSource(0, latencyObserver.observe)
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
	lastMetrics := core.SystemMetrics{
		LoadAverage1Min:   0.0,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  0.0,
		MemoryCachedBytes: 0.0,
	}
	a.Do(state.UpdateSystemMetrics, lastMetrics)

	// Check we're not supposed to do anything
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")},
	})

	// Have the vm-monitor request upscaling:
	a.Do(state.Monitor().UpscaleRequested, clock.Now(), api.MoreResources{Cpu: false, Memory: true})
	// Revision advances
	expectedRevision.Value = 1
	expectedRevision.Flags = revsource.Upscale

	// First need to check with the scheduler plugin to get approval for upscaling:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("6s")}, // if nothing else happens, requested upscale expires.
		PluginRequest: &core.ActionPluginRequest{
			LastPermit:     lo.ToPtr(resForCU(1)),
			Target:         resForCU(2),
			Metrics:        lo.ToPtr(lastMetrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("5.9s")}, // same waiting for requested upscale expiring
	})
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(2),
		Migrate: nil,
	})

	// After approval from the scheduler plugin, now need to make NeonVM request:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")}, // plugin tick wait is earlier than requested upscale expiration
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current:        resForCU(1),
			Target:         resForCU(2),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())

	// Update the VM to set current=1
	a.Do(state.UpdatedVM, helpers.CreateVmInfo(
		DefaultInitialStateConfig.VM,
		helpers.WithCurrentCU(2),
		helpers.WithCurrentRevision(expectedRevision.WithTime()),
	))
	latencyObserver.assert(duration("0.2s"), revsource.Upscale)

	// Finally, tell the vm-monitor that it got upscaled:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")}, // still waiting on plugin tick
		MonitorUpscale: &core.ActionMonitorUpscale{
			Current:        resForCU(1),
			Target:         resForCU(2),
			TargetRevision: expectedRevision.WithTime(),
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
			LastPermit:     lo.ToPtr(resForCU(2)),
			Target:         resForCU(2),
			Metrics:        lo.ToPtr(lastMetrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("0.9s")}, // waiting for requested upscale expiring
	})
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(2),
		Migrate: nil,
	})

	// Still should just be waiting on vm-monitor upscale expiring
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("0.9s")},
	})
	clock.Inc(duration("0.9s"))
	// Upscale expired, revision advances
	expectedRevision.Value = 2
	expectedRevision.Flags = revsource.Downscale
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4s")}, // now, waiting on plugin request tick
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current:        resForCU(2),
			Target:         resForCU(1),
			TargetRevision: expectedRevision.WithTime(),
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
	var expectedRevision *helpers.ExpectedRevision
	latencyObserver := &latencyObserver{t: t, observations: nil}
	defer latencyObserver.assertEmpty()
	resForCU := DefaultComputeUnit.Mul

	var state *core.State
	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	initialMetrics := core.SystemMetrics{
		LoadAverage1Min:   0.0,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  0.0,
		MemoryCachedBytes: 0.0,
	}
	newMetrics := core.SystemMetrics{
		LoadAverage1Min:   0.3,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  0.0,
		MemoryCachedBytes: 0.0,
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

						TargetRevision: expectedRevision.WithTime(),
					},
				})
				a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(1))
				halfClockTick()
				midRequest()
				halfClockTick()
				*pluginWait -= clockTickDuration
				t.Log(" > finish vm-monitor downscale")
				a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now(), expectedRevision.WithTime())
			},
			post: func(pluginWait *time.Duration) {
				expectedRevision.Value = 2
				t.Log(" > start vm-monitor upscale")
				a.Call(nextActions).Equals(core.ActionSet{
					Wait: &core.ActionWait{Duration: *pluginWait},
					MonitorUpscale: &core.ActionMonitorUpscale{
						Current:        resForCU(1),
						Target:         resForCU(2),
						TargetRevision: expectedRevision.WithTime(),
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
						Current:        resForCU(2),
						Target:         resForCU(1),
						TargetRevision: expectedRevision.WithTime(),
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
						Current:        resForCU(1),
						Target:         resForCU(2),
						TargetRevision: expectedRevision.WithTime(),
					},
				})
				a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
				clockTick()
				*pluginWait -= clockTickDuration
				t.Log(" > finish NeonVM upscale")
				a.Do(state.NeonVM().RequestSuccessful, clock.Now())
			},
		},
		// NeonVM propagation
		{
			pre: func(_ *time.Duration, midAction func()) {
				a.Do(state.UpdatedVM, helpers.CreateVmInfo(
					DefaultInitialStateConfig.VM,
					helpers.WithCurrentCU(1),
					helpers.WithCurrentRevision(expectedRevision.WithTime()),
				))
				latencyObserver.assert(duration("0.2s"), revsource.Downscale)
				midAction()
			},
			post: func(_ *time.Duration) {
				// No action
			},
		},
		// plugin requests
		{
			pre: func(pluginWait *time.Duration, midRequest func()) {
				t.Log(" > start plugin downscale")
				a.Call(nextActions).Equals(core.ActionSet{
					PluginRequest: &core.ActionPluginRequest{
						LastPermit:     lo.ToPtr(resForCU(2)),
						Target:         resForCU(1),
						Metrics:        lo.ToPtr(initialMetrics.ToAPI()),
						TargetRevision: expectedRevision.WithTime(),
					},
				})
				a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(1))
				halfClockTick()
				midRequest()
				halfClockTick()
				*pluginWait = duration("4.9s") // reset because we just made a request
				t.Log(" > finish plugin downscale")
				a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
					Permit:  resForCU(1),
					Migrate: nil,
				})
			},
			post: func(pluginWait *time.Duration) {
				t.Log(" > start plugin upscale")
				a.Call(nextActions).Equals(core.ActionSet{
					PluginRequest: &core.ActionPluginRequest{
						LastPermit:     lo.ToPtr(resForCU(1)),
						Target:         resForCU(2),
						Metrics:        lo.ToPtr(newMetrics.ToAPI()),
						TargetRevision: expectedRevision.WithTime(),
					},
				})
				a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
				clockTick()
				*pluginWait = duration("4.9s") // reset because we just made a request
				t.Log(" > finish plugin upscale")
				a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
					Permit:  resForCU(2),
					Migrate: nil,
				})
			},
		},
	}

	for i := 0; i < len(steps); i++ {
		t.Logf("iter(%d)", i)

		// Initial setup
		clock = helpers.NewFakeClock(t)
		expectedRevision = helpers.NewExpectedRevision(clock.Now)
		state = helpers.CreateInitialState(
			DefaultInitialStateConfig,
			helpers.WithStoredWarnings(a.StoredWarnings()),
			helpers.WithMinMaxCU(1, 3),
			helpers.WithCurrentCU(2),
			helpers.WithConfigSetting(func(c *core.Config) {
				c.RevisionSource = revsource.NewRevisionSource(0, latencyObserver.observe)
			}),
		)

		state.Monitor().Active(true)

		doInitialPluginRequest(a, state, clock, duration("0.1s"), nil, resForCU(2))

		clockTick().AssertEquals(duration("0.2s"))
		pluginWait := duration("4.8s")

		a.Do(state.UpdateSystemMetrics, initialMetrics)
		// double-check that we agree about the desired resources
		a.Call(getDesiredResources, state, clock.Now()).
			Equals(resForCU(1))

		// We start with downscale
		expectedRevision.Value = 1
		expectedRevision.Flags = revsource.Downscale

		for j := 0; j <= i; j++ {
			midRequest := func() {}
			if j == i {
				// at the midpoint, start backtracking by setting the metrics
				midRequest = func() {
					t.Log(" > > updating metrics mid-request")
					a.Do(state.UpdateSystemMetrics, newMetrics)
					a.Call(getDesiredResources, state, clock.Now()).
						Equals(resForCU(2))
				}
			}

			steps[j].pre(&pluginWait, midRequest)
		}

		for j := i; j >= 0; j-- {
			// Now it is upscale
			expectedRevision.Value = 2
			expectedRevision.Flags = revsource.Upscale

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
	expectedRevision := helpers.NewExpectedRevision(clock.Now)
	latencyObserver := &latencyObserver{t: t, observations: nil}
	defer latencyObserver.assertEmpty()
	resForCU := DefaultComputeUnit.Mul

	state := helpers.CreateInitialState(
		DefaultInitialStateConfig,
		helpers.WithStoredWarnings(a.StoredWarnings()),
		helpers.WithMinMaxCU(1, 3),
		helpers.WithCurrentCU(2),
		helpers.WithConfigSetting(func(config *core.Config) {
			config.RevisionSource = revsource.NewRevisionSource(0, latencyObserver.observe)
		}),
	)
	nextActions := func() core.ActionSet {
		return state.NextActions(clock.Now())
	}

	state.Monitor().Active(true)

	// Send initial scheduler request:
	doInitialPluginRequest(a, state, clock, duration("0.1s"), nil, resForCU(2))

	clockTick()

	// Set metrics so the desired resources are still 2 CU
	metrics := core.SystemMetrics{
		LoadAverage1Min:   0.3,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  0.0,
		MemoryCachedBytes: 0.0,
	}
	a.Do(state.UpdateSystemMetrics, metrics)
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
	expectedRevision.Value += 1
	expectedRevision.Flags = revsource.Downscale
	// TODO: In the future, we should have a "force-downscale" alternative so the vm-monitor doesn't
	// get to deny the downscaling.
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.7s")},
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current: resForCU(2),
			Target:  resForCU(1),

			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(1))
	clockTick()
	a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now(), expectedRevision.WithTime())
	// Do NeonVM request for that downscaling
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.6s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current: resForCU(2),
			Target:  resForCU(1),

			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(1))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())
	// Do plugin request for that downscaling:
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit:     lo.ToPtr(resForCU(2)),
			Target:         resForCU(1),
			Metrics:        lo.ToPtr(metrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(1))
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(1),
		Migrate: nil,
	})

	// Update the VM to set currentCU==1 CU
	clockTick()
	a.Do(state.UpdatedVM, helpers.CreateVmInfo(
		DefaultInitialStateConfig.VM,
		helpers.WithCurrentCU(1),
		helpers.WithMinMaxCU(1, 1),
		helpers.WithCurrentRevision(expectedRevision.WithTime()),
	))

	latencyObserver.assert(duration("0.4s"), revsource.Downscale)

	// And then, we shouldn't need to do anything else:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")},
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
	expectedRevision := helpers.NewExpectedRevision(clock.Now)
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
	metrics := core.SystemMetrics{
		LoadAverage1Min:   0.3,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  0.0,
		MemoryCachedBytes: 0.0,
	}
	a.Do(state.UpdateSystemMetrics, metrics)
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
			LastPermit:     lo.ToPtr(resForCU(2)),
			Target:         resForCU(3),
			Metrics:        lo.ToPtr(metrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(3),
		Migrate: nil,
	})
	// Do NeonVM request for the upscaling
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")},
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current:        resForCU(2),
			Target:         resForCU(3),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(3))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())
	// Do vm-monitor upscale request
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.8s")},
		MonitorUpscale: &core.ActionMonitorUpscale{
			Current:        resForCU(2),
			Target:         resForCU(3),
			TargetRevision: expectedRevision.WithTime(),
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
	expectedRevision := helpers.NewExpectedRevision(clock.Now)
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
	metrics := core.SystemMetrics{
		LoadAverage1Min:   0.3,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  0.0,
		MemoryCachedBytes: 0.0,
	}
	a.Do(state.UpdateSystemMetrics, metrics)

	// We should be asking the scheduler for upscaling
	a.Call(nextActions).Equals(core.ActionSet{
		PluginRequest: &core.ActionPluginRequest{
			LastPermit:     lo.ToPtr(resForCU(1)),
			Target:         resForCU(2),
			Metrics:        lo.ToPtr(metrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
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
			LastPermit:     lo.ToPtr(resForCU(1)),
			Target:         resForCU(2),
			Metrics:        lo.ToPtr(metrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(2),
		Migrate: nil,
	})

	// Now, after plugin request is successful, we should be making a request to NeonVM.
	// We'll have that request fail the first time as well:
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")}, // plugin request tick
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current:        resForCU(1),
			Target:         resForCU(2),
			TargetRevision: expectedRevision.WithTime(),
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
			Current:        resForCU(1),
			Target:         resForCU(2),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
	clockTick()
	a.Do(state.NeonVM().RequestSuccessful, clock.Now())

	// And then finally, we should be looking to inform the vm-monitor about this upscaling.
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("1.7s")}, // plugin request tick
		MonitorUpscale: &core.ActionMonitorUpscale{
			Current:        resForCU(1),
			Target:         resForCU(2),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
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
	expectedRevision := helpers.NewExpectedRevision(clock.Now)
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
		WithWarnings(
			"Making scaling decision without all required metrics available",
			"Wanted to send vm-monitor downscale request, but there's no active connection",
		).
		Call(state.NextActions, clock.Now()).
		Equals(core.ActionSet{
			PluginRequest: &core.ActionPluginRequest{
				LastPermit:     nil,
				Target:         resForCU(3),
				Metrics:        nil,
				TargetRevision: expectedRevision.WithTime(),
			},
		})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(3))
	clockTick()
	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(3),
		Migrate: nil,
	})

	clockTick()

	// Monitor's now active, so we should be asking it for downscaling.
	// We don't yet have metrics though, so we only want to downscale as much as is required.
	state.Monitor().Active(true)
	a.
		WithWarnings("Making scaling decision without all required metrics available").
		Call(nextActions).
		Equals(core.ActionSet{
			Wait: &core.ActionWait{Duration: duration("4.8s")},
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current:        resForCU(3),
				Target:         resForCU(2),
				TargetRevision: expectedRevision.WithTime(),
			},
		})
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(2))

	// In the middle of the vm-monitor request, update the metrics so that now the desired resource
	// usage is actually 1 CU
	clockTick()
	// the actual metrics we got in the actual logs
	metrics := core.SystemMetrics{
		LoadAverage1Min:   0.0,
		LoadAverage5Min:   0.0,
		MemoryUsageBytes:  150589570, // 143.6 MiB
		MemoryCachedBytes: 0.0,
	}
	a.Do(state.UpdateSystemMetrics, metrics)

	// nothing to do yet, until the existing vm-monitor request finishes
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.7s")}, // plugin request tick wait
	})

	clockTick()

	// When the vm-monitor request finishes, we want to both
	// (a) request additional downscaling from vm-monitor, and
	// (b) make a NeonVM request for the initially approved downscaling
	a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now(), expectedRevision.WithTime())
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.6s")}, // plugin request tick wait
		NeonVMRequest: &core.ActionNeonVMRequest{
			Current:        resForCU(3),
			Target:         resForCU(2),
			TargetRevision: expectedRevision.WithTime(),
		},
		MonitorDownscale: &core.ActionMonitorDownscale{
			Current:        resForCU(2),
			Target:         resForCU(1),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	// Start both requests. The vm-monitor request will finish first, but after that we'll just be
	// waiting on the NeonVM request (and then redoing a follow-up for more downscaling).
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(2))
	a.Do(state.Monitor().StartingDownscaleRequest, clock.Now(), resForCU(1))

	clockTick()

	a.Do(state.Monitor().DownscaleRequestAllowed, clock.Now(), expectedRevision.WithTime())
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
	state.Debug(true)
	a.
		Call(nextActions).
		Equals(core.ActionSet{
			// At this point in the original logs from staging, the intended request to the plugin was
			// incorrectly for 1 CU, rather than 2 CU. So, the rest of this test case is mostly just
			// rounding out the rest of the scale-down routine.
			PluginRequest: &core.ActionPluginRequest{
				LastPermit:     lo.ToPtr(resForCU(3)),
				Target:         resForCU(2),
				Metrics:        lo.ToPtr(metrics.ToAPI()),
				TargetRevision: expectedRevision.WithTime(),
			},
			NeonVMRequest: &core.ActionNeonVMRequest{
				Current:        resForCU(2),
				Target:         resForCU(1),
				TargetRevision: expectedRevision.WithTime(),
			},
		})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(2))
	a.Do(state.NeonVM().StartingRequest, clock.Now(), resForCU(1))

	clockTick()

	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(2),
		Migrate: nil,
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
			LastPermit:     lo.ToPtr(resForCU(2)),
			Target:         resForCU(1),
			Metrics:        lo.ToPtr(metrics.ToAPI()),
			TargetRevision: expectedRevision.WithTime(),
		},
	})
	a.Do(state.Plugin().StartingRequest, clock.Now(), resForCU(1))

	clockTick()

	a.NoError(state.Plugin().RequestSuccessful, clock.Now(), expectedRevision.WithTime(), api.PluginResponse{
		Permit:  resForCU(1),
		Migrate: nil,
	})
	// Nothing left to do
	a.Call(nextActions).Equals(core.ActionSet{
		Wait: &core.ActionWait{Duration: duration("4.9s")}, // plugin request tick wait
	})
}
