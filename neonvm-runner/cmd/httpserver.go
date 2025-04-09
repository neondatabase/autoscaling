package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/samber/lo"
	"go.uber.org/zap"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
)

type cpuServerCallbacks struct {
	get   func(*zap.Logger) (*vmv1.MilliCPU, *int, error)
	set   func(*zap.Logger, vmv1.MilliCPU) (*int, error)
	ready func(*zap.Logger) bool
}

func listenForHTTPRequests(
	ctx context.Context,
	logger *zap.Logger,
	port int32,
	callbacks cpuServerCallbacks,
	wg *sync.WaitGroup,
	networkMonitoring bool,
) {
	defer wg.Done()
	mux := http.NewServeMux()
	loggerHandlers := logger.Named("http-handlers")
	cpuChangeLogger := loggerHandlers.Named("cpu_change")
	mux.HandleFunc("/cpu_change", func(w http.ResponseWriter, r *http.Request) {
		handleCPUChange(cpuChangeLogger, w, r, callbacks.set)
	})
	cpuCurrentLogger := loggerHandlers.Named("cpu_current")
	mux.HandleFunc("/cpu_current", func(w http.ResponseWriter, r *http.Request) {
		handleCPUCurrent(cpuCurrentLogger, w, r, callbacks.get)
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if callbacks.ready(logger) {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(500)
		}
	})
	if networkMonitoring {
		reg := prometheus.NewRegistry()
		metrics := NewMonitoringMetrics(reg)
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			metrics.update(logger)
			h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
			h.ServeHTTP(w, r)
		})
	}
	server := http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", port),
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}
	errChan := make(chan error)
	go func() {
		errChan <- server.ListenAndServe()
	}()
	select {
	case err := <-errChan:
		if errors.Is(err, http.ErrServerClosed) {
			logger.Info("http server closed")
		} else if err != nil {
			logger.Fatal("http server exited with error", zap.Error(err))
		}
	case <-ctx.Done():
		err := server.Shutdown(context.Background())
		logger.Info("shut down http server", zap.Error(err))
	}
}

func handleCPUChange(
	logger *zap.Logger,
	w http.ResponseWriter,
	r *http.Request,
	set func(*zap.Logger, vmv1.MilliCPU) (*int, error),
) {
	if r.Method != "POST" {
		logger.Error("unexpected method", zap.String("method", r.Method))
		w.WriteHeader(400)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Error("could not read body", zap.Error(err))
		w.WriteHeader(400)
		return
	}

	var parsed api.VCPUChange
	if err = json.Unmarshal(body, &parsed); err != nil {
		logger.Error("could not parse body", zap.Error(err))
		w.WriteHeader(400)
		return
	}

	// update cgroup
	logger.Info("got CPU update", zap.Float64("CPU", parsed.VCPUs.AsFloat64()))
	status, err := set(logger, parsed.VCPUs)
	if err != nil {
		logger.Error("could not set cgroup limit", zap.Error(err))
		w.WriteHeader(lo.FromPtrOr(status, 500))
		return
	}

	w.WriteHeader(200)
}

func handleCPUCurrent(
	logger *zap.Logger,
	w http.ResponseWriter,
	r *http.Request,
	get func(*zap.Logger) (*vmv1.MilliCPU, *int, error),
) {
	if r.Method != "GET" {
		logger.Error("unexpected method", zap.String("method", r.Method))
		w.WriteHeader(400)
		return
	}

	cpus, status, err := get(logger)
	if err != nil {
		logger.Error("could not get cgroup quota", zap.Error(err))
		w.WriteHeader(lo.FromPtrOr(status, 500))
		return
	}
	resp := api.VCPUCgroup{VCPUs: *cpus}
	body, err := json.Marshal(resp)
	if err != nil {
		logger.Error("could not marshal body", zap.Error(err))
		w.WriteHeader(500)
		return
	}

	w.Header().Add("Content-Type", "application/json")
	w.Write(body) //nolint:errcheck // Not much to do with the error here. TODO: log it?
}
