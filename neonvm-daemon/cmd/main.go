package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/neonvm/cpuscaling"
	"github.com/neondatabase/autoscaling/pkg/util"
)

func main() {
	cpuAddr := flag.String("cpu-server-addr", "", `address to bind for the CPU HTTP server`)
	metricsAddr := flag.String("metrics-addr", "", `address to bind for the metrics server`)
	flag.Parse()

	if *cpuAddr == "" {
		fmt.Println("neonvm-daemon missing -cpu-server-addr flag")
		os.Exit(1)
	}
	if *metricsAddr == "" {
		fmt.Println("neonvm-daemon missing -metrics-addr flag")
		os.Exit(1)
	}

	logConfig := zap.NewProductionConfig()
	logConfig.Sampling = nil                // Disable sampling, which the production config enables by default.
	logConfig.Level.SetLevel(zap.InfoLevel) // Only "info" level and above (i.e. not debug logs)
	logger := zap.Must(logConfig.Build()).Named("neonvm-daemon")
	defer logger.Sync() //nolint:errcheck // what are we gonna do, log something about it?

	logger.Info("Starting neonvm-daemon", zap.String("cpuAddr", *cpuAddr), zap.String("metricsAddr", *metricsAddr))

	cpuScaler := &cpuscaling.CPUSysFsStateScaler{}

	startMetrics(logger, *metricsAddr, cpuScaler)

	srv := cpuServer{
		cpuOperationsMutex: &sync.Mutex{},
		cpuScaler:          cpuScaler,
		logger:             logger.Named("cpu-srv"),
	}
	srv.run(*cpuAddr)
}

func startMetrics(logger *zap.Logger, addr string, cpuScaler *cpuscaling.CPUSysFsStateScaler) {
	reg := prometheus.NewRegistry()

	load1Gauge := util.RegisterMetric(reg, prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "neonvmd_load1",
			Help: "Measure of load average, similar to Linux's 1-minute load average",
		},
	))

	// Repeatedly (re-)calculate load average
	go func() {
		var load1 float64

		// e^(-1/60), cached.
		expFactor := math.Exp(float64(-1.0 / 60.0))

		// update the load average every second
		ticker := time.NewTicker(time.Second)
		for _ = range ticker.C {
			var err error

			load1, err = updateLoadAverage(cpuScaler, expFactor, load1)
			if err != nil {
				// panic, so that the entire process exits, rather than just this one goroutine.
				logger.Panic("Failed to update load average", zap.Error(err))
			}

			load1Gauge.Set(load1)
		}
	}()

	// Launch the metrics server
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
		s := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadTimeout:       time.Second,
			ReadHeaderTimeout: time.Second,
			WriteTimeout:      time.Second,
		}

		if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			// panic, so that the entire process exits, rather than just this one goroutine.
			logger.Panic("Metrics server exited with unexpected error", zap.Error(err))
		}
	}()
}

// updateLoadAverage updates the load average 'load1', returning the new value.
func updateLoadAverage(cpuScaler *cpuscaling.CPUSysFsStateScaler, expFactor, load1 float64) (float64, error) {
	// For more information on this function, see:
	// https://www.notion.so/neondatabase/131f189e004780b2915ef2fdb95bae6a

	cpus, err := cpuScaler.ActiveCPUsCount()
	if err != nil {
		return 0, fmt.Errorf("could not fetch number of CPUs: %w", err)
	}

	tasks, err := getActiveTasksCount()

	var instantaneousLoad float64
	if tasks <= cpus {
		instantaneousLoad = float64(tasks)
	} else {
		instantaneousLoad = float64(cpus) + 0.5*float64(tasks-cpus)
	}

	load1 = load1*expFactor + instantaneousLoad*(1-expFactor)
	return load1, nil
}

var badLoadavgFileContents = errors.New("invalid contents of /proc/loadavg")

// getActiveTasksCount reads '/proc/loadavg' to return the number of currently running tasks.
func getActiveTasksCount() (uint32, error) {
	// From 'man 5 proc':
	//
	//   /proc/loadavg
	//     The first three fields in this file are load average figures giving the number of jobs in
	//     the run queue (state R) or waiting for disk I/O (state D) averaged over 1, 5, and 15
	//     minutes. They are the same as the load average numbers given by uptime(1) and other
	//     programs. The fourth field con‐sists of two numbers separated by a slash (/). The first
	//     of these is the number of currently runnable kernel scheduling entities (processes,
	//     threads). The value after the slash is the number of kernel scheduling entities that
	//     currently exist on the system. The fifth field is the PID of the process that was most
	//     recently created on the system.
	//
	// See also: <https://linux.die.net/man/5/proc>
	//
	// ---
	//
	// So basically, the contents of /proc/loadavg looks something like:
	//
	//   1.23 0.45 0.67 89/1011 121314
	//                  ^^
	//            value we want
	loadavgContents, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, fmt.Errorf("failed to read /proc/loadavg: %w", err)
	}

	fields := strings.SplitN(string(loadavgContents), " " /* single space */, 5)
	if len(fields) != 5 {
		return 0, badLoadavgFileContents
	}

	procsField := fields[3]
	procsParts := strings.SplitN(procsField, "/", 2)
	if len(procsParts) != 2 {
		return 0, badLoadavgFileContents
	}

	nprocs, err := strconv.ParseUint(procsParts[0], 10, 32)
	if err != nil {
		return 0, badLoadavgFileContents
	}
	return uint32(nprocs), nil
}

type cpuServer struct {
	// Protects CPU operations from concurrent access to prevent multiple ensureOnlineCPUs calls from running concurrently
	// and ensure that status response is always actual
	cpuOperationsMutex *sync.Mutex
	cpuScaler          *cpuscaling.CPUSysFsStateScaler
	logger             *zap.Logger
}

func (s *cpuServer) handleGetCPUStatus(w http.ResponseWriter) {
	s.cpuOperationsMutex.Lock()
	defer s.cpuOperationsMutex.Unlock()
	activeCPUs, err := s.cpuScaler.ActiveCPUsCount()
	if err != nil {
		s.logger.Error("could not get active CPUs count", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(fmt.Sprintf("%d", activeCPUs*1000))); err != nil {
		s.logger.Error("could not write response", zap.Error(err))
	}
}

func (s *cpuServer) handleSetCPUStatus(w http.ResponseWriter, r *http.Request) {
	s.cpuOperationsMutex.Lock()
	defer s.cpuOperationsMutex.Unlock()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Error("could not read request body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	updateInt, err := strconv.Atoi(string(body))
	update := vmv1.MilliCPU(updateInt)
	if err != nil {
		s.logger.Error("could not unmarshal request body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.logger.Info("Setting CPU status", zap.String("body", string(body)))
	if err := s.cpuScaler.EnsureOnlineCPUs(int(update.RoundedUp())); err != nil {
		s.logger.Error("could not ensure online CPUs", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *cpuServer) run(addr string) {

	mux := http.NewServeMux()
	mux.HandleFunc("/cpu", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.handleGetCPUStatus(w)
			return
		} else if r.Method == http.MethodPut {
			s.handleSetCPUStatus(w, r)
			return
		} else {
			// unknown method
			w.WriteHeader(http.StatusNotFound)
		}
	})

	timeout := 5 * time.Second
	server := http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadTimeout:       timeout,
		ReadHeaderTimeout: timeout,
		WriteTimeout:      timeout,
	}

	err := server.ListenAndServe()
	if err != nil {
		s.logger.Fatal("CPU server exited with error", zap.Error(err))
	}
	s.logger.Info("CPU server exited without error")
}
