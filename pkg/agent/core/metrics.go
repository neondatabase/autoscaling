package core

// Abstraction over the []metrics -> CU "algorithm"
//
// In general, there's only *really* one ScalingAlgorithm implementation, but we keep it generic so
// that tests can dependency-inject an alternate implementation that's more

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type Metrics struct {
	LoadAverage1Min  float32
	LoadAverage5Min  float32 // unused. To be removed when api.Metrics.LoadAverage5Min is removed.
	MemoryUsageBytes float32
}

// ToAPI converts the Metrics to the subset used for communications with the scheduler plugin and
// exposed in pkg/api
//
// ToAPI implements ToAPIMetrics.
func (m Metrics) ToAPI() api.Metrics {
	return api.Metrics{
		LoadAverage1Min:  m.LoadAverage1Min,
		LoadAverage5Min:  m.LoadAverage5Min,
		MemoryUsageBytes: m.MemoryUsageBytes,
	}
}

// ScalingAlgorithm is the abstract interface provided by the default scaling algorithm, so that a
// simpler version can be dependency-injected for easier testing.
//
// And also, separation of concerns is nice. Where state.go (and State within it) are more concerned
// with the overall process of scaling approval and notification by communicating with other
// components, the implementation(s) of ScalingAlgorithm can be more focused, making the calculation
// of desired state abundantly clear.
type ScalingAlgorithm[M ToAPIMetrics] interface {
	// Update adds recent metrics
	Update(metrics M)

	// GoalCU returns the desired number of compute units that the VM should be using, given data
	// from prior calls to Update and the configured resources equal to one compute unit.
	GoalCU(scalingConfig api.ScalingConfig, computeUnit api.Resources) (_ uint32, ok bool)

	// DeepCopy produces a copy of the value, intended for use when serializing State
	DeepCopy() ScalingAlgorithm[M]
}

// ToApiMetrics is the interface that ScalingAlgorithm's metrics must satisfy - namely, that they
// can be converted to an api.Metrics
type ToAPIMetrics interface {
	ToAPI() api.Metrics
}

// SimpleFactorScaling is a ScalingAlgorithm implementation that calculates the desired resource
// usage by a simple multiple of the most recently collected metrics
type SimpleFactorScaling struct {
	// nb: fields only public to allow JSON serialization
	Metrics *Metrics
}

func NewSimpleFactorScaling() *SimpleFactorScaling {
	return &SimpleFactorScaling{
		Metrics: nil,
	}
}

// DeepCopy implements ScalingAlgorithm[Metrics]
func (s *SimpleFactorScaling) DeepCopy() ScalingAlgorithm[Metrics] {
	return &SimpleFactorScaling{
		Metrics: shallowCopy[Metrics](s.Metrics),
	}
}

// Update implements ScalingAlgorithm[Metrics]
func (s *SimpleFactorScaling) Update(metrics Metrics) {
	s.Metrics = &metrics
}

// GoalCU implements ScalingAlgorithm[Metrics], refer there for more.
func (s *SimpleFactorScaling) GoalCU(scalingConfig api.ScalingConfig, computeUnit api.Resources) (_ uint32, ok bool) {
	if s.Metrics == nil {
		return 0, false
	}
	// Broadly, the implementation works like this:
	// For CPU:
	// Based on load average, calculate the "goal" number of CPUs (and therefore compute units)
	//
	// For Memory:
	// Based on memory usage, calculate the VM's desired memory allocation and extrapolate a
	// goal number of CUs from that.
	//
	// 1. Take the maximum of these two goal CUs to create a unified goal CU
	// 2. Cap the goal CU by min/max, etc
	// 3. that's it!

	// For CPU:
	// Goal compute unit is at the point where (CPUs) × (LoadAverageFractionTarget) == (load
	// average),
	// which we can get by dividing LA by LAFT, and then dividing by the number of CPUs per CU
	goalCPUs := float64(s.Metrics.LoadAverage1Min) / scalingConfig.LoadAverageFractionTarget
	cpuGoalCU := uint32(math.Round(goalCPUs / computeUnit.VCPU.AsFloat64()))

	// For Mem:
	// Goal compute unit is at the point where (Mem) * (MemoryUsageFractionTarget) == (Mem Usage)
	// We can get the desired memory allocation in bytes by dividing MU by MUFT, and then convert
	// that to CUs
	//
	// NOTE: use uint64 for calculations on bytes as uint32 can overflow
	memGoalBytes := uint64(math.Round(float64(s.Metrics.MemoryUsageBytes) / scalingConfig.MemoryUsageFractionTarget))
	memGoalCU := uint32(memGoalBytes / uint64(computeUnit.Mem))

	goalCU := util.Max(cpuGoalCU, memGoalCU)
	return goalCU, true
}

// ReadMetrics generates Metrics from node_exporter output, or returns error on failure
//
// This function could be more efficient, but realistically it doesn't matter. The size of the
// output from node_exporter/vector is so small anyways.
func ReadMetrics(nodeExporterOutput []byte, loadPrefix string) (m Metrics, err error) {
	lines := strings.Split(string(nodeExporterOutput), "\n")

	getField := func(linePrefix, dontMatch string) (float32, error) {
		var line string
		for _, l := range lines {
			if strings.HasPrefix(l, linePrefix) && (len(dontMatch) == 0 || !strings.HasPrefix(l, dontMatch)) {
				line = l
				break
			}
		}
		if line == "" {
			return 0, fmt.Errorf("No line in metrics output starting with %q", linePrefix)
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf(
				"Expected >= 2 fields in metrics output for %q. Got %v",
				linePrefix, len(fields),
			)
		}

		v, err := strconv.ParseFloat(fields[1], 32)
		if err != nil {
			return 0, fmt.Errorf(
				"Error parsing %q as float for line starting with %q: %w",
				fields[1], linePrefix, err,
			)
		}
		return float32(v), nil
	}

	m.LoadAverage1Min, err = getField(loadPrefix+"load1", loadPrefix+"load15")
	if err != nil {
		return
	}
	m.LoadAverage5Min, err = getField(loadPrefix+"load5", "")
	if err != nil {
		return
	}

	availableMem, err := getField(loadPrefix+"memory_available_bytes", "")
	if err != nil {
		return
	}
	totalMem, err := getField(loadPrefix+"memory_total_bytes", "")
	if err != nil {
		return
	}

	// Add an extra 100 MiB to account for kernel memory usage
	m.MemoryUsageBytes = totalMem - availableMem + 100*(1<<20)

	return
}
