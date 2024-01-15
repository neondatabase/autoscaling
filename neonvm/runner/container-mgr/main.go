package main

// container-mgr ('neonvm-container-runner') runs in a container alongside neonvm-runner, and is
// responsible for updating the CPU shares & quotas for neonvm-runner by communicating directly with
// containerd (via CRI API, over containerd's socket).
//
// We have to go this back-channel route because kubernetes <1.27 doesn't support updating container
// resources without restarting, and with cgroups v2, kubernetes 1.25+ uses cgroup namespaces which
// prevents our ability to interact with cgroups inside the container *except by* going through the
// containerd API.
//
// We use a separate container to limit possibilities for privilege escalation.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
)

const (
	runnerContainerName = "neonvm-runner"

	retryInitEvery = 5 * time.Second

	// cpuLimitOvercommitFactor sets the amount above the VM's spec.guest.cpus.use that we set the
	// QEMU cgroup's CPU limit to. e.g. if cpuLimitOvercommitFactor = 3 and the VM is using 0.5
	// CPUs, we set the cgroup to limit QEMU+VM to 1.5 CPUs.
	//
	// This exists because setting the cgroup exactly equal to the VM's CPU value is overly
	// pessimistic, results in a lot of unused capacity on the host, and particularly impacts
	// operations that parallelize between the VM and QEMU, like heavy disk access.
	//
	// See also: https://neondb.slack.com/archives/C03TN5G758R/p1693462680623239
	cpuLimitOvercommitFactor = 4

	// number of CPU shares per vCPU to use when updating a container
	sharesPerCPU = 1024
	cpuPeriod    = 100000
)

func main() {
	logger := zap.Must(zap.NewProduction()).Named("neonvm-container-mgr")

	selfPodUID, ok := os.LookupEnv("K8S_POD_UID")
	if !ok {
		logger.Fatal("environment variable K8S_POD_UID missing")
	}
	logger.Info("Got pod UID", zap.String("uid", selfPodUID))

	containerRuntimeEndpoint, ok := os.LookupEnv("CRI_ENDPOINT")
	if !ok {
		logger.Fatal("environment variable CRI_ENDPOINT missing")
	}
	logger.Info("Got CRI endpoint", zap.String("endpoint", containerRuntimeEndpoint))

	crictl := &Crictl{
		endpoint: containerRuntimeEndpoint,
	}

	var httpPort int
	var initMilliCPU int
	flag.IntVar(&httpPort, "port", -1, "Port for the CPU http server")
	flag.IntVar(&initMilliCPU, "init-milli-cpu", -1, "Initial milli-CPU to use for the VM")
	flag.Parse()

	if httpPort < 0 {
		logger.Fatal("missing 'port' flag")
	} else if initMilliCPU < 0 {
		logger.Fatal("missing 'init-milli-cpu' flag")
	}

	pods, err := crictl.Pods(logger)
	if err != nil {
		logger.Fatal("failed to run crictl command to get CRI ID for pod UID", zap.String("uid", selfPodUID), zap.Error(err))
	}

	// find pod with matching uid
	var criPodID string
	for _, p := range pods.Items {
		if p.Metadata.UID == selfPodUID {
			criPodID = p.ID
			break
		}
	}
	if criPodID == "" {
		logger.Fatal("could not find CRI pod with matching UID", zap.String("uid", selfPodUID))
	}

	logger.Info("Got CRI ID for pod", zap.String("podID", criPodID))

	var criRunnerContainerID string
	for criRunnerContainerID == "" {
		containers, err := crictl.Ps(logger, criPodID)
		if err != nil {
			logger.Fatal(
				"failed to run crictl command to get CRI ID for container",
				zap.String("podID", criPodID),
				zap.String("name", runnerContainerName),
				zap.Error(err),
			)
		}

		for _, c := range containers.Containers {
			if c.Metadata.Name == runnerContainerName {
				criRunnerContainerID = c.ID
				break
			}
		}
		if criRunnerContainerID == "" {
			logger.Error(
				"could not find CRI container with matching name",
				zap.String("podID", criPodID),
				zap.String("name", runnerContainerName),
			)
			time.Sleep(retryInitEvery)
		}
	}

	logger.Info(
		fmt.Sprintf("Got CRI ID for %s container", runnerContainerName),
		zap.String("containerID", criRunnerContainerID),
	)

	// Set the CPU to initMilliCPU:
	err = updateContainerCPU(logger, crictl, criRunnerContainerID, vmv1.MilliCPU(initMilliCPU))
	if err != nil {
		logger.Fatal("could not set initial runner container CPU", zap.Error(err))
	}

	srvState := cpuServerState{
		podID:        criPodID,
		containerID:  criRunnerContainerID,
		lastMilliCPU: atomic.Uint32{},
	}
	srvState.lastMilliCPU.Store(uint32(initMilliCPU))

	srvState.listenForCPUChanges(context.TODO(), logger, crictl, int32(httpPort))
}

type cpuServerState struct {
	podID        string
	containerID  string
	lastMilliCPU atomic.Uint32
}

func (s *cpuServerState) listenForCPUChanges(ctx context.Context, logger *zap.Logger, crictl *Crictl, port int32) {
	mux := http.NewServeMux()
	loggerHandlers := logger.Named("http-handlers")
	cpuChangeLogger := loggerHandlers.Named("cpu_change")
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		w.WriteHeader(200)
	})
	mux.HandleFunc("/cpu_change", func(w http.ResponseWriter, r *http.Request) {
		s.handleCPUChange(cpuChangeLogger, crictl, w, r)
	})
	cpuCurrentLogger := loggerHandlers.Named("cpu_current")
	mux.HandleFunc("/cpu_current", func(w http.ResponseWriter, r *http.Request) {
		s.handleCPUCurrent(cpuCurrentLogger, crictl, w, r)
	})
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
			logger.Info("cpu_change server closed")
		} else if err != nil {
			logger.Fatal("cpu_change server exited with error", zap.Error(err))
		}
	case <-ctx.Done():
		err := server.Shutdown(context.Background())
		logger.Info("shut down cpu_change server", zap.Error(err))
	}
}

func (s *cpuServerState) handleCPUChange(logger *zap.Logger, crictl *Crictl, w http.ResponseWriter, r *http.Request) {
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

	logger.Info("got CPU update", zap.Float64("CPU", parsed.VCPUs.AsFloat64()))

	if err := updateContainerCPU(logger, crictl, s.containerID, parsed.VCPUs); err != nil {
		logger.Error("could not update container CPU", zap.String("id", s.containerID), zap.Error(err))
		w.WriteHeader(500)
		return
	}

	// store the milli CPU now that we've set it, so that in handleCPUCurrent we can handle rounding
	// issues based on the last operation we did.
	s.lastMilliCPU.Store(uint32(parsed.VCPUs))

	w.WriteHeader(200)
}

func updateContainerCPU(logger *zap.Logger, crictl *Crictl, containerID string, cpu vmv1.MilliCPU) error {
	shares := sharesForCPU(cpu)
	quota := int64(vmv1.MilliCPU(cpuLimitOvercommitFactor*cpu).AsFloat64() * float64(cpuPeriod))

	logger.Info(
		"calculated CPU quantities for vCPU",
		zap.Int("shares", shares),
		zap.Int64("quota", quota),
		zap.Int("period", cpuPeriod),
	)

	// update container
	return crictl.Update(logger, containerID, CrictlContainerUpdate{
		cpuShares: shares,
		cpuQuota:  quota,
		cpuPeriod: cpuPeriod,
	})
}

func (s *cpuServerState) handleCPUCurrent(logger *zap.Logger, crictl *Crictl, w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		logger.Error("unexpected method", zap.String("method", r.Method))
		w.WriteHeader(400)
		return
	}

	logger.Info("got CPU current request")

	container, err := crictl.Inspect(logger, s.containerID)
	if err != nil {
		logger.Error("could not inspect container", zap.String("id", s.containerID), zap.Error(err))
	}

	shares := int(container.Info.RuntimeSpec.Linux.Resources.CPU.Shares)

	logger.Info(
		"fetched current CPU shares",
		zap.Int("shares", shares),
	)

	last := vmv1.MilliCPU(s.lastMilliCPU.Load())
	expectedIfNoChange := sharesForCPU(last)

	var resp api.VCPUCgroup
	if shares == expectedIfNoChange {
		resp = api.VCPUCgroup{VCPUs: last}
	} else {
		resp = api.VCPUCgroup{VCPUs: cpuForShares(shares)}
	}

	logger.Info("responding with current CPU", zap.Float64("cpu", resp.VCPUs.AsFloat64()))

	body, err := json.Marshal(resp)
	if err != nil {
		logger.Error("could not marshal body", zap.Error(err))
		w.WriteHeader(500)
		return
	}

	w.Header().Add("Content-Type", "application/json")
	w.Write(body) //nolint:errcheck // Not much to do with the error here. TODO: log it?
}

func sharesForCPU(cpu vmv1.MilliCPU) int {
	return sharesPerCPU * int(cpu) / 1000
}

func cpuForShares(shares int) vmv1.MilliCPU {
	return vmv1.MilliCPU(shares * 1000 / sharesPerCPU)
}
