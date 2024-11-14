package core

// extracted components of how "goal CU" is determined

import (
	"math"

	"github.com/samber/lo"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/neondatabase/autoscaling/pkg/api"
)

type ScalingGoal struct {
	HasAllMetrics bool
	GoalCU        uint32
}

type StdAlgorithm struct{}

func DefaultAlgorithm() *StdAlgorithm {
	return &StdAlgorithm{}
}

func (*StdAlgorithm) CalculateGoalCU(
	warn func(string),
	cfg api.ScalingConfig,
	computeUnit api.Resources,
	systemMetrics *SystemMetrics,
	lfcMetrics *LFCMetrics,
) (ScalingGoal, []zap.Field) {
	hasAllMetrics := systemMetrics != nil && (!*cfg.EnableLFCMetrics || lfcMetrics != nil)
	if !hasAllMetrics {
		warn("Making scaling decision without all required metrics available")
	}

	var lfcGoalCU, cpuGoalCU, memGoalCU, memTotalGoalCU uint32
	var logFields []zap.Field

	var wss *api.Bytes // estimated working set size

	if lfcMetrics != nil {
		var lfcLogFunc func(zapcore.ObjectEncoder) error
		lfcGoalCU, wss, lfcLogFunc = calculateLFCGoalCU(warn, cfg, computeUnit, *lfcMetrics)
		if lfcLogFunc != nil {
			logFields = append(logFields, zap.Object("lfc", zapcore.ObjectMarshalerFunc(lfcLogFunc)))
		}
	}

	if systemMetrics != nil {
		cpuGoalCU = calculateCPUGoalCU(cfg, computeUnit, *systemMetrics)

		memGoalCU = calculateMemGoalCU(cfg, computeUnit, *systemMetrics)
	}

	if systemMetrics != nil && wss != nil {
		memTotalGoalCU = calculateMemTotalGoalCU(cfg, computeUnit, *systemMetrics, *wss)
	}

	goalCU := max(cpuGoalCU, memGoalCU, memTotalGoalCU, lfcGoalCU)

	return ScalingGoal{HasAllMetrics: hasAllMetrics, GoalCU: goalCU}, logFields
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
	// goal memory size, just looking at allocated memory (not including page cache...)
	memGoalBytes := api.Bytes(math.Round(systemMetrics.MemoryUsageBytes / *cfg.MemoryUsageFractionTarget))

	// note: this is equal to ceil(memGoalBytes / computeUnit.Mem), because ceil(X/M) == floor((X+M-1)/M)
	memGoalCU := uint32((memGoalBytes + computeUnit.Mem - 1) / computeUnit.Mem)
	return memGoalCU
}

// goal memory size, looking at allocated memory and min(page cache usage, LFC working set size)
func calculateMemTotalGoalCU(
	cfg api.ScalingConfig,
	computeUnit api.Resources,
	systemMetrics SystemMetrics,
	wss api.Bytes,
) uint32 {
	lfcCached := min(float64(wss), systemMetrics.MemoryCachedBytes)
	totalGoalBytes := api.Bytes((lfcCached + systemMetrics.MemoryUsageBytes) / *cfg.MemoryTotalFractionTarget)

	memTotalGoalCU := uint32((totalGoalBytes + computeUnit.Mem - 1) / computeUnit.Mem)
	return memTotalGoalCU
}

func calculateLFCGoalCU(
	warn func(string),
	cfg api.ScalingConfig,
	computeUnit api.Resources,
	lfcMetrics LFCMetrics,
) (uint32, *api.Bytes, func(zapcore.ObjectEncoder) error) {
	wssValues := lfcMetrics.ApproximateworkingSetSizeBuckets
	// At this point, we can assume that the values are equally spaced at 1 minute apart,
	// starting at 1 minute.
	offsetIndex := *cfg.LFCMinWaitBeforeDownscaleMinutes - 1 // -1 because values start at 1m
	windowSize := *cfg.LFCWindowSizeMinutes
	// Handle invalid metrics:
	if len(wssValues) < offsetIndex+windowSize {
		warn("not enough working set size values to make scaling determination")
		return 0, nil, nil
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
		// into GiB...
		estimateWssMem := predictedHighestNextMinute * 8192
		// ... and then invert the discount form only some of the memory going towards LFC...
		requiredMem := estimateWssMem / *cfg.LFCToMemoryRatio
		// ... and then convert that into the actual CU required to fit the working set:
		requiredCU := requiredMem / computeUnit.Mem.AsFloat64()
		lfcGoalCU := uint32(math.Ceil(requiredCU))

		lfcLogFields := func(obj zapcore.ObjectEncoder) error {
			obj.AddFloat64("estimateWssPages", estimateWss)
			obj.AddFloat64("predictedNextWssPages", predictedHighestNextMinute)
			obj.AddFloat64("requiredCU", requiredCU)
			return nil
		}

		return lfcGoalCU, lo.ToPtr(api.Bytes(estimateWssMem)), lfcLogFields
	}
}
