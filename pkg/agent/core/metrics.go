package core

// Definition of the Metrics type, plus reading it from vector.dev's prometheus format host metrics

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	promtypes "github.com/prometheus/client_model/go"
	promfmt "github.com/prometheus/common/expfmt"
	"github.com/tychoish/fun/erc"

	"github.com/neondatabase/autoscaling/pkg/api"
)

type SystemMetrics struct {
	LoadAverage1Min   float64
	LoadAverage5Min   float64
	MemoryUsageBytes  float64
	MemoryCachedBytes float64
}

func (m SystemMetrics) ToAPI() api.Metrics {
	return api.Metrics{
		LoadAverage1Min:  float32(m.LoadAverage1Min),
		LoadAverage5Min:  nil,
		MemoryUsageBytes: nil,
	}
}

type LFCMetrics struct {
	CacheHitsTotal   float64
	CacheMissesTotal float64
	CacheWritesTotal float64

	// lfc_approximate_working_set_size_windows, currently requires that values are exactly every
	// minute
	ApproximateworkingSetSizeBuckets []float64
}

// FromPrometheus represents metric types that can be parsed from prometheus output.
type FromPrometheus interface {
	fromPrometheus(map[string]*promtypes.MetricFamily) error
}

// ParseMetrics reads the prometheus text-format content, parses it, and uses M's implementation of
// FromPrometheus to populate it before returning.
func ParseMetrics(content io.Reader, metrics FromPrometheus) error {
	var parser promfmt.TextParser
	mfs, err := parser.TextToMetricFamilies(content)
	if err != nil {
		return fmt.Errorf("failed to parse content as prometheus text format: %w", err)
	}

	if err := metrics.fromPrometheus(mfs); err != nil {
		return fmt.Errorf("failed to extract metrics: %w", err)
	}

	return nil
}

func extractFloatGauge(mf *promtypes.MetricFamily) (float64, error) {
	if mf.GetType() != promtypes.MetricType_GAUGE {
		return 0, fmt.Errorf("wrong metric type: expected %s but got %s", promtypes.MetricType_GAUGE, mf.GetType())
	} else if len(mf.Metric) != 1 {
		return 0, fmt.Errorf("expected 1 metric, found %d", len(mf.Metric))
	}

	return mf.Metric[0].GetGauge().GetValue(), nil
}

// Helper function to return an error for a missing metric
func missingMetric(name string) error {
	return fmt.Errorf("missing expected metric %s", name)
}

// fromPrometheus implements FromPrometheus, so SystemMetrics can be used with ParseMetrics.
func (m *SystemMetrics) fromPrometheus(mfs map[string]*promtypes.MetricFamily) error {
	ec := &erc.Collector{}

	getFloat := func(metricName string) float64 {
		if mf := mfs[metricName]; mf != nil {
			f, err := extractFloatGauge(mf)
			ec.Add(err) // does nothing if err == nil
			return f
		} else {
			ec.Add(missingMetric(metricName))
			return 0
		}
	}

	load1 := getFloat("host_load1")
	load5 := getFloat("host_load5")
	memTotal := getFloat("host_memory_total_bytes")
	memAvailable := getFloat("host_memory_available_bytes")
	memCached := getFloat("host_memory_cached_bytes")

	tmp := SystemMetrics{
		LoadAverage1Min: load1,
		LoadAverage5Min: load5,
		// Add an extra 100 MiB to account for kernel memory usage
		MemoryUsageBytes:  memTotal - memAvailable + 100*(1<<20),
		MemoryCachedBytes: memCached,
	}

	if err := ec.Resolve(); err != nil {
		return err
	}

	*m = tmp
	return nil
}

// fromPrometheus implements FromPrometheus, so LFCMetrics can be used with ParseMetrics.
func (m *LFCMetrics) fromPrometheus(mfs map[string]*promtypes.MetricFamily) error {
	ec := &erc.Collector{}

	getFloat := func(metricName string) float64 {
		if mf := mfs[metricName]; mf != nil {
			f, err := extractFloatGauge(mf)
			ec.Add(err) // does nothing if err == nil
			return f
		} else {
			ec.Add(missingMetric(metricName))
			return 0
		}
	}

	wssBuckets, err := extractWorkingSetSizeWindows(mfs)
	ec.Add(err)

	tmp := LFCMetrics{
		CacheHitsTotal:   getFloat("lfc_hits"),
		CacheMissesTotal: getFloat("lfc_misses"),
		CacheWritesTotal: getFloat("lfc_writes"),

		ApproximateworkingSetSizeBuckets: wssBuckets,
	}

	if err := ec.Resolve(); err != nil {
		return err
	}

	*m = tmp
	return nil
}

func extractWorkingSetSizeWindows(mfs map[string]*promtypes.MetricFamily) ([]float64, error) {
	metricName := "lfc_approximate_working_set_size_windows"
	mf := mfs[metricName]
	if mf == nil {
		return nil, missingMetric(metricName)
	}

	if mf.GetType() != promtypes.MetricType_GAUGE {
		return nil, fmt.Errorf("wrong metric type: expected %s, but got %s", promtypes.MetricType_GAUGE, mf.GetType())
	} else if len(mf.Metric) < 1 {
		return nil, fmt.Errorf("expected >= metric, found %d", len(mf.Metric))
	}

	type pair struct {
		duration time.Duration
		value    float64
	}

	var pairs []pair
	for _, m := range mf.Metric {
		// Find the duration label
		durationLabel := "duration_seconds"
		durationIndex := slices.IndexFunc(m.Label, func(l *promtypes.LabelPair) bool {
			return l.GetName() == durationLabel
		})
		if durationIndex == -1 {
			return nil, fmt.Errorf("metric missing label %q", durationLabel)
		}

		durationSeconds, err := strconv.Atoi(m.Label[durationIndex].GetValue())
		if err != nil {
			return nil, fmt.Errorf("couldn't parse metric's %q label as int: %w", durationLabel, err)
		}

		pairs = append(pairs, pair{
			duration: time.Second * time.Duration(durationSeconds),
			value:    m.GetGauge().GetValue(),
		})
	}

	slices.SortFunc(pairs, func(x, y pair) int {
		return cmp.Compare(x.duration, y.duration)
	})

	// Check that the values make are as expected: they should all be 1 minute apart, starting
	// at 1 minute.
	// NOTE: this assumption is relied on elsewhere for scaling on ApproximateworkingSetSizeBuckets.
	// Please search for usages before changing this behavior.
	if pairs[0].duration != time.Minute {
		return nil, fmt.Errorf("expected smallest duration to be %v, got %v", time.Minute, pairs[0].duration)
	}
	for i := range pairs {
		expected := time.Minute * time.Duration(i+1)
		if pairs[i].duration != expected {
			return nil, fmt.Errorf(
				"expected duration values to be exactly 1m apart, got unexpected value %v instead of %v",
				pairs[i].duration,
				expected,
			)
		}
	}

	var values []float64
	for _, p := range pairs {
		values = append(values, p.value)
	}
	return values, nil
}
