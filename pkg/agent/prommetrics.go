package agent

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
	schedulerRequests         *prometheus.CounterVec
	informantRequestsOutbound *prometheus.CounterVec
	informantRequestsInbound  *prometheus.CounterVec
}

func startPrometheusServer(ctx context.Context, globalstate *agentState) (PromMetrics, error) {
	// Separate binding from serving, so that we can catch any error in this thread, rather than the
	// server's.
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4zero, Port: 9100})
	if err != nil {
		return PromMetrics{}, fmt.Errorf("Error listening on TCP: %w", err)
	}

	schedulerRequests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "scheduler_plugin_requests_total",
			Help: "Number of attempted HTTP requests to the scheduler plugin",
		},
		[]string{"code"},
	)
	informantRequestsOutbound := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "informant_outbound_requests_total",
			Help: "Number of attempted HTTP requests to vm-informants",
		},
		[]string{"code"},
	)
	informantRequestsInbound := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "informant_inbound_requests_total",
			Help: "Number of HTTP requests from vm-informants",
		},
		[]string{"endpoint", "code"},
	)
	totalVMs := prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "tracked_vms_current",
			Help: "Number of VMs on the autoscaler-agent's node that it's tracking",
		},
		func() float64 {
			globalstate.lock.Lock()
			defer globalstate.lock.Unlock()

			return float64(len(globalstate.pods))
		},
	)

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		schedulerRequests,
		informantRequestsOutbound,
		informantRequestsInbound,
		totalVMs,
	)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	srv := &http.Server{Handler: mux}

	shutdownCtx, shutdown := context.WithCancel(ctx)

	// Shutdown the server when the context expires, but also exit the shutdown watcher if the
	// server shuts down for some other reason.
	go func() {
		<-shutdownCtx.Done()
		if err := srv.Shutdown(context.Background()); err != nil {
			klog.Errorf("Error shutting down prometheus server: %w", err)
		}
	}()

	go func() {
		// shutdown the shutdown watcher if we exit before it
		defer shutdown()
		if err := srv.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			klog.Errorf("Prometheus server exited with unexpected error: %s", err)
		}
	}()

	return PromMetrics{
		schedulerRequests:         schedulerRequests,
		informantRequestsOutbound: informantRequestsOutbound,
		informantRequestsInbound:  informantRequestsInbound,
	}, nil
}
