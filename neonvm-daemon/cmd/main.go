package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/samber/lo"
	"go.uber.org/zap"

	k8sutil "k8s.io/kubernetes/pkg/volume/util"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/neonvm/cpuscaling"
	"github.com/neondatabase/autoscaling/pkg/util"
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
		cpuOperationsMutex:  &sync.Mutex{},
		cpuScaler:           cpuscaling.NewCPUScaler(),
		fileOperationsMutex: &sync.Mutex{},
		logger:              logger.Named("cpu-srv"),
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

func (s *cpuServer) handleGetFileChecksum(w http.ResponseWriter, r *http.Request, path string) {
	s.fileOperationsMutex.Lock()
	defer s.fileOperationsMutex.Unlock()

	if err := r.Context().Err(); err != nil {
		w.WriteHeader(http.StatusRequestTimeout)
		return
	}

	dir := filepath.Join(path, "..data")
	checksum, err := util.ChecksumFlatDir(dir)
	if err != nil {
		s.logger.Error("could not checksum dir", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(checksum)); err != nil {
		s.logger.Error("could not write response", zap.Error(err))
	}
}

type File struct {
	// base64 encoded file contents
	Data string `json:"data"`
	// the name of the user that should own this file
	User string `json:"user"`
	// the file mode
	Mode int32 `json:"mode"`
}

func (s *cpuServer) handleUploadFile(w http.ResponseWriter, r *http.Request, path string) {
	s.fileOperationsMutex.Lock()
	defer s.fileOperationsMutex.Unlock()

	if err := r.Context().Err(); err != nil {
		w.WriteHeader(http.StatusRequestTimeout)
		return
	}

	if r.Body == nil {
		s.logger.Error("no body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Error("could not ready body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var files map[string]File
	if err := json.Unmarshal(body, &files); err != nil {
		s.logger.Error("could not ready body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	payload := make(map[string]k8sutil.FileProjection)
	for k, v := range files {
		data, err := base64.StdEncoding.DecodeString(v.Data)
		if err != nil {
			s.logger.Error("could not ready body", zap.Error(err))
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		mode := v.Mode
		if mode == 0 {
			// read-write by owner
			// read-only otherwise
			mode = 0o644
		}

		var fsuser *int64
		if v.User != "" {
			user, err := user.Lookup(v.User)
			if err != nil {
				s.logger.Error("invalid user", zap.Error(err))
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Uid is the user ID. On POSIX systems, this is a decimal number representing the uid.
			uid, err := strconv.Atoi(user.Uid)
			if err != nil {
				s.logger.Error("invalid user id", zap.Error(err))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fsuser = lo.ToPtr(int64(uid))
		}

		payload[k] = k8sutil.FileProjection{
			Data:   data,
			Mode:   mode,
			FsUser: fsuser,
		}
	}

	aw, err := k8sutil.NewAtomicWriter(path, "neonvm-daemon")
	if err != nil {
		s.logger.Error("could not create writer", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if err := aw.Write(payload, nil); err != nil {
		s.logger.Error("could not create files", zap.Error(err))
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
		path := fmt.Sprintf("/%s", r.PathValue("path"))
		if r.Method == http.MethodGet {
			s.handleGetFileChecksum(w, r, path)
			return
		} else if r.Method == http.MethodPut {
			s.handleUploadFile(w, r, path)
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
