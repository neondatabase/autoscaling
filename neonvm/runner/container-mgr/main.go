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

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/containerd/cgroups"
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
	var enableNetworkMonitoring bool
	flag.IntVar(&httpPort, "port", -1, "Port for the CPU http server")
	flag.IntVar(&initMilliCPU, "init-milli-cpu", -1, "Initial milli-CPU to use for the VM")
	flag.BoolVar(&enableNetworkMonitoring, "enable-network-monitoring", false, "Enable the eBPF function that measures ingress/egress traffic")
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

	var counters *ebpf.Map
	if enableNetworkMonitoring {
		podInspect, err := crictl.InspectPod(logger, criPodID)
		if err != nil {
			logger.Fatal(
				"failed to run crictl command to get cgroup path for container",
				zap.String("containerID", criRunnerContainerID),
				zap.Error(err),
			)
		}
		links, cs, err := attachBPF(logger, podInspect.Info.Config.Linux.CgroupParent)
		counters = cs // To prevent shadowing
		if err != nil {
			logger.Fatal("Failed to attach BPF", zap.Error(err))
		}
		defer links[NETWORK_INGRESS].Close()
		defer links[NETWORK_EGRESS].Close()
		defer counters.Close()
	}

	srvState := cpuServerState{
		podID:        criPodID,
		containerID:  criRunnerContainerID,
		lastMilliCPU: atomic.Uint32{},
	}
	srvState.lastMilliCPU.Store(uint32(initMilliCPU))

	srvState.listenForHTTPRequests(context.TODO(), logger, crictl, int32(httpPort), counters)
}

type NetworkDirection uint32

const (
	NETWORK_INGRESS NetworkDirection = 0
	NETWORK_EGRESS  NetworkDirection = 1
)

func generateBPF(logger *zap.Logger, counters *ebpf.Map, direction NetworkDirection) (*ebpf.Program, error) {
	const (
		ENABLE_DEBUG_PRINTING bool = false

		// Below are offsets into `struct __sk_buff`, defined in uapi/linux/bpf.h
		// https://elixir.bootlin.com/linux/v4.15/source/include/uapi/linux/bpf.h#L799
		LEN_OFFSET        = 0
		REMOTE_IP4_OFFSET = 92

		CLASS_A_MASK int32 = 255              // bswap(255.0.0.0)
		CLASS_B_MASK int32 = 255 + (240 << 8) // bswap(255.240.0.0)
		CLASS_C_MASK int32 = 255 + (255 << 8) // bswap(255.255.0.0)

		CLASS_A_ADDRESS  int32 = 10                                     // bswap(10.0.0.0)
		CLASS_B_ADDRESS  int32 = 172 + (16 << 8)                        // bswap(172.16.0.0)
		CLASS_C_ADDRESS  int32 = 192 + (168 << 8)                       // bswap(192.168.0.0)
		LOOPBACK_ADDRESS int32 = 127 + (0 << 8) + (0 << 16) + (1 << 24) // bswap(127.0.0.1)
	)

	insns := asm.Instructions{
		// R1 contains a pointer to an __sk_buff (defined in bpf.h)
		// Load length, and remote_ip4 into R6 and R7
		asm.LoadMem(asm.R6, asm.R1, LEN_OFFSET, asm.Word),
		asm.LoadMem(asm.R7, asm.R1, REMOTE_IP4_OFFSET, asm.Word),
	}

	if ENABLE_DEBUG_PRINTING {
		insns = append(insns,
			// Store the string "%d\n\0" to the stack
			asm.Mov.Reg(asm.R1, asm.RFP),
			asm.Add.Imm(asm.R1, -4),
			asm.StoreImm(asm.R1, 0, 0x000a6425, asm.Word),
			// Store the size of the string in R2 (which is 4
			// including the null terminator)
			asm.Mov.Imm(asm.R2, 4),
			// Store the value we want to print (the IP address) in R3
			asm.Mov.Reg(asm.R3, asm.R7),
			asm.FnTracePrintk.Call(),
		)
	}

	insns = append(insns,
		// Check if remote address is one of:
		// - empty address (0.0.0.0)
		// - loopback address (127.0.0.1)
		// - class A private network (10.0.0.0/8)
		// - class B private network (172.16.0.0/12)
		// - class C private network (192.168.0.0/16)
		// If it is any of these, exit
		asm.JEq.Imm32(asm.R7, 0, "exit"),
		asm.JEq.Imm32(asm.R7, LOOPBACK_ADDRESS, "exit"),
		asm.Mov.Reg32(asm.R8, asm.R7),
		asm.And.Imm32(asm.R8, CLASS_A_MASK),
		asm.JEq.Imm32(asm.R8, CLASS_A_ADDRESS, "exit"),
		asm.Mov.Reg32(asm.R8, asm.R7),
		asm.And.Imm32(asm.R8, CLASS_B_MASK),
		asm.JEq.Imm32(asm.R8, CLASS_B_ADDRESS, "exit"),
		asm.Mov.Reg32(asm.R8, asm.R7),
		asm.And.Imm32(asm.R8, CLASS_C_MASK),
		asm.JEq.Imm32(asm.R8, CLASS_C_ADDRESS, "exit"),

		// Load the map into R1
		// By the way: isn't it funny how we're putting a file descriptor
		// here? Even though this program is getting shared among multiple
		// processes?
		// Well, it turns out that when this program gets loaded into the kernel
		// there's a special pass that converts these LoadMapPtr calls from FDs
		// into pointers to kernel memory. Pretty nifty!
		asm.LoadMapPtr(asm.R1, counters.FD()),

		// bpf_map_lookup_elem takes a pointer to the key, so we
		// have to spill the key to the stack. The key is the
		// network direction (i.e. 0 for ingress and 1 for egress).
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -4),
		asm.StoreImm(asm.R2, 0, int64(direction), asm.Word),
		// Now we have a pointer to the map in R1 and
		// a pointer to the key in R2.
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		// R0 now holds a pointer to the counter
		// Atomically add the packet length (stored in R6) to it
		asm.StoreXAdd(asm.R0, asm.R6, asm.DWord),
		asm.Mov.Imm(asm.R0, 1).WithSymbol("exit"),
		asm.Return(),
	)

	//nolint:exhaustruct // This has a whole bunch of optional configuration options that we don't care about
	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:         "neon_network_monitor",
		Type:         ebpf.CGroupSKB,
		License:      "Apache-2.0",
		Instructions: insns,
	})
	if err != nil {
		logger.Fatal("Failed to load eBPF program into the kernel", zap.Error(err))
		return nil, err
	}
	return prog, nil
}

// Attaches a BPF program to the given cgroup which checks each packet to see if it comes
// from/is going to the external network. These counts are exposed through a map which
// can be read.
func attachBPF(logger *zap.Logger, cgroupPath string) ([]link.Link, *ebpf.Map, error) {
	const (
		MAP_KEY_SIZE = 4
		MAP_VAL_SIZE = 8
	)

	cgroupMountPoint := "/sys/fs/cgroup/net_cls,net_prio"
	if cgroups.Mode() == cgroups.Unified {
		cgroupMountPoint = "/sys/fs/cgroup"
	}

	cgroupPath = fmt.Sprintf("%s/%s", cgroupMountPoint, cgroupPath)

	if err := rlimit.RemoveMemlock(); err != nil {
		logger.Fatal("Failed to remove memlock", zap.Error(err))
		return nil, nil, err
	}

	//nolint:exhaustruct // This has a whole bunch of optional configuration options that we don't care about
	counters, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.Array,
		KeySize:    MAP_KEY_SIZE,
		ValueSize:  MAP_VAL_SIZE,
		MaxEntries: 2,
	})
	if err != nil {
		logger.Fatal("Failed to create eBPF map", zap.Error(err))
		return nil, nil, err
	}

	ingressProgram, err := generateBPF(logger, counters, NETWORK_INGRESS)
	if err != nil {
		counters.Close()
		return nil, nil, err
	}
	ingressLink, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupInetIngress,
		Program: ingressProgram,
	})

	logger.Info("Attempting to link to cgroup", zap.String("cgroup_path", cgroupPath))
	if err != nil {
		logger.Fatal("Failed to link ingress program", zap.Error(err))
		return nil, nil, err
	}

	egressProgram, err := generateBPF(logger, counters, NETWORK_EGRESS)
	if err != nil {
		counters.Close()
		return nil, nil, err
	}

	egressLink, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupInetEgress,
		Program: egressProgram,
	})
	if err != nil {
		logger.Fatal("Failed to link egress program", zap.Error(err))
		counters.Close()
		return nil, nil, err
	}
	logger.Info("Successfully attached eBPF programs")
	return []link.Link{ingressLink, egressLink}, counters, nil
}

type cpuServerState struct {
	podID        string
	containerID  string
	lastMilliCPU atomic.Uint32
}

func (s *cpuServerState) listenForHTTPRequests(ctx context.Context, logger *zap.Logger, crictl *Crictl, port int32, counters *ebpf.Map) {
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
	if counters != nil {
		networkUsageLogger := loggerHandlers.Named("network_usage")
		logger.Info("Listening for network_usage")
		mux.HandleFunc("/network_usage", func(w http.ResponseWriter, r *http.Request) {
			handleGetNetworkUsage(networkUsageLogger, w, r, counters)
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
			logger.Fatal("http exited with error", zap.Error(err))
		}
	case <-ctx.Done():
		err := server.Shutdown(context.Background())
		logger.Info("shut down http server", zap.Error(err))
	}
}

func handleGetNetworkUsage(logger *zap.Logger, w http.ResponseWriter, r *http.Request, counters *ebpf.Map) {
	if r.Method != "GET" {
		logger.Error("unexpected method", zap.String("method", r.Method))
		w.WriteHeader(400)
		return
	}

	var counts vmv1.VirtualMachineNetworkUsage
	if err := counters.Lookup(NETWORK_INGRESS, &counts.IngressBytes); err != nil {
		logger.Error("error reading ingress byte counts", zap.Error(err))
		w.WriteHeader(500)
		return
	}
	if err := counters.Lookup(NETWORK_EGRESS, &counts.EgressBytes); err != nil {
		logger.Error("error reading egress byte counts", zap.Error(err))
		w.WriteHeader(500)
		return
	}
	body, err := json.Marshal(counts)
	if err != nil {
		logger.Error("could not marshal byte counts", zap.Error(err))
		w.WriteHeader(500)
		return
	}

	w.Header().Add("Content-Type", "application/json")
	w.Write(body) //nolint:errcheck // Not much to do with the error here. TODO: log it?
	logger.Info("Responded with byte counts", zap.String("byte_counts", string(body)))
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
