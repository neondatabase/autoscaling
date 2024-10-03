package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/neondatabase/autoscaling/neonvm/daemon/pkg/cpuscaling"
	"go.uber.org/zap"
)

func main() {
	addr := flag.String("addr", "", `address to bind for HTTP requests`)
	flag.Parse()

	if *addr == "" {
		fmt.Println("neonvm-daemon missing -addr flag")
		os.Exit(1)
	}

	logConfig := zap.NewProductionConfig()
	logConfig.Sampling = nil                // Disable sampling, which the production config enables by default.
	logConfig.Level.SetLevel(zap.InfoLevel) // Only "info" level and above (i.e. not debug logs)
	logger := zap.Must(logConfig.Build()).Named("neonvm-daemon")
	defer logger.Sync() //nolint:errcheck // what are we gonna do, log something about it?

	logger.Info("Starting neonvm-daemon", zap.String("addr", *addr))
	cpuHotPlugController := &cpuscaling.CPUOnlineOffliner{}
	srv := cpuServer{
		cpuSystemWideScaler: cpuHotPlugController,
		logger:              logger.Named("cpu-srv"),
	}
	srv.run(*addr)
}

type cpuServer struct {
	// Protects CPU operations from concurrent access to prevent multiple ensureOnlineCPUs calls from running concurrently
	// and ensure that status response is always actual
	cpuOperationsMutex  *sync.Mutex
	cpuSystemWideScaler *cpuscaling.CPUOnlineOffliner
	logger              *zap.Logger
}

// milliCPU is a type that represents CPU in milli units
type milliCPU uint64

// milliCPUFromString converts a byte slice to milliCPU
func milliCPUFromString(s []byte) (milliCPU, error) {
	cpu, err := strconv.ParseUint(string(s), 10, 32)
	if err != nil {
		return 0, err
	}
	return milliCPU(cpu), nil
}

// ToCPU converts milliCPU to CPU
func (m milliCPU) ToCPU() int {
	cpu := float64(m) / 1000.0
	// Use math.Ceil to round up to the next CPU.
	return int(math.Ceil(cpu))
}

func (s *cpuServer) handleGetCPUStatus(w http.ResponseWriter) {
	s.cpuOperationsMutex.Lock()
	defer s.cpuOperationsMutex.Unlock()
	totalCPUs, err := s.cpuSystemWideScaler.GetTotalCPUsCount()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	activeCPUs, err := s.cpuSystemWideScaler.GetActiveCPUsCount()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write([]byte(fmt.Sprintf("%d %d", activeCPUs, totalCPUs)))
	w.WriteHeader(http.StatusOK)
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

	milliCPU, err := milliCPUFromString(body)
	if err != nil {
		s.logger.Error("could not parse request body as uint32", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := s.cpuSystemWideScaler.EnsureOnlineCPUs(milliCPU.ToCPU()); err != nil {
		s.logger.Error("failed to ensure online CPUs", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
