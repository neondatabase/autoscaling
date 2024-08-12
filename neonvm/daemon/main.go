package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// the default period is 100000 (i.e. 100 milliseconds). We use 5 milliseconds here because
// running out of quota can result in stalling until the end of the period, and a shorter period
// *generally* helps keep latencies more consistent (at the cost of using more CPU for scheduling).
const cpuPeriodMicroseconds = 5000

func main() {
	addr := flag.String("addr", "", `address to bind for HTTP requests`)
	cgroup := flag.String("cgroup", "", `cgroup for CPU limits`)
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

	logger.Info("Starting neonvm-daemon", zap.String("addr", *addr), zap.String("cgroup", *cgroup))

	srv := cpuServer{
		cgroup: *cgroup,
	}
	srv.run(logger, *addr)
}

type cpuServer struct {
	cgroup string
}

func (s *cpuServer) run(logger *zap.Logger, addr string) {
	logger = logger.Named("cpu-srv")

	mux := http.NewServeMux()
	mux.HandleFunc("/cpu", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = r.Body.Close()

			cpu, err := s.getCPU(logger)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.WriteHeader(http.StatusOK)
			w.Write([]byte(fmt.Sprintf("%d", cpu)))
		} else if r.Method == http.MethodPut {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				logger.Error("could not read request body", zap.Error(err))
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			milliCPU, err := strconv.ParseUint(string(body), 10, 32)
			if err != nil {
				logger.Error("could not parse request body as uint32", zap.Error(err))
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			s.setCPU(logger, uint32(milliCPU))
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
		logger.Fatal("CPU server exited with error", zap.Error(err))
	}
	logger.Info("CPU server exited without error")
}

func (s *cpuServer) cpuMaxPath() string {
	return fmt.Sprintf("/sys/fs/cgroup/%s/cpu.max", s.cgroup)
}

func (s *cpuServer) setCPU(logger *zap.Logger, milliCPU uint32) error {
	path := s.cpuMaxPath()
	quota := milliCPU * (cpuPeriodMicroseconds / 1000)

	fileContents := fmt.Sprintf("%d %d", quota, cpuPeriodMicroseconds)
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		logger.Error("could not open cgroup cpu.max file for writing", zap.Error(err))
		return err
	}

	_, err = file.WriteString(fileContents)
	if err != nil {
		logger.Error("could not write to cgroup cpu.max", zap.Error(err))
		return err
	}

	return nil
}

// returns the current CPU limit, measured in milli-CPUs
func (s *cpuServer) getCPU(logger *zap.Logger) (uint32, error) {
	data, err := os.ReadFile(s.cpuMaxPath())
	if err != nil {
		logger.Error("could not read cgroup cpu.max", zap.Error(err))
		return 0, err
	}

	cpuLimit, err := parseCgroupCPUMax(string(data))
	if err != nil {
		logger.Error("could not parse cgroup cpu.max", zap.Error(err))
		return 0, err
	}

	if cpuLimit.quota == nil {
		// "0" isn't quite correct here (maybe it should be 1<<32 - 1), but zero is a more typical
		// sentinel value, and will still produce the same results.
		return 0, nil
	}
	return uint32(1000 * (*cpuLimit.quota) / cpuLimit.period), nil
}

type cpuMax struct {
	quota  *uint64
	period uint64
}

func parseCgroupCPUMax(data string) (*cpuMax, error) {
	// the contents of cpu.max are "$MAX $PERIOD", where:
	// - $MAX is either a number of microseconds or the literal string "max" (meaning no limit), and
	// - $PERIOD is a number of microseconds over which to account $MAX
	arr := strings.Split(strings.Trim(string(data), "\n"), " ")
	if len(arr) != 2 {
		return nil, errors.New("unexpected contents of cgroup cpu.max")
	}

	var quota *uint64
	if arr[0] != "max" {
		q, err := strconv.ParseUint(arr[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("could not parse cpu quota: %w", err)
		}
		quota = &q
	}

	period, err := strconv.ParseUint(arr[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("could not parse cpu period: %w", err)
	}

	return &cpuMax{quota: quota, period: period}, nil
}
