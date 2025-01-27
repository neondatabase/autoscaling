package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/neonvm/cpuscaling"
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
	srv := cpuServer{
		cpuOperationsMutex: &sync.Mutex{},
		cpuScaler:          cpuscaling.NewCPUScaler(),
		logger:             logger.Named("cpu-srv"),
	}
	srv.run(*addr)
}

type cpuServer struct {
	// Protects CPU operations from concurrent access to prevent multiple ensureOnlineCPUs calls from running concurrently
	// and ensure that status response is always actual
	cpuOperationsMutex  *sync.Mutex
	cpuScaler           *cpuscaling.CPUScaler
	fileOperationsMutex *sync.Mutex
	logger              *zap.Logger
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

func (s *cpuServer) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	s.fileOperationsMutex.Lock()
	defer s.fileOperationsMutex.Unlock()

	path := r.PathValue("path")
	if !filepath.IsLocal(path) {
		s.logger.Error("path is not local")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	path = filepath.Clean(filepath.Join("/var/sync", path))

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.logger.Error("could not create directory", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	file, err := os.CreateTemp("", "")
	if err != nil {
		s.logger.Error("could not create file", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer file.Close()
	defer os.Remove(file.Name())

	if _, err := io.Copy(file, r.Body); err != nil {
		s.logger.Error("could not read request body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// rename is an atomic operation on unix.
	// this ensures that the file is either not updated at all,
	// or is updated entirely.
	//
	// this ensures that other processes reading the file will never
	// have any inconsistencies. they will either read the old contents
	// or the new contents. Any open files will still point to the old inode
	// and thus still read the old contents.
	if err := os.Rename(file.Name(), path); err != nil {
		s.logger.Error("could not rename file", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *cpuServer) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	s.fileOperationsMutex.Lock()
	defer s.fileOperationsMutex.Unlock()

	path := r.PathValue("path")
	if !filepath.IsLocal(path) {
		s.logger.Error("path is not local")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	path = filepath.Clean(filepath.Join("/var/sync", path))

	if err := os.Remove(path); err != nil {
		s.logger.Error("could not delete file", zap.Error(err))
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
	mux.HandleFunc("/files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			s.handleUploadFile(w, r)
			return
		} else if r.Method == http.MethodDelete {
			s.handleDeleteFile(w, r)
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
