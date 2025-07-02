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
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
	"github.com/jpillora/backoff"
	"github.com/samber/lo"
	"go.uber.org/zap"

	"k8s.io/apimachinery/pkg/api/resource"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/gzip64"
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
	// autoMovableRatio value for VirtioMem provider. Validated in newConfig.
	autoMovableRatio string
	// cpuScalingMode is a mode to use for CPU scaling. Validated in newConfig.
	cpuScalingMode vmv1.CpuScalingMode
	// System CPU architecture. Set automatically equal to runtime.GOARCH.
	architecture string
	// useVirtioConsole is a flag to use virtio console instead of serial console.
	useVirtioConsole bool
}

func newConfig(logger *zap.Logger) *Config {
	cfg := &Config{
		vmSpecDump:           "",
		vmStatusDump:         "",
		kernelPath:           defaultKernelPath,
		appendKernelCmdline:  "",
		skipCgroupManagement: false,
		diskCacheSettings:    "cache=none",
		autoMovableRatio:     "",
		cpuScalingMode:       "",
		architecture:         runtime.GOARCH,
		useVirtioConsole:     false,
	}
	flag.StringVar(&cfg.vmSpecDump, "vmspec", cfg.vmSpecDump,
		"Base64 gzip compressed VirtualMachine json specification")
	flag.StringVar(&cfg.vmStatusDump, "vmstatus", cfg.vmStatusDump,
		"Base64 gzip compressed VirtualMachine json status")
	flag.StringVar(&cfg.kernelPath, "kernelpath", cfg.kernelPath,
		"Override path for kernel to use")
	flag.StringVar(&cfg.appendKernelCmdline, "appendKernelCmdline",
		cfg.appendKernelCmdline, "Additional kernel command line arguments")
	flag.BoolVar(&cfg.skipCgroupManagement, "skip-cgroup-management",
		cfg.skipCgroupManagement,
		"Don't try to manage CPU")
	flag.StringVar(&cfg.diskCacheSettings, "qemu-disk-cache-settings",
		cfg.diskCacheSettings, "Cache settings to add to -drive args for VM disks")
	flag.StringVar(&cfg.autoMovableRatio, "memhp-auto-movable-ratio",
		cfg.autoMovableRatio, "Set value of kernel's memory_hotplug.auto_movable_ratio [virtio-mem only]")
	flag.Func("cpu-scaling-mode", "Set CPU scaling mode", cfg.cpuScalingMode.FlagFunc)
	flag.BoolVar(&cfg.useVirtioConsole, "use-virtio-console",
		cfg.useVirtioConsole, "Use virtio console instead of serial console")
	flag.Parse()

	if cfg.autoMovableRatio == "" {
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

	vmSpecJson, err := gzip64.Decode(cfg.vmSpecDump)
	if err != nil {
		return fmt.Errorf("failed to decode VirtualMachine Spec dump: %w", err)
	}
	vmStatusJson, err := gzip64.Decode(cfg.vmStatusDump)
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

	// Run init script before doing anything else. It can create
	// iptables rules, which we don't want to interleave with rules
	// created by code in net.go
	if err := runInitScript(logger, vmSpec.InitScript); err != nil {
		return fmt.Errorf("failed to run init script: %w", err)
	}

	tg := taskgroup.NewGroup(logger)

	// create iso9660 disk with runtime options (command, args, envs, mounts)
	tg.Go("iso9660-runtime", func(logger *zap.Logger) error {
		disks := vmSpec.Disks

		// add the tls path.
		// this is needed to just `mkdir` the mounting directory.
		if vmSpec.TLS != nil {
			disks = append(disks, vmv1.Disk{
				Name:      "tls-keys",
				MountPath: vmSpec.TLS.MountPath,
				Watch:     lo.ToPtr(true),
				ReadOnly:  nil,
				DiskSource: vmv1.DiskSource{
					EmptyDisk: nil,
					ConfigMap: nil,
					Secret:    nil,
					Tmpfs:     nil,
				},
			})
		}

		return createISO9660runtime(
			runtimeDiskPath,
			vmSpec.Guest.Command,
			vmSpec.Guest.Args,
			sysctl,
			vmSpec.Guest.Env,
			disks,
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
		qemuCmd = append(qemuCmd, "-bios", "/vm/firmware/QEMU_EFI.fd")
		// arm virt has only one UART, setup virtio-serial to add more /dev/hvcX
		qemuCmd = append(qemuCmd,
			"-chardev", "stdio,id=virtio-console",
			"-device", "virtconsole,chardev=virtio-console",
		)
	case architectureAmd64:
		// UART port is used by default but it has performance issues.
		// Virtio console is more performant.
		if cfg.useVirtioConsole {
			qemuCmd = append(qemuCmd,
				"-chardev", "stdio,id=virtio-console",
				"-device", "virtconsole,chardev=virtio-console",
			)
		} else {
			// on amd we have multiple UART ports so we can just use serial stdio
			qemuCmd = append(qemuCmd, "-serial", "stdio")
		}
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
		panic(fmt.Errorf("unknown CPU scaling mode %q", cfg.cpuScalingMode))
	}

	// memory details
	qemuCmd = append(qemuCmd, "-m", fmt.Sprintf(
		"size=%db,slots=%d,maxmem=%db",
		vmSpec.Guest.MemorySlotSize.Value()*int64(vmSpec.Guest.MemorySlots.Min),
		vmSpec.Guest.MemorySlots.Max-vmSpec.Guest.MemorySlots.Min,
		vmSpec.Guest.MemorySlotSize.Value()*int64(vmSpec.Guest.MemorySlots.Max),
	))
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
	// this loglevel is used only during startup, later it is overriden during vminit
	baseKernelCmdline          = "panic=-1 init=/neonvm/bin/init loglevel=6 root=/dev/vda rw"
	kernelCmdlineVirtioMemTmpl = "memhp_default_state=online memory_hotplug.online_policy=auto-movable memory_hotplug.auto_movable_ratio=%s"
)

func makeKernelCmdline(cfg *Config, logger *zap.Logger, vmSpec *vmv1.VirtualMachineSpec, vmStatus *vmv1.VirtualMachineStatus, hostname string) string {
	cmdlineParts := []string{baseKernelCmdline}

	cmdlineParts = append(cmdlineParts, fmt.Sprintf(kernelCmdlineVirtioMemTmpl, cfg.autoMovableRatio))

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
		// use virtio-serial device if virtio console is enabled
		if cfg.useVirtioConsole {
			cmdlineParts = append(cmdlineParts, "console=hvc0")
		} else {
			cmdlineParts = append(cmdlineParts, "console=ttyS1")
		}
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
		var err error
		cgroupPath, err = setupQEMUCgroup(logger, selfPodName, vmSpec.Guest.CPUs.Use)
		if err != nil {
			return err
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
		get: func(logger *zap.Logger) (*vmv1.MilliCPU, int, error) {
			switch cfg.cpuScalingMode {
			case vmv1.CpuScalingModeSysfs:
				cpu, status, err := getNeonvmDaemonCPU()
				if err != nil {
					logger.Error("failed to get CPU from NeonVM Daemon", zap.Error(err))
					return nil, status, err
				}
				storedCpu := vmv1.MilliCPU(lastValue.Load())
				if storedCpu.RoundedUp() != cpu.RoundedUp() {
					logger.Warn("CPU from NeonVM Daemon does not match stored value, returning daemon value to let controller reconcile correct state", zap.Any("stored", storedCpu), zap.Any("current", cpu))
					return &cpu, 0, nil
				}
				return &storedCpu, 0, nil

			case vmv1.CpuScalingModeQMP:
				return lo.ToPtr(vmv1.MilliCPU(lastValue.Load())), 0, nil
			default:
				panic(fmt.Errorf("unknown CPU scaling mode %q", cfg.cpuScalingMode))
			}
		},
		set: func(logger *zap.Logger, cpu vmv1.MilliCPU) (int, error) {
			switch cfg.cpuScalingMode {
			case vmv1.CpuScalingModeSysfs:
				status, err := setNeonvmDaemonCPU(cpu)
				if err != nil {
					logger.Error("setting CPU through NeonVM Daemon failed", zap.Any("cpu", cpu), zap.Error(err))
					return status, err
				}
				lastValue.Store(uint32(cpu))
			case vmv1.CpuScalingModeQMP:
				lastValue.Store(uint32(cpu))
				return 0, nil
			default:
				panic(fmt.Errorf("unknown CPU scaling mode %q", cfg.cpuScalingMode))
			}
			return 0, nil
		},
		ready: func(logger *zap.Logger) bool {
			switch cfg.cpuScalingMode {
			case vmv1.CpuScalingModeSysfs:
				// check if the NeonVM Daemon is ready to accept requests
				_, _, err := getNeonvmDaemonCPU()
				if err != nil {
					logger.Warn("neonvm-daemon ready probe failed", zap.Error(err))
					return false
				}
				return true
			case vmv1.CpuScalingModeQMP:
				// no readiness check for QMP mode
				return true
			default:
				// explicit panic for unknown CPU scaling mode
				// in case if we add a new CPU scaling mode and forget to update this function
				panic(fmt.Errorf("unknown CPU scaling mode %q", cfg.cpuScalingMode))
			}
		},
	}

	wg.Add(1)
	monitoring := vmSpec.EnableNetworkMonitoring != nil && *vmSpec.EnableNetworkMonitoring
	go listenForHTTPRequests(ctx, logger, vmSpec.RunnerPort, callbacks, &wg, monitoring)
	wg.Add(1)
	go forwardLogs(ctx, logger, &wg)
	wg.Add(1)
	go monitorFiles(ctx, logger, &wg, vmSpec)

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
		panic(fmt.Errorf("unknown architecture %q", architecture))
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
		panic(fmt.Errorf("unknown architecture %q", architecture))
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

// monitorFiles watches a specific set of files and copied them into the guest VM via neonvm-daemon.
func monitorFiles(ctx context.Context, logger *zap.Logger, wg *sync.WaitGroup, vmSpec *vmv1.VirtualMachineSpec) {
	defer wg.Done()

	secrets := make(map[string]string)
	secretsOrd := []string{}
	for _, disk := range vmSpec.Disks {
		if disk.Watch != nil && *disk.Watch {
			// secrets/configmaps are mounted using the atomicwriter utility,
			// which loads the directory into `..data`.
			dataDir := fmt.Sprintf("/vm/mounts%s/..data", disk.MountPath)
			secrets[dataDir] = disk.MountPath
			secretsOrd = append(secretsOrd, dataDir)
		}
	}

	if vmSpec.TLS != nil {
		dataDir := fmt.Sprintf("/vm/mounts%s/..data", vmSpec.TLS.MountPath)
		secrets[dataDir] = vmSpec.TLS.MountPath
		secretsOrd = append(secretsOrd, dataDir)
	}

	if len(secretsOrd) == 0 {
		return
	}

	// Faster loop for the initial upload.
	// The VM might need the secrets in order for postgres to actually start up,
	// so it's important we sync them as soon as the daemon is available.
	for {
		success := true
		for _, hostpath := range secretsOrd {
			guestpath := secrets[hostpath]
			if err := sendFilesToNeonvmDaemon(ctx, hostpath, guestpath); err != nil {
				success = false
				logger.Error("failed to upload file to vm guest", zap.Error(err))
			}
		}
		if success {
			break
		}

		select {
		case <-time.After(1 * time.Second):
			continue
		case <-ctx.Done():
			return
		}
	}

	// For the entire duration the VM is alive, periodically check whether any of the watched disks
	// still match what's inside the VM, and if not, send the update.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// for each secret we are tracking
			for hostpath, guestpath := range secrets {
				// get the checksum for the pod directory
				hostsum, err := util.ChecksumFlatDir(hostpath)
				if err != nil {
					logger.Error("failed to get dir checksum from host", zap.Error(err), zap.String("dir", hostpath))
					continue
				}

				// get the checksum for the VM directory
				guestsum, err := getFileChecksumFromNeonvmDaemon(ctx, guestpath)
				if err != nil {
					logger.Error("failed to get dir checksum from guest", zap.Error(err), zap.String("dir", guestpath))
					continue
				}

				// if not equal, update the files inside the VM.
				if guestsum != hostsum {
					if err = sendFilesToNeonvmDaemon(ctx, hostpath, guestpath); err != nil {
						logger.Error("failed to upload files to vm guest", zap.Error(err))
					}
				}
			}
		}
	}
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

func httpStatusForURLError(err error) int {
	// From RFC 9110:
	//
	// > The 502 (Bad Gateway) status code indicates that the server, while acting as a gateway or
	// > proxy, received an invalid response from an inbound server it accessed while attempting to
	// > fulfill the request.
	//
	// and
	//
	// > The 504 (Gateway Timeout) status code indicates that the server, while acting as a gateway
	// > or proxy, did not receive a timely response from an upstream server it needed to access in
	// > order to complete the request.

	urlErr := new(neturl.Error)
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return http.StatusGatewayTimeout
	} else {
		return http.StatusBadGateway
	}
}

func setNeonvmDaemonCPU(cpu vmv1.MilliCPU) (status int, _ error) {
	_, vmIP, _, err := calcIPs(defaultNetworkCIDR)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("could not calculate VM IP address: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.TODO(), time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:25183/cpu", vmIP)
	body := bytes.NewReader([]byte(fmt.Sprintf("%d", uint32(cpu))))

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("could not build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		status := httpStatusForURLError(err)
		return status, fmt.Errorf("could not send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return http.StatusBadGateway, fmt.Errorf("neonvm-daemon responded with status %d", resp.StatusCode)
	}

	return http.StatusOK, nil
}

func getNeonvmDaemonCPU() (_ vmv1.MilliCPU, status int, _ error) {
	_, vmIP, _, err := calcIPs(defaultNetworkCIDR)
	if err != nil {
		return 0, http.StatusInternalServerError, fmt.Errorf("could not calculate VM IP address: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.TODO(), time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:25183/cpu", vmIP)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, http.StatusInternalServerError, fmt.Errorf("could not build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		status := httpStatusForURLError(err)
		return 0, status, fmt.Errorf("could not send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, http.StatusBadGateway, fmt.Errorf("neonvm-daemon responded with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, http.StatusBadGateway, fmt.Errorf("could not read response: %w", err)
	}

	value, err := strconv.ParseUint(string(body), 10, 32)
	if err != nil {
		return 0, http.StatusBadGateway, fmt.Errorf("could not parse response: %w", err)
	}

	return vmv1.MilliCPU(value), http.StatusOK, nil
}

type File struct {
	// base64 encoded file contents
	Data string `json:"data"`
}

func sendFilesToNeonvmDaemon(ctx context.Context, hostpath, guestpath string) error {
	_, vmIP, _, err := calcIPs(defaultNetworkCIDR)
	if err != nil {
		return fmt.Errorf("could not calculate VM IP address: %w", err)
	}

	files, err := util.ReadAllFiles(hostpath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not open file: %w", err)
	}

	encodedFiles := make(map[string]File)
	for k, v := range files {
		encodedFiles[k] = File{Data: base64.StdEncoding.EncodeToString(v)}
	}
	body, err := json.Marshal(encodedFiles)
	if err != nil {
		return fmt.Errorf("could not encode files: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	// guestpath has a leading forward slash
	url := fmt.Sprintf("http://%s:25183/files%s", vmIP, guestpath)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("could not build request: %w", err)
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("neonvm-daemon responded with status %d", resp.StatusCode)
	}

	return nil
}

func getFileChecksumFromNeonvmDaemon(ctx context.Context, guestpath string) (string, error) {
	_, vmIP, _, err := calcIPs(defaultNetworkCIDR)
	if err != nil {
		return "", fmt.Errorf("could not calculate VM IP address: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	// guestpath has a leading forward slash
	url := fmt.Sprintf("http://%s:25183/files%s", vmIP, guestpath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("could not build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("neonvm-daemon responded with status %d", resp.StatusCode)
	}

	checksum, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("could not read response: %w", err)
	}

	return string(checksum), nil
}
