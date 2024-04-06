package core

// Definition of the Metrics type, plus reading it from vector.dev's prometheus format host metrics

import (
	"fmt"
	"io"

	promtypes "github.com/prometheus/client_model/go"
	promfmt "github.com/prometheus/common/expfmt"
	"github.com/tychoish/fun/erc"

	"github.com/neondatabase/autoscaling/pkg/api"
)

type Metrics struct {
	LoadAverage1Min  float64
	MemoryUsageBytes float64
}

func (m Metrics) ToAPI() api.Metrics {
	return api.Metrics{
		LoadAverage1Min:  float32(m.LoadAverage1Min),
		LoadAverage5Min:  nil,
		MemoryUsageBytes: nil,
	}
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

// fromPrometheus implements FromPrometheus, so Metrics can be used with ParseMetrics.
func (m *Metrics) fromPrometheus(mfs map[string]*promtypes.MetricFamily) error {
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
	memTotal := getFloat("host_memory_total_bytes")
	memAvailable := getFloat("host_memory_available_bytes")

	tmp := Metrics{
		LoadAverage1Min: load1,
		// Add an extra 100 MiB to account for kernel memory usage
		MemoryUsageBytes: memTotal - memAvailable + 100*(1<<20),
	}

	if err := ec.Resolve(); err != nil {
		return err
	}

	*m = tmp
	return nil
}
