package plugin

// defines prometheus metrics and provides the server, via (*AutoscaleEnforcer).startPrometheusServer()

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"k8s.io/klog/v2"
)

type PromMetrics struct {
	pluginCalls           *prometheus.CounterVec
	resourceRequests      *prometheus.CounterVec
	validResourceRequests *prometheus.CounterVec
}

func (p *AutoscaleEnforcer) startPrometheusServer(ctx context.Context) error {
	// Separate binding from serving, so that we can catch any error in this thread, rather than the
	// server's.
	addr := net.TCPAddr{IP: net.IPv4zero, Port: 9100}
	listener, err := net.ListenTCP("tcp", &addr)
	if err != nil {
		return fmt.Errorf("Error listening on TCP: %w", err)
	}

	p.metrics = PromMetrics{
		pluginCalls: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "plugin_extension_calls_total",
				Help: "Number of calls to plugin extension points",
			},
			[]string{"method"},
		),
		resourceRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "plugin_resource_requests_total",
				Help: "Number of resource requests",
			},
			[]string{"client_addr", "code"},
		),
		validResourceRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "plugin_resource_requests_results_total",
				Help: "Number of resource requests with various results",
			},
			[]string{"node", "protocol_version", "has_metrics"},
		),
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		p.metrics.pluginCalls,
		p.metrics.resourceRequests,
		p.metrics.validResourceRequests,
	)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	srv := &http.Server{Handler: mux}

	go func() {
		if err := srv.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			klog.Errorf("Prometheus server exited with unexpected error: %s", err)
		}
	}()

	return nil
}
