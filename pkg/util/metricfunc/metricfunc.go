package metricfunc

import "github.com/prometheus/client_golang/prometheus"

type MetricValue struct {
	Labels prometheus.Labels
	Value  float64
}

type GaugeVecFunc struct {
	vec *prometheus.GaugeVec
	cbs []func() []MetricValue
}

func (g *GaugeVecFunc) Collect(ch chan<- prometheus.Metric) {
	for _, cb := range g.cbs {
		for _, metric := range cb() {
			g.vec.With(metric.Labels).Set(metric.Value)
		}
	}
	g.vec.Collect(ch)
}

func (g *GaugeVecFunc) Describe(ch chan<- *prometheus.Desc) {
	g.vec.Describe(ch)
}

func (g *GaugeVecFunc) Add(cb func() []MetricValue) {
	g.cbs = append(g.cbs, cb)
}

func NewGaugeVecFunc(opts prometheus.GaugeOpts, labels []string) *GaugeVecFunc {
	return &GaugeVecFunc{
		vec: prometheus.NewGaugeVec(opts, labels),
		cbs: nil,
	}
}
