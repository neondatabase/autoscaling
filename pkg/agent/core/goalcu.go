package core

// extracted components of how "goal CU" is determined

import (
	"math"

	"github.com/samber/lo"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/neondatabase/autoscaling/pkg/api"
)

// AlgorithmState abstracts over providers of "goal CU" calculation.
//
// This interface exists so that our unit tests for State.NextActions() don't need to be rewritten
// when the algorithm changes.
//
// StdAlgorithm is expected to be used for anything outside unit tests.
type AlgorithmState interface {
	// CalculateGoalCU returns the desired compute units to scale to, plus any log fields with
	// additional useful information.
	CalculateGoalCU(
		warn func(string),
		cfg api.ScalingConfig,
		computeUnit api.Resources,
	) (ScalingGoal, []zap.Field)

	// LatestAPIMetrics returns the api.Metrics that should be sent to the scheduler for
	// prioritization of live migration.
	LatestAPIMetrics() *api.Metrics

	// ScalingConfigUpdated gives the algorithm a chance to update its internal state to reflect
	// that the scaling config has been updated.
	//
	// In practice, this is only used by StdAlgorithm to reset its LFC metrics when they become
	// disabled, so that we don't accidentally use stale metrics if/when it's re-enabled.
	ScalingConfigUpdated(api.ScalingConfig)
}

type ScalingGoal struct {
	HasAllMetrics bool
	GoalCU        uint32
}

// StdAlgorithm is the standard implementation of AlgorithmState for usage in production.
type StdAlgorithm struct {
	System *SystemMetrics
	LFC    *LFCMetrics
}

func DefaultAlgorithm() *StdAlgorithm {
	return &StdAlgorithm{
		System: nil,
		LFC:    nil,
	}
}

// LatestAPIMetrics implements AlgorithmState
func (m *StdAlgorithm) LatestAPIMetrics() *api.Metrics {
	if m.System != nil {
		return lo.ToPtr(m.System.ToAPI())
	} else {
		return nil
	}
}

func (m *StdAlgorithm) ScalingConfigUpdated(conf api.ScalingConfig) {
	// Make sure that if LFC metrics are disabled & later enabled, we don't make decisions based on
	// stale data.
	if !*conf.EnableLFCMetrics {
		m.LFC = nil
	}
}

// CalculateGoalCU implements AlgorithmState
func (m *StdAlgorithm) CalculateGoalCU(
	warn func(string),
	cfg api.ScalingConfig,
	computeUnit api.Resources,
) (ScalingGoal, []zap.Field) {
	hasAllMetrics := m.System != nil && (!*cfg.EnableLFCMetrics || m.LFC != nil)

	var lfcGoalCU, cpuGoalCU, memGoalCU, memTotalGoalCU uint32
	var logFields []zap.Field

	var wss *api.Bytes // estimated working set size

	if m.LFC != nil {
		var lfcLogFunc func(zapcore.ObjectEncoder) error
		lfcGoalCU, wss, lfcLogFunc = calculateLFCGoalCU(warn, cfg, computeUnit, *m.LFC)
		if lfcLogFunc != nil {
			logFields = append(logFields, zap.Object("lfc", zapcore.ObjectMarshalerFunc(lfcLogFunc)))
		}
	}

	if m.System != nil {
		cpuGoalCU = calculateCPUGoalCU(cfg, computeUnit, *m.System)

		memGoalCU = calculateMemGoalCU(cfg, computeUnit, *m.System)
	}

	if m.System != nil && wss != nil {
		memTotalGoalCU = calculateMemTotalGoalCU(cfg, computeUnit, *m.System, *wss)
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
