package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/digitalocean/go-qemu/qmp"
	"github.com/jpillora/backoff"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/samber/lo"
	"go.uber.org/zap"

	"k8s.io/apimachinery/pkg/api/resource"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util/taskgroup"
)

const (
	qemuBinArm64 = "qemu-system-aarch64"
	qemuBinX8664 = "qemu-system-x86_64"
	qemuImgBin   = "qemu-img"

	architectureArm64 = "arm64"
	architectureAmd64 = "amd64"
	defaultKernelPath = "/vm/kernel/vmlinuz"

	qmpUnixSocketForSigtermHandler = "/vm/qmp-sigterm.sock"
	logSerialSocket                = "/vm/log.sock"
	bufferedReaderSize             = 4096

	// cgroupPeriod is the period for evaluating cgroup quota
	// in microseconds. Min 1000 microseconds, max 1 second
	cgroupPeriod     = uint64(100000)
	cgroupMountPoint = "/sys/fs/cgroup"

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
)

func checkKVM() bool {
	info, err := os.Stat("/dev/kvm")
	if err != nil {
		return false
	}
	mode := info.Mode()

	return mode&os.ModeCharDevice == os.ModeCharDevice
}

func checkDevTun() bool {
	info, err := os.Stat("/dev/net/tun")
	if err != nil {
		return false
	}
	mode := info.Mode()

	return mode&os.ModeCharDevice == os.ModeCharDevice
}

func runInitScript(logger *zap.Logger, script string) error {
	if len(script) == 0 {
		return nil
	}

	// creates a tmp file with the script content
	tmpFile, err := os.CreateTemp(os.TempDir(), "init-script-")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name()) // clean up

	if _, err := tmpFile.Write([]byte(script)); err != nil {
		return err
	}

	if err := tmpFile.Close(); err != nil {
		return err
	}

	logger.Info("running init script", zap.String("path", tmpFile.Name()))

	if err := execFg("/bin/sh", tmpFile.Name()); err != nil {
		return err
	}

	return nil
}

type Config struct {
	vmSpecDump           string
	vmStatusDump         string
	kernelPath           string
	appendKernelCmdline  string
	skipCgroupManagement bool
	diskCacheSettings    string
	// memoryProvider is a memory provider to use. Validated in newConfig.
	memoryProvider vmv1.MemoryProvider
	// autoMovableRatio value for VirtioMem provider. Validated in newConfig.
	autoMovableRatio string
	// cpuScalingMode is a mode to use for CPU scaling. Validated in newConfig.
	cpuScalingMode vmv1.CpuScalingMode
	// System CPU architecture. Set automatically equal to runtime.GOARCH.
	architecture string
}

func newConfig(logger *zap.Logger) *Config {
	cfg := &Config{
		vmSpecDump:           "",
		vmStatusDump:         "",
		kernelPath:           defaultKernelPath,
		appendKernelCmdline:  "",
		skipCgroupManagement: false,
		diskCacheSettings:    "cache=none",
		memoryProvider:       "",
		autoMovableRatio:     "",
		cpuScalingMode:       "",
		architecture:         runtime.GOARCH,
	}
	flag.StringVar(&cfg.vmSpecDump, "vmspec", cfg.vmSpecDump,
		"Base64 encoded VirtualMachine json specification")
	flag.StringVar(&cfg.vmStatusDump, "vmstatus", cfg.vmStatusDump,
		"Base64 encoded VirtualMachine json status")
	flag.StringVar(&cfg.kernelPath, "kernelpath", cfg.kernelPath,
		"Override path for kernel to use")
	flag.StringVar(&cfg.appendKernelCmdline, "appendKernelCmdline",
		cfg.appendKernelCmdline, "Additional kernel command line arguments")
	flag.BoolVar(&cfg.skipCgroupManagement, "skip-cgroup-management",
		cfg.skipCgroupManagement,
		"Don't try to manage CPU")
	flag.StringVar(&cfg.diskCacheSettings, "qemu-disk-cache-settings",
		cfg.diskCacheSettings, "Cache settings to add to -drive args for VM disks")
	flag.Func("memory-provider", "Set provider for memory hotplug", cfg.memoryProvider.FlagFunc)
	flag.StringVar(&cfg.autoMovableRatio, "memhp-auto-movable-ratio",
		cfg.autoMovableRatio, "Set value of kernel's memory_hotplug.auto_movable_ratio [virtio-mem only]")
	flag.Func("cpu-scaling-mode", "Set CPU scaling mode", cfg.cpuScalingMode.FlagFunc)
	flag.Parse()

	if cfg.memoryProvider == "" {
		logger.Fatal("missing required flag '-memory-provider'")
	}
	if cfg.memoryProvider == vmv1.MemoryProviderVirtioMem && cfg.autoMovableRatio == "" {
		logger.Fatal("missing required flag '-memhp-auto-movable-ratio'")
	}
	if cfg.cpuScalingMode == "" {
		logger.Fatal("missing required flag '-cpu-scaling-mode'")
	}

	return cfg
}

func main() {
	logger := zap.Must(zap.NewProduction()).Named("neonvm-runner")

	if err := run(logger); err != nil {
		logger.Fatal("Failed to run", zap.Error(err))
	}
}

func run(logger *zap.Logger) error {
	cfg := newConfig(logger)

	vmSpecJson, err := base64.StdEncoding.DecodeString(cfg.vmSpecDump)
	if err != nil {
		return fmt.Errorf("failed to decode VirtualMachine Spec dump: %w", err)
	}
	vmStatusJson, err := base64.StdEncoding.DecodeString(cfg.vmStatusDump)
	if err != nil {
		return fmt.Errorf("failed to decode VirtualMachine Status dump: %w", err)
	}

	vmSpec := &vmv1.VirtualMachineSpec{}
	if err := json.Unmarshal(vmSpecJson, vmSpec); err != nil {
		return fmt.Errorf("failed to unmarshal VM spec: %w", err)
	}
	var vmStatus vmv1.VirtualMachineStatus
	if err := json.Unmarshal(vmStatusJson, &vmStatus); err != nil {
		return fmt.Errorf("failed to unmarshal VM Status: %w", err)
	}

	enableSSH := false
	if vmSpec.EnableSSH != nil && *vmSpec.EnableSSH {
		enableSSH = true
	}

	// Set hostname, with "vm-" prefix to distinguish it from the pod name
	//
	// This is just to reduce the risk of mixing things up when ssh'ing to different
	// computes, the hostname isn't used for anything as such.
	hostname, err := os.Hostname()
	if err != nil {
		logger.Warn("could not read pod's hostname", zap.Error(err))
	} else {
		hostname = fmt.Sprintf("vm-%s", hostname)
	}

	sysctl := []string{
		"kernel.core_pattern=core",
		"kernel.core_uses_pid=1",
	}
	var shmSize *resource.Quantity
	var swapSize *resource.Quantity
	if vmSpec.Guest.Settings != nil {
		sysctl = append(sysctl, vmSpec.Guest.Settings.Sysctl...)
		swapSize = vmSpec.Guest.Settings.Swap

		// By default, Linux sets the size of /dev/shm to 1/2 of the physical memory.  If
		// swap is configured, we want to set /dev/shm higher, because we can autoscale
		// the memory up.
		//
		// See https://github.com/neondatabase/autoscaling/issues/800
		initialMemorySize := vmSpec.Guest.MemorySlotSize.Value() * int64(vmSpec.Guest.MemorySlots.Min)
		if swapSize != nil && swapSize.Value() > initialMemorySize/2 {
			shmSize = swapSize
		}
	}

	tg := taskgroup.NewGroup(logger)
	tg.Go("init-script", func(logger *zap.Logger) error {
		return runInitScript(logger, vmSpec.InitScript)
	})

	// create iso9660 disk with runtime options (command, args, envs, mounts)
	tg.Go("iso9660-runtime", func(logger *zap.Logger) error {
		return createISO9660runtime(
			runtimeDiskPath,
			vmSpec.Guest.Command,
			vmSpec.Guest.Args,
			sysctl,
			vmSpec.Guest.Env,
			vmSpec.Disks,
			enableSSH,
			swapSize,
			shmSize,
		)
	})

	tg.Go("rootDisk", func(logger *zap.Logger) error {
		// resize rootDisk image of size specified and new size more than current
		return resizeRootDisk(logger, vmSpec)
	})
	var qemuCmd []string

	tg.Go("qemu-cmd", func(logger *zap.Logger) error {
		var err error
		qemuCmd, err = buildQEMUCmd(cfg, logger, vmSpec, &vmStatus, enableSSH, swapSize, hostname)
		return err
	})

	if err := tg.Wait(); err != nil {
		return err
	}

	err = runQEMU(cfg, logger, vmSpec, qemuCmd)
	if err != nil {
		return fmt.Errorf("failed to run QEMU: %w", err)
	}

	return nil
}

func buildQEMUCmd(
	cfg *Config,
	logger *zap.Logger,
	vmSpec *vmv1.VirtualMachineSpec,
	vmStatus *vmv1.VirtualMachineStatus,
	enableSSH bool,
	swapSize *resource.Quantity,
	hostname string,
) ([]string, error) {
	// prepare qemu command line
	qemuCmd := []string{
		"-runas", "qemu",
		"-machine", getMachineType(cfg.architecture),
		"-nographic",
		"-no-reboot",
		"-nodefaults",
		"-only-migratable",
		"-audiodev", "none,id=noaudio",
		"-serial", "pty",
		"-msg", "timestamp=on",
		"-qmp", fmt.Sprintf("tcp:0.0.0.0:%d,server,wait=off", vmSpec.QMP),
		"-qmp", fmt.Sprintf("tcp:0.0.0.0:%d,server,wait=off", vmSpec.QMPManual),
		"-qmp", fmt.Sprintf("unix:%s,server,wait=off", qmpUnixSocketForSigtermHandler),
		"-device", "virtio-serial",
		"-chardev", fmt.Sprintf("socket,path=%s,server=on,wait=off,id=log", logSerialSocket),
		"-device", "virtserialport,chardev=log,name=tech.neon.log.0",
	}

	qemuDiskArgs, err := setupVMDisks(logger, cfg.diskCacheSettings, enableSSH, swapSize, vmSpec.Disks)
	if err != nil {
		return nil, err
	}
	qemuCmd = append(qemuCmd, qemuDiskArgs...)

	switch cfg.architecture {
	case architectureArm64:
		// add custom firmware to have ACPI working
		qemuCmd = append(qemuCmd, "-bios", "/vm/QEMU_EFI_ARM.fd")
		// arm virt has only one UART, setup virtio-serial to add more /dev/hvcX
		qemuCmd = append(qemuCmd,
			"-chardev", "stdio,id=virtio-console",
			"-device", "virtconsole,chardev=virtio-console",
		)
	case architectureAmd64:
		// on amd we have multiple UART ports so we can just use serial stdio
		qemuCmd = append(qemuCmd, "-serial", "stdio")
	default:
		logger.Fatal("unsupported architecture", zap.String("architecture", cfg.architecture))
	}

	// cpu details
	// NB: EnableAcceleration guaranteed non-nil because the k8s API server sets the default for us.
	if *vmSpec.EnableAcceleration && checkKVM() {
		logger.Info("using KVM acceleration")
		qemuCmd = append(qemuCmd, "-enable-kvm")
	} else {
		logger.Warn("not using KVM acceleration")
	}
	qemuCmd = append(qemuCmd, "-cpu", "max")

	// cpu scaling details
	maxCPUs := vmSpec.Guest.CPUs.Max.RoundedUp()
	minCPUs := vmSpec.Guest.CPUs.Min.RoundedUp()

	switch cfg.cpuScalingMode {
	case vmv1.CpuScalingModeSysfs:
		// Boot with all CPUs plugged, we will online them on-demand
		qemuCmd = append(qemuCmd, "-smp", fmt.Sprintf(
			"cpus=%d,maxcpus=%d,sockets=1,cores=%d,threads=1",
			maxCPUs,
			maxCPUs,
			maxCPUs,
		))
	case vmv1.CpuScalingModeQMP:
		// Boot with minCPUs hotplugged, but with slots reserved for maxCPUs.
		qemuCmd = append(qemuCmd, "-smp", fmt.Sprintf(
			"cpus=%d,maxcpus=%d,sockets=1,cores=%d,threads=1",
			minCPUs,
			maxCPUs,
			maxCPUs,
		))
	default:
		// we should never get here because we validate the flag in newConfig
		panic(fmt.Errorf("unknown CPU scaling mode %s", cfg.cpuScalingMode))
	}

	// memory details
	logger.Info(fmt.Sprintf("Using memory provider %s", cfg.memoryProvider))
	qemuCmd = append(qemuCmd, "-m", fmt.Sprintf(
		"size=%db,slots=%d,maxmem=%db",
		vmSpec.Guest.MemorySlotSize.Value()*int64(vmSpec.Guest.MemorySlots.Min),
		vmSpec.Guest.MemorySlots.Max-vmSpec.Guest.MemorySlots.Min,
		vmSpec.Guest.MemorySlotSize.Value()*int64(vmSpec.Guest.MemorySlots.Max),
	))
	if cfg.memoryProvider == vmv1.MemoryProviderVirtioMem {
		// we don't actually have any slots because it's virtio-mem, but we're still using the API
		// designed around DIMM slots, so we need to use them to calculate how much memory we expect
		// to be able to plug in.
		numSlots := vmSpec.Guest.MemorySlots.Max - vmSpec.Guest.MemorySlots.Min
		virtioMemSize := int64(numSlots) * vmSpec.Guest.MemorySlotSize.Value()
		// We can add virtio-mem if it actually needs to be a non-zero size.
		// Otherwise, QEMU fails with:
		//   property 'size' of memory-backend-ram doesn't take value '0'
		if virtioMemSize != 0 {
			qemuCmd = append(qemuCmd, "-object", fmt.Sprintf("memory-backend-ram,id=vmem0,size=%db", virtioMemSize))
			qemuCmd = append(qemuCmd, "-device", "virtio-mem-pci,id=vm0,memdev=vmem0,block-size=8M,requested-size=0")
		}
	}

	qemuNetArgs, err := setupVMNetworks(logger, vmSpec.Guest.Ports, vmSpec.ExtraNetwork)
	if err != nil {
		return nil, err
	}
	qemuCmd = append(qemuCmd, qemuNetArgs...)

	// kernel details
	qemuCmd = append(
		qemuCmd,
		"-kernel", cfg.kernelPath,
		"-append", makeKernelCmdline(cfg, logger, vmSpec, vmStatus, hostname),
	)

	// should runner receive migration ?
	if os.Getenv("RECEIVE_MIGRATION") == "true" {
		qemuCmd = append(qemuCmd, "-incoming", fmt.Sprintf("tcp:0:%d", vmv1.MigrationPort))
	}

	return qemuCmd, nil
}

const (
	baseKernelCmdline          = "panic=-1 init=/neonvm/bin/init loglevel=7 root=/dev/vda rw"
	kernelCmdlineDIMMSlots     = "memhp_default_state=online_movable"
	kernelCmdlineVirtioMemTmpl = "memhp_default_state=online memory_hotplug.online_policy=auto-movable memory_hotplug.auto_movable_ratio=%s"
)

func makeKernelCmdline(cfg *Config, logger *zap.Logger, vmSpec *vmv1.VirtualMachineSpec, vmStatus *vmv1.VirtualMachineStatus, hostname string) string {
	cmdlineParts := []string{baseKernelCmdline}

	switch cfg.memoryProvider {
	case vmv1.MemoryProviderDIMMSlots:
		cmdlineParts = append(cmdlineParts, kernelCmdlineDIMMSlots)
	case vmv1.MemoryProviderVirtioMem:
		cmdlineParts = append(cmdlineParts, fmt.Sprintf(kernelCmdlineVirtioMemTmpl, cfg.autoMovableRatio))
	default:
		panic(fmt.Errorf("unknown memory provider %s", cfg.memoryProvider))
	}

	if vmSpec.ExtraNetwork != nil && vmSpec.ExtraNetwork.Enable {
		netDetails := fmt.Sprintf("ip=%s:::%s:%s:eth1:off", vmStatus.ExtraNetIP, vmStatus.ExtraNetMask, vmStatus.PodName)
		cmdlineParts = append(cmdlineParts, netDetails)
	}

	if len(hostname) != 0 {
		cmdlineParts = append(cmdlineParts, fmt.Sprintf("hostname=%s", hostname))
	}

	if cfg.appendKernelCmdline != "" {
		cmdlineParts = append(cmdlineParts, cfg.appendKernelCmdline)
	}
	if cfg.cpuScalingMode == vmv1.CpuScalingModeSysfs {
		// Limit the number of online CPUs kernel boots with. More CPUs will be enabled on upscaling
		cmdlineParts = append(cmdlineParts, fmt.Sprintf("maxcpus=%d", vmSpec.Guest.CPUs.Min.RoundedUp()))
	}

	switch cfg.architecture {
	case architectureArm64:
		// explicitly enable acpi if we run on arm
		cmdlineParts = append(cmdlineParts, "acpi=on")
		// use virtio-serial device kernel console
		cmdlineParts = append(cmdlineParts, "console=hvc0")
	case architectureAmd64:
		cmdlineParts = append(cmdlineParts, "console=ttyS1")
	default:
		logger.Fatal("unsupported architecture", zap.String("architecture", cfg.architecture))
	}

	return strings.Join(cmdlineParts, " ")
}

func runQEMU(
	cfg *Config,
	logger *zap.Logger,
	vmSpec *vmv1.VirtualMachineSpec,
	qemuCmd []string,
) error {
	selfPodName, ok := os.LookupEnv("K8S_POD_NAME")
	if !ok {
		return fmt.Errorf("environment variable K8S_POD_NAME missing")
	}

	var cgroupPath string

	if !cfg.skipCgroupManagement {
		selfCgroupPath, err := getSelfCgroupPath(logger)
		if err != nil {
			return fmt.Errorf("Failed to get self cgroup path: %w", err)
		}
		// Sometimes we'll get just '/' as our cgroup path. If that's the case, we should reset it so
		// that the cgroup '/neonvm-qemu-...' still works.
		if selfCgroupPath == "/" {
			selfCgroupPath = ""
		}
		// ... but also we should have some uniqueness just in case, so we're not sharing a root level
		// cgroup if that *is* what's happening. This *should* only be relevant for local clusters.
		//
		// We don't want to just use the VM spec's .status.PodName because during migrations that will
		// be equal to the source pod, not this one, which may be... somewhat confusing.
		cgroupPath = fmt.Sprintf("%s/neonvm-qemu-%s", selfCgroupPath, selfPodName)

		logger.Info("Determined QEMU cgroup path", zap.String("path", cgroupPath))

		useCPU := vmSpec.Guest.CPUs.Use
		if err := setCgroupLimit(logger, useCPU, cgroupPath); err != nil {
			return fmt.Errorf("Failed to set cgroup limit: %w", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}

	wg.Add(1)
	go terminateQemuOnSigterm(ctx, logger, &wg)
	var callbacks cpuServerCallbacks
	// lastValue is used to store last fractional CPU request
	// we need to store the value as is because we can't convert it back from MilliCPU
	// and otherwise we would have infinite reconciliation loop
	// this will eventually be dropped in favor of real fractional CPU scaling based on the cgroups
	lastValue := &atomic.Uint32{}
	lastValue.Store(uint32(vmSpec.Guest.CPUs.Min))

	callbacks = cpuServerCallbacks{
		get: func(logger *zap.Logger) (*vmv1.MilliCPU, error) {
			return lo.ToPtr(vmv1.MilliCPU(lastValue.Load())), nil
		},
		set: func(logger *zap.Logger, cpu vmv1.MilliCPU) error {
			if cfg.cpuScalingMode == vmv1.CpuScalingModeSysfs {
				err := setNeonvmDaemonCPU(cpu)
				if err != nil {
					logger.Error("setting CPU through NeonVM Daemon failed", zap.Any("cpu", cpu), zap.Error(err))
					return err
				}
			}
			lastValue.Store(uint32(cpu))
			return nil
		},
	}

	wg.Add(1)
	monitoring := vmSpec.EnableNetworkMonitoring != nil && *vmSpec.EnableNetworkMonitoring
	go listenForHTTPRequests(ctx, logger, vmSpec.RunnerPort, callbacks, &wg, monitoring)
	wg.Add(1)
	go forwardLogs(ctx, logger, &wg)

	qemuBin := getQemuBinaryName(cfg.architecture)
	var bin string
	var cmd []string
	if !cfg.skipCgroupManagement {
		bin = "cgexec"
		cmd = append([]string{"-g", fmt.Sprintf("cpu:%s", cgroupPath), qemuBin}, qemuCmd...)
	} else {
		bin = qemuBin
		cmd = qemuCmd
	}

	logger.Info(fmt.Sprintf("calling %s", bin), zap.Strings("args", cmd))
	err := execFg(bin, cmd...)
	if err != nil {
		msg := "QEMU exited with error" // TODO: technically this might not be accurate. This can also happen if it fails to start.
		logger.Error(msg, zap.Error(err))
		err = fmt.Errorf("%s: %w", msg, err)
	} else {
		logger.Info("QEMU exited without error")
	}

	cancel()
	wg.Wait()

	return err
}

func getQemuBinaryName(architecture string) string {
	switch architecture {
	case architectureArm64:
		return qemuBinArm64
	case architectureAmd64:
		return qemuBinX8664
	default:
		panic(fmt.Errorf("unknown architecture %s", architecture))
	}
}

func getMachineType(architecture string) string {
	switch architecture {
	case architectureArm64:
		// virt is the most up to date and generic ARM machine architecture
		return "virt"
	case architectureAmd64:
		// q35 is the most up to date and generic x86_64 machine architecture
		return "q35"
	default:
		panic(fmt.Errorf("unknown architecture %s", architecture))
	}
}

func handleCPUChange(
	logger *zap.Logger,
	w http.ResponseWriter,
	r *http.Request,
	set func(*zap.Logger, vmv1.MilliCPU) error,
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
	err = set(logger, parsed.VCPUs)
	if err != nil {
		logger.Error("could not set cgroup limit", zap.Error(err))
		w.WriteHeader(500)
		return
	}

	w.WriteHeader(200)
}

func handleCPUCurrent(
	logger *zap.Logger,
	w http.ResponseWriter,
	r *http.Request,
	get func(*zap.Logger) (*vmv1.MilliCPU, error),
) {
	if r.Method != "GET" {
		logger.Error("unexpected method", zap.String("method", r.Method))
		w.WriteHeader(400)
		return
	}

	cpus, err := get(logger)
	if err != nil {
		logger.Error("could not get cgroup quota", zap.Error(err))
		w.WriteHeader(500)
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

type cpuServerCallbacks struct {
	get func(*zap.Logger) (*vmv1.MilliCPU, error)
	set func(*zap.Logger, vmv1.MilliCPU) error
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

func printWithNewline(slice []byte) error {
	if len(slice) == 0 {
		return nil
	}

	_, err := os.Stdout.Write(slice)
	if err != nil {
		return err
	}

	if slice[len(slice)-1] == '\n' {
		return nil
	}

	_, err = os.Stdout.WriteString("\n")
	return err
}

func drainLogsReader(reader *bufio.Reader, logger *zap.Logger) error {
	for {
		// ReadSlice actually can return no more than bufferedReaderSize bytes
		slice, err := reader.ReadSlice('\n')
		// If err != nil, slice might not have \n at the end
		err2 := printWithNewline(slice)

		err = errors.Join(err, err2)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil
			}
			if errors.Is(err, io.EOF) {
				logger.Warn("EOF while reading from log serial")
			} else {
				logger.Error("failed to read from log serial", zap.Error(err))
			}
			return err
		}
	}
}

// forwardLogs writes from socket to stdout line by line
func forwardLogs(ctx context.Context, logger *zap.Logger, wg *sync.WaitGroup) {
	defer wg.Done()

	delay := 3 * time.Second
	var conn net.Conn
	var reader *bufio.Reader

	b := &backoff.Backoff{
		Min:    100 * time.Millisecond,
		Max:    delay,
		Factor: 2,
		Jitter: true,
	}

	// Wait a bit to reduce the chance we attempt dialing before
	// QEMU is started
	select {
	case <-time.After(200 * time.Millisecond):
	case <-ctx.Done():
		logger.Warn("QEMU shut down too soon to start forwarding logs")
	}

	for {
		func() {
			if conn == nil {
				var err error
				conn, err = net.Dial("unix", logSerialSocket)
				if err != nil {
					logger.Error("failed to dial to logSerialSocket", zap.Error(err))
					return
				}
				reader = bufio.NewReaderSize(conn, bufferedReaderSize)
			}

			b.Attempt()
			err := conn.SetReadDeadline(time.Now().Add(delay))
			if err != nil {
				logger.Error("failed to set read deadline", zap.Error(err))
				conn = nil
				return
			}

			err = drainLogsReader(reader, logger)
			if errors.Is(err, os.ErrDeadlineExceeded) {
				// We've hit the deadline, meaning the reading session was successful.
				b.Reset()
				return
			}

			if err != nil {
				conn = nil
			}
		}()

		select {
		case <-ctx.Done():
			if conn != nil {
				conn.Close()
			}
			if reader != nil {
				_ = drainLogsReader(reader, logger)
			}
			return
		case <-time.After(b.Duration()):
		}
	}
}

func getSelfCgroupPath(logger *zap.Logger) (string, error) {
	// There's some fun stuff here. For general information, refer to `man 7 cgroups` - specifically
	// the section titled "/proc files" - for "/proc/cgroups" and "/proc/pid/cgroup".
	//
	// In general, the idea is this: If we start QEMU outside of the cgroup for the container we're
	// running in, we run into multiple problems - it won't show up in metrics, and we'll have to
	// clean up the cgroup ourselves. (not good!).
	//
	// So we'd like to start it in the same cgroup - the question is just how to find the name of
	// the cgroup we're running in. Thankfully, this is visible in `/proc/self/cgroup`!
	// The only difficulty is the file format.
	//
	// In cgroup v1 (which is what we have on EKS [as of 2023-07]), the contents of
	// /proc/<pid>/cgroup tend to look like:
	//
	//   11:cpuset:/path/to/cgroup
	//   10:perf_event:/path/to/cgroup
	//   9:hugetlb:/path/to/cgroup
	//   8:blkio:/path/to/cgroup
	//   7:pids:/path/to/cgroup
	//   6:freezer:/path/to/cgroup
	//   5:memory:/path/to/cgroup
	//   4:net_cls,net_prio:/path/to/cgroup
	//   3:cpu,cpuacct:/path/to/cgroup
	//   2:devices:/path/to/cgroup
	//   1:name=systemd:/path/to/cgroup
	//
	// For cgroup v2, we have:
	//
	//   0::/path/to/cgroup
	//
	// The file format is defined to have 3 fields, separated by colons. The first field gives the
	// Hierarchy ID, which is guaranteed to be 0 if the cgroup is part of a cgroup v2 ("unified")
	// hierarchy.
	// The second field is a comma-separated list of the controllers. Or, if it's cgroup v2, nothing.
	// The third field is the "pathname" of the cgroup *in its hierarchy*, relative to the mount
	// point of the hierarchy.
	//
	// So we're looking for EITHER:
	//  1. an entry like '<N>:<controller...>,cpu,<controller...>:/path/to/cgroup (cgroup v1); OR
	//  2. an entry like '0::/path/to/cgroup', and we'll return the path (cgroup v2)
	// We primarily care about the 'cpu' controller, so for cgroup v1, we'll search for that instead
	// of e.g. "name=systemd", although it *really* shouldn't matter because the paths will be the
	// same anyways.
	//
	// Now: Technically it's possible to run a "hybrid" system with both cgroup v1 and v2
	// hierarchies. If this is the case, it's possible for /proc/self/cgroup to show *some* v1
	// hierarchies attached, in addition to the v2 "unified" hierarchy, for the same cgroup. To
	// handle this, we should look for a cgroup v1 "cpu" controller, and if we can't find it, try
	// for the cgroup v2 unified entry.
	//
	// As far as I (@sharnoff) can tell, the only case where that might actually get messed up is if
	// the CPU controller isn't available for the cgroup we're running in, in which case there's
	// nothing we can do about it! (other than e.g. using a cgroup higher up the chain, which would
	// be really bad tbh).

	// ---
	// On to the show!

	procSelfCgroupContents, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("failed to read /proc/self/cgroup: %w", err)
	}
	logger.Info("Read /proc/self/cgroup", zap.String("contents", string(procSelfCgroupContents)))

	// Collect all candidate paths from the lines of the file. If there isn't exactly one,
	// something's wrong and we should make an error.
	var v1Candidates []string
	var v2Candidates []string
	for lineno, line := range strings.Split(string(procSelfCgroupContents), "\n") {
		if line == "" {
			continue
		}

		// Split into the three ':'-delimited fields
		fields := strings.Split(line, ":")
		if len(fields) != 3 {
			return "", fmt.Errorf("line %d of /proc/self/cgroup did not have 3 colon-delimited fields", lineno+1)
		}

		id := fields[0]
		controllers := fields[1]
		path := fields[2]
		if id == "0" {
			v2Candidates = append(v2Candidates, path)
			continue
		}

		// It's not cgroup v2, otherwise id would have been 0. So, check if the comma-separated list
		// of controllers contains 'cpu' as an entry.
		for _, c := range strings.Split(controllers, ",") {
			if c == "cpu" {
				v1Candidates = append(v1Candidates, path)
				break // ... and then continue to the next loop iteration
			}
		}
	}

	var errMsg string

	// Check v1, then v2
	if len(v1Candidates) == 1 {
		return v1Candidates[0], nil
	} else if len(v1Candidates) != 0 {
		errMsg = "More than one applicable cgroup v1 entry in /proc/self/cgroup"
	} else if len(v2Candidates) == 1 {
		return v2Candidates[0], nil
	} else if len(v2Candidates) != 0 {
		errMsg = "More than one applicable cgroup v2 entry in /proc/self/cgroup"
	} else {
		errMsg = "Couldn't find applicable entry in /proc/self/cgroup"
	}

	return "", errors.New(errMsg)
}

func setCgroupLimit(logger *zap.Logger, r vmv1.MilliCPU, cgroupPath string) error {
	r *= cpuLimitOvercommitFactor

	isV2 := cgroups.Mode() == cgroups.Unified
	period := cgroupPeriod
	// quota may be greater than period if the cgroup is allowed
	// to use more than 100% of a CPU.
	quota := int64(float64(r) / float64(1000) * float64(cgroupPeriod))
	logger.Info(fmt.Sprintf("setting cgroup CPU limit %v %v", quota, period))
	if isV2 {
		resources := cgroup2.Resources{
			CPU: &cgroup2.CPU{
				Max: cgroup2.NewCPUMax(&quota, &period),
			},
		}
		_, err := cgroup2.NewManager(cgroupMountPoint, cgroupPath, &resources)
		if err != nil {
			return err
		}
	} else {
		_, err := cgroup1.New(cgroup1.StaticPath(cgroupPath), &specs.LinuxResources{
			CPU: &specs.LinuxCPU{
				Quota:  &quota,
				Period: &period,
			},
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func terminateQemuOnSigterm(ctx context.Context, logger *zap.Logger, wg *sync.WaitGroup) {
	logger = logger.Named("terminate-qemu-on-sigterm")

	defer wg.Done()
	logger.Info("watching OS signals")
	c := make(chan os.Signal, 1) // we need to reserve to buffer size 1, so the notifier are not blocked
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	select {
	case <-c:
	case <-ctx.Done():
		logger.Info("context canceled, not going to powerdown QEMU because it's already finished")
		return
	}

	logger.Info("got signal, sending powerdown command to QEMU")

	mon, err := qmp.NewSocketMonitor("unix", qmpUnixSocketForSigtermHandler, 2*time.Second)
	if err != nil {
		logger.Error("failed to connect to QEMU monitor", zap.Error(err))
		return
	}

	if err := mon.Connect(); err != nil {
		logger.Error("failed to start monitor connection", zap.Error(err))
		return
	}
	defer mon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	qmpcmd := []byte(`{"execute": "system_powerdown"}`)
	_, err = mon.Run(qmpcmd)
	if err != nil {
		logger.Error("failed to execute system_powerdown command", zap.Error(err))
		return
	}

	logger.Info("system_powerdown command sent to QEMU")
}

//lint:ignore U1000 the function is not in use right now, but it's good to have for the future
func execBg(name string, arg ...string) error {
	cmd := exec.Command(name, arg...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	return nil
}

func execFg(name string, arg ...string) error {
	cmd := exec.Command(name, arg...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func setNeonvmDaemonCPU(cpu vmv1.MilliCPU) error {
	_, vmIP, _, err := calcIPs(defaultNetworkCIDR)
	if err != nil {
		return fmt.Errorf("could not calculate VM IP address: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.TODO(), time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:25183/cpu", vmIP)
	body := bytes.NewReader([]byte(fmt.Sprintf("%d", uint32(cpu))))

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("could not build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("neonvm-daemon responded with status %d", resp.StatusCode)
	}

	return nil
}
