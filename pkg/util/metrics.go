package util

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	klog "k8s.io/klog/v2"
)

func RegisterMetric[P prometheus.Collector](reg *prometheus.Registry, collector P) P {
	reg.MustRegister(collector)
	return collector
}

// Prometheus metrics server common to >1 component

// Starts the prometheus server in a background thread. Returns error if binding on the port fails.
func StartPrometheusMetricsServer(ctx context.Context, port uint16, reg *prometheus.Registry) error {
	// Separate binding from serving, so that we can catch any error in this thread, rather than the
	// server's.
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4zero, Port: int(port)})
	if err != nil {
		return fmt.Errorf("Error listening on TCP port %d: %w", port, err)
	}

	shutdownCtx, shutdown := context.WithCancel(ctx)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))

	baseContext := context.Background()
	srv := &http.Server{Handler: mux, BaseContext: func(net.Listener) context.Context { return baseContext }}

	go func() {
		<-shutdownCtx.Done()
		if err := srv.Shutdown(context.Background()); err != nil {
			klog.Errorf("Error shutting down prometheus server: %s", err)
		}
	}()

	go func() {
		// shutdown the shutdown watcher if we exit before it
		defer shutdown()
		if err := srv.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			klog.Errorf("Prometheus server exited with unexpected error: %s", err)
		}
	}()

	return nil
}
