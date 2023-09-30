package core_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slices"

	"k8s.io/apimachinery/pkg/api/resource"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

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

type initialStateParams struct {
	computeUnit api.Resources
	minCU       uint16
	maxCU       uint16
}

type initialStateOpt struct {
	preCreate  func(*initialStateParams)
	postCreate func(*api.VmInfo, *core.Config)
}

func withStoredWarnings(warnings *[]string) (o initialStateOpt) {
	o.postCreate = func(_ *api.VmInfo, config *core.Config) {
		config.Warn = func(format string, args ...any) {
			*warnings = append(*warnings, fmt.Sprintf(format, args...))
		}
	}
	return
}

func createInitialState(opts ...initialStateOpt) *core.State {
	pre := initialStateParams{
		computeUnit: api.Resources{VCPU: 250, Mem: 1},
		minCU:       1,
		maxCU:       4,
	}
	for _, o := range opts {
		if o.preCreate != nil {
			o.preCreate(&pre)
		}
	}

	vm := api.VmInfo{
		Name:      "test",
		Namespace: "test",
		Cpu: api.VmCpuInfo{
			Min: vmapi.MilliCPU(pre.minCU) * pre.computeUnit.VCPU,
			Use: vmapi.MilliCPU(pre.minCU) * pre.computeUnit.VCPU,
			Max: vmapi.MilliCPU(pre.maxCU) * pre.computeUnit.VCPU,
		},
		Mem: api.VmMemInfo{
			SlotSize: resource.NewQuantity(1<<30 /* 1 Gi */, resource.BinarySI),
			Min:      pre.minCU * pre.computeUnit.Mem,
			Use:      pre.minCU * pre.computeUnit.Mem,
			Max:      pre.maxCU * pre.computeUnit.Mem,
		},
		ScalingConfig:  nil,
		AlwaysMigrate:  false,
		ScalingEnabled: true,
	}

	config := core.Config{
		DefaultScalingConfig: api.ScalingConfig{
			LoadAverageFractionTarget: 0.5,
			MemoryUsageFractionTarget: 0.5,
		},
		PluginRequestTick:              5 * time.Second,
		PluginDeniedRetryWait:          2 * time.Second,
		MonitorDeniedDownscaleCooldown: 5 * time.Second,
		MonitorRetryWait:               3 * time.Second,
		Warn:                           func(string, ...any) {},
	}

	for _, o := range opts {
		if o.postCreate != nil {
			o.postCreate(&vm, &config)
		}
	}

	return core.NewState(vm, config)
}

type fakeClock struct {
	base time.Time
	now  time.Time
}

func newFakeClock() *fakeClock {
	base, err := time.Parse(time.RFC3339, "2000-01-01T00:00:00Z") // a nice round number, to make things easier
	if err != nil {
		panic(err)
	}

	return &fakeClock{base: base, now: base}
}

func (c *fakeClock) inc(duration time.Duration) {
	c.now = c.now.Add(duration)
}

func (c *fakeClock) elapsed() time.Duration {
	return c.now.Sub(c.base)
}

func Test_NextActions(t *testing.T) {
	simulateInitialSchedulerRequest := func(t *testing.T, state *core.State, clock *fakeClock, reqTime time.Duration) {
		state.Plugin().NewScheduler()

		actions := state.NextActions(clock.now)
		require.NotNil(t, actions.PluginRequest)
		action := actions.PluginRequest
		require.Nil(t, action.LastPermit)
		state.Plugin().StartingRequest(clock.now, action.Target)
		clock.inc(reqTime)
		require.NoError(t, state.Plugin().RequestSuccessful(clock.now, api.PluginResponse{
			Permit:      action.Target,
			Migrate:     nil,
			ComputeUnit: api.Resources{VCPU: 250, Mem: 1}, // TODO: make this configurable... somehow.
		}))
	}

	// Thorough checks of a relatively simple flow
	t.Run("BasicScaleupAndDownFlow", func(t *testing.T) {
		warnings := []string{}
		clock := newFakeClock()
		state := createInitialState(
			withStoredWarnings(&warnings),
		)

		hundredMillis := 100 * time.Millisecond

		state.Plugin().NewScheduler()
		state.Monitor().Active(true)

		simulateInitialSchedulerRequest(t, state, clock, hundredMillis)
		require.Equal(t, warnings, []string{"Can't determine desired resources because compute unit hasn't been set yet"})
		warnings = nil // reset

		clock.inc(hundredMillis)
		metrics := api.Metrics{
			LoadAverage1Min:  0.3,
			LoadAverage5Min:  0.0, // unused
			MemoryUsageBytes: 0.0,
		}
		state.UpdateMetrics(metrics)
		require.Equal(t, clock.elapsed(), 2*hundredMillis)
		// double-check that we agree about the desired resources
		desiredResources, _ := state.DesiredResourcesFromMetricsOrRequestedUpscaling(clock.now)
		require.Equal(t, api.Resources{VCPU: 500, Mem: 2}, desiredResources)
		require.Empty(t, warnings)

		// Now that the initial scheduler request is done, and we have metrics that indicate
		// scale-up would be a good idea, we should be contacting the scheduler to get approval.
		actions := state.NextActions(clock.now)
		require.Equal(t, core.ActionSet{
			Wait: nil,
			PluginRequest: &core.ActionPluginRequest{
				LastPermit: &api.Resources{VCPU: 250, Mem: 1},
				Target:     api.Resources{VCPU: 500, Mem: 2},
				Metrics:    &metrics,
			},
			// shouldn't have anything to say to the other components
			NeonVMRequest:    nil,
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, actions)
		require.Empty(t, warnings)
		// start the request:
		state.Plugin().StartingRequest(clock.now, actions.PluginRequest.Target)
		clock.inc(hundredMillis)
		// should have nothing more to do; waiting on plugin request to come back
		require.Equal(t, core.ActionSet{
			Wait:             nil,
			PluginRequest:    nil,
			NeonVMRequest:    nil,
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, state.NextActions(clock.now))
		require.Empty(t, warnings)
		require.NoError(t, state.Plugin().RequestSuccessful(clock.now, api.PluginResponse{
			Permit:      api.Resources{VCPU: 500, Mem: 2},
			Migrate:     nil,
			ComputeUnit: api.Resources{VCPU: 250, Mem: 1},
		}))
		require.Empty(t, warnings)
		require.Equal(t, clock.elapsed(), 3*hundredMillis)

		// Scheduler approval is done, now we should be making the request to NeonVM
		actions = state.NextActions(clock.now)
		require.Equal(t, core.ActionSet{
			// expected to make a scheduler request every 5s; it's been 100ms since the last one, so
			// if the NeonVM request didn't come back in time, we'd need to get woken up to start
			// the next scheduler request.
			Wait: &core.ActionWait{
				Duration: 5*time.Second - hundredMillis,
			},
			PluginRequest: nil,
			NeonVMRequest: &core.ActionNeonVMRequest{
				Current: api.Resources{VCPU: 250, Mem: 1},
				Target:  api.Resources{VCPU: 500, Mem: 2},
			},
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, actions)
		require.Empty(t, warnings)
		// start the request:
		state.NeonVM().StartingRequest(clock.now, actions.NeonVMRequest.Target)
		clock.inc(hundredMillis)
		// should have nothing more to do; waiting on NeonVM request to come back
		require.Equal(t, core.ActionSet{
			Wait: &core.ActionWait{
				Duration: 5*time.Second - 2*hundredMillis,
			},
			PluginRequest:    nil,
			NeonVMRequest:    nil,
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, state.NextActions(clock.now))
		require.Empty(t, warnings)
		state.NeonVM().RequestSuccessful(clock.now)
		require.Empty(t, warnings)
		require.Equal(t, clock.elapsed(), 4*hundredMillis)

		// NeonVM change is done, now we should finish by notifying the vm-monitor
		actions = state.NextActions(clock.now)
		require.Equal(t, core.ActionSet{
			Wait: &core.ActionWait{
				Duration: 5*time.Second - 2*hundredMillis, // same as previous, clock hasn't changed
			},
			PluginRequest:    nil,
			NeonVMRequest:    nil,
			MonitorDownscale: nil,
			MonitorUpscale: &core.ActionMonitorUpscale{
				Current: api.Resources{VCPU: 250, Mem: 1},
				Target:  api.Resources{VCPU: 500, Mem: 2},
			},
		}, actions)
		require.Empty(t, warnings)
		// start the request:
		state.Monitor().StartingUpscaleRequest(clock.now, actions.MonitorUpscale.Target)
		clock.inc(hundredMillis)
		// should have nothing more to do; waiting on vm-monitor request to come back
		require.Equal(t, core.ActionSet{
			Wait: &core.ActionWait{
				Duration: 5*time.Second - 3*hundredMillis,
			},
			PluginRequest:    nil,
			NeonVMRequest:    nil,
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, state.NextActions(clock.now))
		require.Empty(t, warnings)
		state.Monitor().UpscaleRequestSuccessful(clock.now)
		require.Empty(t, warnings)
		require.Equal(t, clock.elapsed(), 5*hundredMillis)

		// And now, double-check that there's no sneaky follow-up actions before we change the
		// metrics
		actions = state.NextActions(clock.now)
		require.Equal(t, core.ActionSet{
			Wait: &core.ActionWait{
				Duration: 5*time.Second - 3*hundredMillis, // same as previous, clock hasn't changed
			},
			PluginRequest:    nil,
			NeonVMRequest:    nil,
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, actions)
		require.Empty(t, warnings)

		// ---- Scaledown !!! ----

		clock.inc(hundredMillis)
		require.Equal(t, clock.elapsed(), 6*hundredMillis)

		// Set metrics back so that desired resources should now be zero
		metrics = api.Metrics{
			LoadAverage1Min:  0.0,
			LoadAverage5Min:  0.0, // unused
			MemoryUsageBytes: 0.0,
		}
		state.UpdateMetrics(metrics)
		// double-check that we agree about the new desired resources
		desiredResources, _ = state.DesiredResourcesFromMetricsOrRequestedUpscaling(clock.now)
		require.Equal(t, api.Resources{VCPU: 250, Mem: 1}, desiredResources)
		require.Empty(t, warnings)

		// First step in downscaling is getting approval from the vm-monitor:
		actions = state.NextActions(clock.now)
		require.Equal(t, core.ActionSet{
			Wait: &core.ActionWait{
				Duration: 5*time.Second - 4*hundredMillis,
			},
			PluginRequest: nil,
			NeonVMRequest: nil,
			MonitorDownscale: &core.ActionMonitorDownscale{
				Current: api.Resources{VCPU: 500, Mem: 2},
				Target:  api.Resources{VCPU: 250, Mem: 1},
			},
			MonitorUpscale: nil,
		}, actions)
		require.Empty(t, warnings)
		state.Monitor().StartingDownscaleRequest(clock.now, actions.MonitorDownscale.Target)
		clock.inc(hundredMillis)
		// should have nothing more to do; waiting on vm-monitor request to come back
		require.Equal(t, core.ActionSet{
			Wait: &core.ActionWait{
				Duration: 5*time.Second - 5*hundredMillis,
			},
			PluginRequest:    nil,
			NeonVMRequest:    nil,
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, state.NextActions(clock.now))
		require.Empty(t, warnings)
		state.Monitor().DownscaleRequestAllowed(clock.now)
		require.Empty(t, warnings)
		require.Equal(t, clock.elapsed(), 7*hundredMillis)

		// After getting approval from the vm-monitor, we make the request to NeonVM to carry it out
		actions = state.NextActions(clock.now)
		require.Equal(t, core.ActionSet{
			Wait: &core.ActionWait{
				Duration: 5*time.Second - 5*hundredMillis, // same as previous, clock hasn't changed
			},
			PluginRequest: nil,
			NeonVMRequest: &core.ActionNeonVMRequest{
				Current: api.Resources{VCPU: 500, Mem: 2},
				Target:  api.Resources{VCPU: 250, Mem: 1},
			},
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, actions)
		require.Empty(t, warnings)
		state.NeonVM().StartingRequest(clock.now, actions.NeonVMRequest.Target)
		clock.inc(hundredMillis)
		// should have nothing more to do; waiting on NeonVM request to come back
		require.Equal(t, core.ActionSet{
			Wait: &core.ActionWait{
				Duration: 5*time.Second - 6*hundredMillis,
			},
			PluginRequest:    nil,
			NeonVMRequest:    nil,
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, state.NextActions(clock.now))
		require.Empty(t, warnings)
		state.NeonVM().RequestSuccessful(clock.now)
		require.Empty(t, warnings)
		require.Equal(t, clock.elapsed(), 8*hundredMillis)

		// Request to NeonVM completed, it's time to inform the scheduler plugin:
		actions = state.NextActions(clock.now)
		require.Equal(t, core.ActionSet{
			Wait: nil,
			PluginRequest: &core.ActionPluginRequest{
				LastPermit: &api.Resources{VCPU: 500, Mem: 2},
				Target:     api.Resources{VCPU: 250, Mem: 1},
				Metrics:    &metrics,
			},
			// shouldn't have anything to say to the other components
			NeonVMRequest:    nil,
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, actions)
		require.Empty(t, warnings)
		state.Plugin().StartingRequest(clock.now, actions.PluginRequest.Target)
		clock.inc(hundredMillis)
		// should have nothing more to do; waiting on plugin request to come back
		require.Equal(t, core.ActionSet{
			Wait:             nil, // and don't need to wait, because plugin req is ongoing
			PluginRequest:    nil,
			NeonVMRequest:    nil,
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, state.NextActions(clock.now))
		require.Empty(t, warnings)
		require.NoError(t, state.Plugin().RequestSuccessful(clock.now, api.PluginResponse{
			Permit:      api.Resources{VCPU: 250, Mem: 1},
			Migrate:     nil,
			ComputeUnit: api.Resources{VCPU: 250, Mem: 1},
		}))
		require.Empty(t, warnings)
		require.Equal(t, clock.elapsed(), 9*hundredMillis)

		// Finally, check there's no leftover actions:
		actions = state.NextActions(clock.now)
		require.Equal(t, core.ActionSet{
			Wait: &core.ActionWait{
				Duration: 5*time.Second - hundredMillis, // request that just finished was started 100ms ago
			},
			PluginRequest:    nil,
			NeonVMRequest:    nil,
			MonitorDownscale: nil,
			MonitorUpscale:   nil,
		}, actions)
		require.Empty(t, warnings)
	})
}
