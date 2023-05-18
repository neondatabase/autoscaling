package util

import "github.com/prometheus/client_golang/prometheus"

func RegisterMetric[P prometheus.Collector](reg *prometheus.Registry, collector P) P {
	reg.MustRegister(collector)
	return collector
}
