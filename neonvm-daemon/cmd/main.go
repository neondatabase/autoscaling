package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/neonvm/cpuscaling"
)

const defaultControlSocketPath = "/neonvm/run/neonvm-daemon-socket"

func main() {
	addr := flag.String("addr", "", `address to bind for HTTP requests`)
	controlSocketPathArg := flag.String("control-socket-path", "", `path for control socket`)
	quotaPath := flag.String("quota-path", "", `path controlled by disk quota`)
	flag.Parse()

	if *addr == "" {
		fmt.Println("neonvm-daemon missing -addr flag")
		os.Exit(1)
	}

	controlSocketPath := *controlSocketPathArg
	if controlSocketPath == "" {
		controlSocketPath = defaultControlSocketPath
	}

	logConfig := zap.NewProductionConfig()
	logConfig.Sampling = nil                // Disable sampling, which the production config enables by default.
	logConfig.Level.SetLevel(zap.InfoLevel) // Only "info" level and above (i.e. not debug logs)
	logger := zap.Must(logConfig.Build()).Named("neonvm-daemon")
	defer logger.Sync() //nolint:errcheck // what are we gonna do, log something about it?

	// neonvm-daemon has two duties:
	//
	// 1. Provide interface to let the VM host to scale the # of CPUs up and down
	// 2. Provide interface for the payload within the VM to perform some privileged
	//    operations. Currently: set swap size and disk quota.

	var wg sync.WaitGroup

	// 1. Launch outward-facing HTTP server for the CPU scaling
	logger.Info("Starting neonvm-daemon", zap.String("addr", *addr))
	srv := cpuServer{
		cpuOperationsMutex: &sync.Mutex{},
		cpuScaler:          cpuscaling.NewCPUScaler(),
		logger:             logger.Named("cpu-srv"),
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.run(*addr)
	}()

	// 2. Launch HTTP server on local unix domain socket, for the internal interface
	controlSocketServer :=  controlSocketServer{
		mutex: &sync.Mutex{},
		logger:             logger.Named("control-socket-srv"),
		quotaPath: *quotaPath,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		controlSocketServer.run(controlSocketPath)
	}()

	wg.Wait()
}

type cpuServer struct {
	// Protects CPU operations from concurrent access to prevent multiple ensureOnlineCPUs calls from running concurrently
	// and ensure that status response is always actual
	cpuOperationsMutex *sync.Mutex
	cpuScaler          *cpuscaling.CPUScaler
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
	defer r.Body.Close()

	updateInt, err := strconv.Atoi(string(body))
	if err != nil {
		s.logger.Error("could not unmarshal request body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.logger.Debug("Setting CPU status", zap.String("body", string(body)))
	update := vmv1.MilliCPU(updateInt)
	if err := s.cpuScaler.ReconcileOnlineCPU(int(update.RoundedUp())); err != nil {
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

	s.logger.Info("listening on TCP addr", zap.String("addr", addr))
	err := server.ListenAndServe()
	if err != nil {
		s.logger.Fatal("CPU server exited with error", zap.Error(err))
	}
	s.logger.Info("CPU server exited without error")
}


type controlSocketServer struct {
	// Protects operations from concurrent access. Not sure this matters for
	// the internal operations - we could resize swap and set disk quota at the
	// same time - but seems good to avoid confusion.
	mutex              *sync.Mutex
	logger             *zap.Logger

	quotaPath          string
	swapAlreadySet     bool
}


func (s *controlSocketServer) handleResizeSwapOnce(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Error("could not read request body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	size_str := string(body)
	size_bytes, err := strconv.ParseUint(size_str, 10, 64)
	if err != nil {
		s.logger.Error("invalid size in resize-swap request", zap.Error(err), zap.String("size_str", size_str))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if s.swapAlreadySet {
		s.logger.Error("swap was already resized, refusing to resize it again")
		w.WriteHeader(http.StatusConflict)
		return
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.logger.Debug("Resizing swap disk", zap.Uint64("size", size_bytes))
	err = resizeSwap(s.logger, size_bytes / 1024)
	if err != nil {
		s.logger.Error("could not resize swap", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	s.swapAlreadySet = true
	w.WriteHeader(http.StatusOK)
}

func (s *controlSocketServer) handleSetDiskQuota(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Error("could not read request body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	size_str := string(body)
	size_bytes, err := strconv.ParseUint(size_str, 10, 64)
	if err != nil {
		s.logger.Error("invalid size in set-disk-quota request", zap.Error(err), zap.String("size_str", size_str))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.logger.Debug("Setting disk quota", zap.Uint64("size", size_bytes))
	err = setDiskQuota(s.logger, s.quotaPath, size_bytes)
	if err != nil {
		s.logger.Error("could not set quota", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *controlSocketServer) run(controlSocketPath string) {
	controlSocketListener, err := net.Listen("unix", controlSocketPath)
	if err != nil {
		s.logger.Fatal("control socket server exited with error", zap.Error(err))
	}
	// Make the socket writable for all
	os.Chmod(controlSocketPath, 0777)

	mux := http.NewServeMux()
	mux.HandleFunc("/resize-swap-once", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.handleResizeSwapOnce(w, r)
			return
		} else {
			// unknown method
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/set-disk-quota", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if s.quotaPath != "" {
				s.handleSetDiskQuota(w, r)
			} else {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, "quota path not configured in neonvm-daemon\n")
			}
			return
		} else {
			// unknown method
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.logger.Error("unmatched request",
			zap.String("url", r.URL.String()),
		)
		w.WriteHeader(http.StatusNotFound)
	})

	timeout := 5 * time.Second
	server := http.Server{
		Handler:           mux,
		ReadTimeout:       timeout,
		ReadHeaderTimeout: timeout,
		WriteTimeout:      timeout,
	}

	s.logger.Info("listening on control socket yyyy", zap.String("path", controlSocketPath))
	err = server.Serve(controlSocketListener)
	if err != nil {
		s.logger.Fatal("control socket server exited with error", zap.Error(err))
	}
	s.logger.Info("control socket server exited without error")
}
