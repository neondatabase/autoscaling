package core

// extracted components of how "goal CU" is determined

import (
	"math"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type scalingGoal struct {
	hasAllMetrics bool
	goalCU        uint32
}

func calculateGoalCU(
	warn func(string),
	cfg api.ScalingConfig,
	computeUnit api.Resources,
	systemMetrics *SystemMetrics,
	lfcMetrics *LFCMetrics,
) (scalingGoal, []zap.Field) {
	hasAllMetrics := systemMetrics != nil && (!*cfg.EnableLFCMetrics || lfcMetrics != nil)
	if !hasAllMetrics {
		warn("Making scaling decision without all required metrics available")
	}

	var goalCU uint32
	var logFields []zap.Field

	if systemMetrics != nil {
		cpuGoalCU := calculateCPUGoalCU(cfg, computeUnit, *systemMetrics)
		goalCU = max(goalCU, cpuGoalCU)

		memGoalCU := calculateMemGoalCU(cfg, computeUnit, *systemMetrics)

		goalCU = max(goalCU, memGoalCU)
	}

	if lfcMetrics != nil {
		lfcGoalCU, lfcLogFunc := calculateLFCGoalCU(warn, cfg, computeUnit, *lfcMetrics)
		goalCU = max(goalCU, lfcGoalCU)
		if lfcLogFunc != nil {
			logFields = append(logFields, zap.Object("lfc", zapcore.ObjectMarshalerFunc(lfcLogFunc)))
		}
	}

	return scalingGoal{hasAllMetrics: hasAllMetrics, goalCU: goalCU}, logFields
}

// For CPU:
// Goal compute unit is at the point where (CPUs) × (LoadAverageFractionTarget) == (load average),
// which we can get by dividing LA by LAFT, and then dividing by the number of CPUs per CU
func calculateCPUGoalCU(
	cfg api.ScalingConfig,
	computeUnit api.Resources,
	systemMetrics SystemMetrics,
) uint32 {
	goalCPUs := systemMetrics.LoadAverage1Min / *cfg.LoadAverageFractionTarget
	cpuGoalCU := uint32(math.Round(goalCPUs / computeUnit.VCPU.AsFloat64()))
	return cpuGoalCU
}

// For Mem:
// Goal compute unit is at the point where (Mem) * (MemoryUsageFractionTarget) == (Mem Usage)
// We can get the desired memory allocation in bytes by dividing MU by MUFT, and then convert
// that to CUs.
func calculateMemGoalCU(
	cfg api.ScalingConfig,
	computeUnit api.Resources,
	systemMetrics SystemMetrics,
) uint32 {
	memGoalBytes := api.Bytes(math.Round(systemMetrics.MemoryUsageBytes / *cfg.MemoryUsageFractionTarget))
	memGoalCU := uint32(memGoalBytes / computeUnit.Mem)
	return memGoalCU
}

func calculateLFCGoalCU(
	warn func(string),
	cfg api.ScalingConfig,
	computeUnit api.Resources,
	lfcMetrics LFCMetrics,
) (uint32, func(zapcore.ObjectEncoder) error) {
	wssValues := lfcMetrics.ApproximateworkingSetSizeBuckets
	// At this point, we can assume that the values are equally spaced at 1 minute apart,
	// starting at 1 minute.
	offsetIndex := *cfg.LFCMinWaitBeforeDownscaleMinutes - 1 // -1 because values start at 1m
	windowSize := *cfg.LFCWindowSizeMinutes
	// Handle invalid metrics:
	if len(wssValues) < offsetIndex+windowSize {
		warn("not enough working set size values to make scaling determination")
		return 0, nil
	} else {
		estimateWss := EstimateTrueWorkingSetSize(wssValues, WssEstimatorConfig{
			MaxAllowedIncreaseFactor: 3.0, // hard-code this for now.
			InitialOffset:            offsetIndex,
			WindowSize:               windowSize,
		})
		projectSliceEnd := offsetIndex // start at offsetIndex to avoid panics if not monotonically non-decreasing
		for ; projectSliceEnd < len(wssValues) && wssValues[projectSliceEnd] <= estimateWss; projectSliceEnd++ {
		}
		projectLen := 0.5 // hard-code this for now.
		predictedHighestNextMinute := ProjectNextHighest(wssValues[:projectSliceEnd], projectLen)

		// predictedHighestNextMinute is still in units of 8KiB pages. Let's convert that
		// into GiB, then convert that into CU, and then invert the discount from only some
		// of the memory going towards LFC to get the actual CU required to fit the
		// predicted working set size.
		requiredCU := predictedHighestNextMinute * 8192 / computeUnit.Mem.AsFloat64() / *cfg.LFCToMemoryRatio
		lfcGoalCU := uint32(math.Ceil(requiredCU))

		lfcLogFields := func(obj zapcore.ObjectEncoder) error {
			obj.AddFloat64("estimateWssPages", estimateWss)
			obj.AddFloat64("predictedNextWssPages", predictedHighestNextMinute)
			obj.AddFloat64("requiredCU", requiredCU)
			return nil
		}

		return lfcGoalCU, lfcLogFields
	}
}
