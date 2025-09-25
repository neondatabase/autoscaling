package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/agent/core/revsource"
	"github.com/neondatabase/autoscaling/pkg/agent/executor"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

var (
	_ executor.PluginInterface = (*pluginMock)(nil)
	_ executor.NeonVMInterface = (*neonvmMock)(nil)
)

type pluginMock struct {
	requests  chan api.Resources
	responses chan api.Resources
}

func (p *pluginMock) Request(_ context.Context, logger *zap.Logger, lastPermit *api.Resources, target api.Resources, _ *api.Metrics) (*api.PluginResponse, error) {
	logger.Info("Plugin request", zap.Any("lastPermit", lastPermit), zap.Any("target", target))
	p.requests <- target
	return &api.PluginResponse{
		Permit: <-p.responses,
	}, nil
}

type neonvmMock struct {
	requests  chan api.Resources
	responses chan error
}

func (n *neonvmMock) Request(_ context.Context, logger *zap.Logger, current, target api.Resources, targetRevision vmv1.RevisionWithTime) error {
	logger.Info("NeonVM request", zap.Any("current", current), zap.Any("target", target), zap.Any("targetRevision", targetRevision))
	n.requests <- target
	return <-n.responses
}

func TestRaceConditionPluginVsNeonVM(t *testing.T) {
	slotSize := api.Bytes(1 << 30 /* 1 Gi */)

	logger, err := zap.NewDevelopment()
	assert.NoError(t, err)

	execLogger := logger.Named("exec")
	coreExecLogger := execLogger.Named("core")

	desiredResources := api.Resources{VCPU: 500, Mem: 2 * slotSize}

	makeVM := func() api.VmInfo {
		return api.VmInfo{
			Name:      "test",
			Namespace: "test",
			Cpu: api.VmCpuInfo{
				Min: 250,
				Use: 500,
				Max: 1000,
			},
			Mem: api.VmMemInfo{
				SlotSize: slotSize,
				Min:      1,
				Use:      uint16(1),
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
				LFCUseLargestWindow:              lo.ToPtr(false),
				LFCToMemoryRatio:                 lo.ToPtr(0.75),
				LFCWindowSizeMinutes:             lo.ToPtr(5),
				LFCMinWaitBeforeDownscaleMinutes: lo.ToPtr(5),
				CPUStableZoneRatio:               lo.ToPtr(0.0),
				CPUMixedZoneRatio:                lo.ToPtr(0.0),
			},
			// these don't really matter, because we're not using (*State).NextActions()
			NeonVMRetryWait:                    time.Second,
			PluginRequestTick:                  time.Second * 5,
			PluginRetryWait:                    time.Second,
			PluginDeniedRetryWait:              time.Second,
			MonitorDeniedDownscaleCooldown:     time.Second,
			MonitorRequestedUpscaleValidPeriod: time.Second,
			MonitorRetryWait:                   time.Second,
			Log: core.LogConfig{
				Info: nil,
				Warn: func(msg string, fields ...zap.Field) {
					// warnings = append(warnings, msg)
					logger.Warn(msg, fields...)
				},
			},
			RevisionSource: revsource.NewRevisionSource(0, nil),
			ObservabilityCallbacks: core.ObservabilityCallbacks{
				PluginLatency:       nil,
				MonitorLatency:      nil,
				NeonVMLatency:       nil,
				ActualScaling:       nil,
				HypotheticalScaling: nil,
			},
			DesiredResourcesHook: func() api.Resources {
				return desiredResources
			},
		}
	}

	coreConfig := makeStateConfig(false)

	vmInfo := makeVM()
	executorCore := executor.NewExecutorCore(coreExecLogger, vmInfo, executor.Config{
		OnNextActions: func() {},
		Core:          coreConfig,
	})

	stateDump := executorCore.StateDump()
	logger.Info("State Dump", zap.Any("state", stateDump))

	globalMetrics, _ := makeGlobalMetrics()
	r := &Runner{
		global: &agentState{
			metrics: globalMetrics,
		},
		status: nil, // set by caller

		shutdown:    nil, // set by (*Runner).Run
		vmName:      vmInfo.NamespacedName(),
		podName:     util.NamespacedName{},
		podIP:       "1.1.1.1",
		memSlotSize: vmInfo.Mem.SlotSize,
		lock:        util.NewChanMutex(),

		executorStateDump: nil, // set by (*Runner).Run

		monitor: nil,

		backgroundWorkerCount: atomic.Int64{},
		backgroundPanic:       make(chan error),
	}

	pluginIface := &pluginMock{
		requests:  make(chan api.Resources),
		responses: make(chan api.Resources),
	}
	neonvmIface := &neonvmMock{
		requests:  make(chan api.Resources),
		responses: make(chan error),
	}

	// "ecwc" stands for "ExecutorCoreWithClients"
	ecwc := executorCore.WithClients(executor.ClientSet{
		Plugin:  pluginIface,
		NeonVM:  neonvmIface,
		Monitor: nil,
	})

	ctx := context.Background()
	r.spawnBackgroundWorker(ctx, execLogger.Named("plugin"), "executor: plugin", ecwc.DoPluginRequests)
	r.spawnBackgroundWorker(ctx, execLogger.Named("neonvm"), "executor: neonvm", ecwc.DoNeonVMRequests)

	update := func() {
		ecwc.ExecutorCore.Updater().UpdateLFCMetrics(core.LFCMetrics{}, func() {})
	}

	waitForPluginRequest := func() api.Resources {
		for {
			update()
			select {
			case req := <-pluginIface.requests:
				return req
			case <-time.After(time.Second):
				continue
			}
		}
	}

	logger.Info("Dumping state", zap.Any("state", ecwc.StateDump()))

	// wait until workers are running
	time.Sleep(time.Second)
	desiredResources = api.Resources{VCPU: 500, Mem: 2 * slotSize}
	update()

	// 1. respond to a plugin request (0.5CPU, 1G) -> (0.5CPU, 2G)
	assert.Equal(t, api.Resources{VCPU: 500, Mem: 2 * slotSize}, <-pluginIface.requests)
	pluginIface.responses <- api.Resources{VCPU: 500, Mem: 2 * slotSize}

	// 2. receive a NeonVM request (0.5CPU, 1G) -> (0.5CPU, 2G), don't respond for now
	assert.Equal(t, api.Resources{VCPU: 500, Mem: 2 * slotSize}, <-neonvmIface.requests)

	// 3. now desired resources are (0.25CPU, 1G)
	desiredResources = api.Resources{VCPU: 250, Mem: 1 * slotSize}

	// 4. let's wait for plugin request (0.5CPU, 2G) -> (0.5CPU, 1G)
	assert.Equal(t, api.Resources{VCPU: 500, Mem: 1 * slotSize}, waitForPluginRequest())
	pluginIface.responses <- api.Resources{VCPU: 500, Mem: 1 * slotSize}

	// 5. respond to NeonVM request
	time.Sleep(time.Second)
	neonvmIface.responses <- nil

	// 6. Panic in clampResources

	select {
	case <-ctx.Done():
		return
	case err := <-r.backgroundPanic:
		panic(err)
	}
}
