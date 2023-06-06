package watch

// Metrics for Watch()

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"k8s.io/apimachinery/pkg/watch"
)

// Metrics holds some common prometheus collectors that are used by Watch
//
// The metrics used are:
//
// - client_calls_total (number of calls to k8s client.{Watch,List}, labeled by method)
// - relist_requests_total (number of "relist" requests from the Store)
// - events_total (number of K8s watch.Events that have occurred, including errors)
// - errors_total (number of errors, either error events or re-List errors, labeled by source: ["List", "Watch", "Watch.Event"])
// - alive_current (1 iff the watcher is currently running or failing, else 0)
// - failing_current (1 iff the watcher's last request failed *and* it's waiting to retry, else 0)
//
// Prefixes are typically of the form "COMPONENT_watchers" (e.g. "autoscaling_agent_watchers").
// Separate reporting per call to Watch is automatically done with the "watcher_instance" label
// attached to the metrics, using MetricsConfig.
//
// A brief note about "alive" and "failing": Reading from a pair of collectors is fundamentally
// racy. It may be possible to temporarily view "failing" but not "alive".
type Metrics struct {
	clientCallsTotal    *prometheus.CounterVec
	relistRequestsTotal *prometheus.CounterVec
	eventsTotal         *prometheus.CounterVec
	errorsTotal         *prometheus.CounterVec
	aliveCurrent        *prometheus.GaugeVec
	failingCurrent      *prometheus.GaugeVec

	// note: all usage of Metrics is by value, so this field gets copied in on each Watch call.
	// It gives us a bit of state to use for the failing and unfailing functions.
	isFailing bool
}

type MetricsConfig struct {
	Metrics
	// Instance provides the value of the "watcher_instance" label that will be applied to all
	// metrics collected for the Watch call
	Instance string
}

const metricInstanceLabel = "watcher_instance"

// NewMetrics creates a new set of metrics for one or many Watch calls
//
// All metrics' names will be prefixed with the provided string.
func NewMetrics(prefix string) Metrics {
	return Metrics{
		isFailing: false,

		clientCallsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: fmt.Sprint(prefix, "_client_calls_total"),
				Help: "Number of calls to k8s client.{Watch,List}, labeled by method",
			},
			[]string{metricInstanceLabel, "method"},
		),
		relistRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: fmt.Sprint(prefix, "_relist_requests_total"),
				Help: "Number of internal manual relisting requests",
			},
			[]string{metricInstanceLabel},
		),
		eventsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: fmt.Sprint(prefix, "_events_total"),
				Help: "Number of k8s watch.Events that have occurred, including errors, labeled by type",
			},
			[]string{metricInstanceLabel, "type"},
		),
		errorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: fmt.Sprint(prefix, "_errors_total"),
				Help: "Number of errors, either error events or re-list errors, labeled by source",
			},
			[]string{metricInstanceLabel, "source"},
		),
		aliveCurrent: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: fmt.Sprint(prefix, "_alive_current"),
				Help: "For each watcher, 1 iff the watcher is currently running or failing, else 0",
			},
			[]string{metricInstanceLabel},
		),
		failingCurrent: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: fmt.Sprint(prefix, "_failing_current"),
				Help: "For each watcher, 1 iff the watcher's last request failed *and* it's waiting to retry, else 0",
			},
			[]string{metricInstanceLabel},
		),
	}
}

// MustRegister registers all the collectors in the Metrics
func (m *Metrics) MustRegister(reg *prometheus.Registry) {
	reg.MustRegister(m.clientCallsTotal)
	reg.MustRegister(m.relistRequestsTotal)
	reg.MustRegister(m.eventsTotal)
	reg.MustRegister(m.errorsTotal)
	reg.MustRegister(m.aliveCurrent)
	reg.MustRegister(m.failingCurrent)
}

///////////////////////////////////////////////
// Internal helper methods for MetricsConfig //
///////////////////////////////////////////////

func (m *MetricsConfig) alive() {
	m.aliveCurrent.WithLabelValues(m.Instance).Inc()
	// Explicitly set the 'failing' count so that it's present (and set to zero)
	m.failingCurrent.WithLabelValues(m.Instance).Add(0.0)
}

func (m *MetricsConfig) unalive() {
	m.aliveCurrent.WithLabelValues(m.Instance).Dec()
}

func (m *MetricsConfig) failing() {
	if !m.isFailing {
		m.failingCurrent.WithLabelValues(m.Instance).Inc()
	}
	m.isFailing = true
}

func (m *MetricsConfig) unfailing() {
	if m.isFailing {
		m.failingCurrent.WithLabelValues(m.Instance).Dec()
	}
	m.isFailing = false
}

func (m *MetricsConfig) startList() {
	m.clientCallsTotal.WithLabelValues(m.Instance, "List").Inc()
}

func (m *MetricsConfig) startWatch() {
	m.clientCallsTotal.WithLabelValues(m.Instance, "Watch").Inc()
}

func (m *MetricsConfig) relistRequested() {
	m.relistRequestsTotal.WithLabelValues(m.Instance).Inc()
}

func (m *MetricsConfig) doneList(err error) {
	if err != nil {
		m.errorsTotal.WithLabelValues(m.Instance, "List").Inc()
	}
}

func (m *MetricsConfig) doneWatch(err error) {
	if err != nil {
		m.errorsTotal.WithLabelValues(m.Instance, "Watch").Inc()
	}
}

func (m *MetricsConfig) recordEvent(ty watch.EventType) {
	m.eventsTotal.WithLabelValues(m.Instance, string(ty)).Inc()

	if ty == watch.Error {
		m.errorsTotal.WithLabelValues(m.Instance, "Watch.Event").Inc()
	}
}
