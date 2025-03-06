package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/agent/core/revsource"
	"github.com/neondatabase/autoscaling/pkg/agent/executor"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestRaceConditionPluginVsNeonVM(t *testing.T) {
	slotSize := api.Bytes(1 << 30 /* 1 Gi */)

	logger, err := zap.NewDevelopment()
	assert.NoError(t, err)

	execLogger := logger.Named("exec")
	coreExecLogger := execLogger.Named("core")

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
				Use:      uint16(2),
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
			PluginRequestTick:                  time.Second,
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

	r := &Runner{
		global: nil,
		status: nil, // set by caller

		shutdown: nil, // set by (*Runner).Run
		vmName:   vmInfo.NamespacedName(),
		podName: util.NamespacedName{
			Name: "podName",
		},
		podIP:       "1.1.1.1",
		memSlotSize: vmInfo.Mem.SlotSize,
		lock:        util.NewChanMutex(),

		executorStateDump: nil, // set by (*Runner).Run

		monitor: nil,

		backgroundWorkerCount: atomic.Int64{},
		backgroundPanic:       make(chan error),
	}

	pluginIface := makePluginInterface(r)
	neonvmIface := makeNeonVMInterface(r)

	// "ecwc" stands for "ExecutorCoreWithClients"
	ecwc := executorCore.WithClients(executor.ClientSet{
		Plugin:  pluginIface,
		NeonVM:  neonvmIface,
		Monitor: nil,
	})

	ctx := context.Background()
	r.spawnBackgroundWorker(ctx, execLogger.Named("plugin"), "executor: plugin", ecwc.DoPluginRequests)
	r.spawnBackgroundWorker(ctx, execLogger.Named("neonvm"), "executor: neonvm", ecwc.DoNeonVMRequests)

	select {
	case <-ctx.Done():
		return
	case err := <-r.backgroundPanic:
		panic(err)
	}
}
