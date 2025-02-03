package core

// extracted components of how "goal CU" is determined

import (
	"math"

	"github.com/samber/lo"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/constraints"

	"github.com/neondatabase/autoscaling/pkg/api"
)

type ScalingGoal struct {
	HasAllMetrics bool
	Parts         ScalingGoalParts
}

type ScalingGoalParts struct {
	CPU *float64
	Mem *float64
	LFC *float64
}

func (g *ScalingGoal) GoalCU() uint32 {
	return uint32(math.Ceil(max(
		math.Round(lo.FromPtr(g.Parts.CPU)), // for historical compatibility, use round() instead of ceil()
		lo.FromPtr(g.Parts.Mem),
		lo.FromPtr(g.Parts.LFC),
	)))
}

func calculateGoalCU(
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

	var logFields []zap.Field
	var parts ScalingGoalParts

	var wss *api.Bytes // estimated working set size

	if lfcMetrics != nil {
		var lfcLogFunc func(zapcore.ObjectEncoder) error
		var lfcGoalCU float64
		lfcGoalCU, wss, lfcLogFunc = calculateLFCGoalCU(warn, cfg, computeUnit, *lfcMetrics)
		parts.LFC = lo.ToPtr(lfcGoalCU)
		if lfcLogFunc != nil {
			logFields = append(logFields, zap.Object("lfc", zapcore.ObjectMarshalerFunc(lfcLogFunc)))
		}
	}

	if systemMetrics != nil {
		cpuGoalCU := calculateCPUGoalCU(cfg, computeUnit, *systemMetrics)
		parts.CPU = lo.ToPtr(cpuGoalCU)

		memGoalCU := calculateMemGoalCU(cfg, computeUnit, *systemMetrics)
		parts.Mem = lo.ToPtr(memGoalCU)
	}

	if systemMetrics != nil && wss != nil {
		memTotalGoalCU := calculateMemTotalGoalCU(cfg, computeUnit, *systemMetrics, *wss)
		parts.Mem = lo.ToPtr(max(*parts.Mem, memTotalGoalCU))
	}

	return ScalingGoal{HasAllMetrics: hasAllMetrics, Parts: parts}, logFields
}

// For CPU:
// Goal compute unit is at the point where (CPUs) × (LoadAverageFractionTarget) == (load average),
// which we can get by dividing LA by LAFT, and then dividing by the number of CPUs per CU
func calculateCPUGoalCU(
	cfg api.ScalingConfig,
	computeUnit api.Resources,
	systemMetrics SystemMetrics,
) float64 {
	stableThreshold := *cfg.CPUStableZoneRatio * systemMetrics.LoadAverage5Min
	mixedThreshold := stableThreshold + *cfg.CPUMixedZoneRatio*systemMetrics.LoadAverage5Min

	diff := math.Abs(systemMetrics.LoadAverage1Min - systemMetrics.LoadAverage5Min)
	// load1Weight is 0 when diff < stableThreshold, and 1 when diff > mixedThreshold.
	// If diff is between the thresholds, it'll be between 0 and 1.
	load1Weight := blendingFactor(diff, stableThreshold, mixedThreshold)

	blendedLoadAverage := load1Weight*systemMetrics.LoadAverage1Min + (1-load1Weight)*systemMetrics.LoadAverage5Min

	goalCPUs := blendedLoadAverage / *cfg.LoadAverageFractionTarget
	cpuGoalCU := goalCPUs / computeUnit.VCPU.AsFloat64()
	return cpuGoalCU
}

func blendingFactor[T constraints.Float](value, t1, t2 T) T {
	if value <= t1 {
		return 0
	}
	if value >= t2 {
		return 1
	}
	// 1e-6 is just a precaution, if t1==t2, we'd return earlier.
	return (value - t1) / (t2 - t1 + 1e-6)
}

// For Mem:
// Goal compute unit is at the point where (Mem) * (MemoryUsageFractionTarget) == (Mem Usage)
// We can get the desired memory allocation in bytes by dividing MU by MUFT, and then convert
// that to CUs.
func calculateMemGoalCU(
	cfg api.ScalingConfig,
	computeUnit api.Resources,
	systemMetrics SystemMetrics,
) float64 {
	// goal memory size, just looking at allocated memory (not including page cache...)
	memGoalBytes := math.Round(systemMetrics.MemoryUsageBytes / *cfg.MemoryUsageFractionTarget)

	return memGoalBytes / float64(computeUnit.Mem)
}

// goal memory size, looking at allocated memory and min(page cache usage, LFC working set size)
func calculateMemTotalGoalCU(
	cfg api.ScalingConfig,
	computeUnit api.Resources,
	systemMetrics SystemMetrics,
	wss api.Bytes,
) float64 {
	lfcCached := min(float64(wss), systemMetrics.MemoryCachedBytes)
	totalGoalBytes := (lfcCached + systemMetrics.MemoryUsageBytes) / *cfg.MemoryTotalFractionTarget

	return totalGoalBytes / float64(computeUnit.Mem)
}

func calculateLFCGoalCU(
	warn func(string),
	cfg api.ScalingConfig,
	computeUnit api.Resources,
	lfcMetrics LFCMetrics,
) (float64, *api.Bytes, func(zapcore.ObjectEncoder) error) {
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
		var estimateWss float64
		if *cfg.LFCUseLargestWindow {
			estimateWss = wssValues[len(wssValues)-1]
		} else {
			estimateWss = EstimateTrueWorkingSetSize(wssValues, WssEstimatorConfig{
				MaxAllowedIncreaseFactor: 3.0, // hard-code this for now.
				InitialOffset:            offsetIndex,
				WindowSize:               windowSize,
			})
		}
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

		lfcLogFields := func(obj zapcore.ObjectEncoder) error {
			obj.AddFloat64("estimateWssPages", estimateWss)
			obj.AddFloat64("predictedNextWssPages", predictedHighestNextMinute)
			obj.AddFloat64("requiredCU", requiredCU)
			return nil
		}

		return requiredCU, lo.ToPtr(api.Bytes(estimateWssMem)), lfcLogFields
	}
}
