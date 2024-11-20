package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alessio/shellescape"
	"github.com/cilium/cilium/pkg/mac"
	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/coreos/go-iptables/iptables"
	"github.com/digitalocean/go-qemu/qmp"
	"github.com/docker/libnetwork/types"
	"github.com/jpillora/backoff"
	"github.com/kdomanski/iso9660"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/samber/lo"
	"github.com/vishvananda/netlink"
	"go.uber.org/zap"

	"k8s.io/apimachinery/pkg/api/resource"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/taskgroup"
)

const (
	qemuBinArm64 = "qemu-system-aarch64"
	qemuBinX8664 = "qemu-system-x86_64"
	qemuImgBin   = "qemu-img"

	architectureArm64 = "arm64"
	architectureAmd64 = "amd64"
	defaultKernelPath = "/vm/kernel/vmlinuz"

	rootDiskPath                   = "/vm/images/rootdisk.qcow2"
	runtimeDiskPath                = "/vm/images/runtime.iso"
	mountedDiskPath                = "/vm/images"
	qmpUnixSocketForSigtermHandler = "/vm/qmp-sigterm.sock"
	logSerialSocket                = "/vm/log.sock"
	bufferedReaderSize             = 4096

	sshAuthorizedKeysDiskPath   = "/vm/images/ssh-authorized-keys.iso"
	sshAuthorizedKeysMountPoint = "/vm/ssh"

	swapName = "swapdisk"

	defaultNetworkBridgeName = "br-def"
	defaultNetworkTapName    = "tap-def"
	defaultNetworkCIDR       = "169.254.254.252/30"

	overlayNetworkBridgeName = "br-overlay"
	overlayNetworkTapName    = "tap-overlay"

	// defaultPath is the default path to the resolv.conf that contains information to resolve DNS. See Path().
	resolveDefaultPath = "/etc/resolv.conf"
	// alternatePath is a path different from defaultPath, that may be used to resolve DNS. See Path().
	resolveAlternatePath = "/run/systemd/resolve/resolv.conf"

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

	protocolTCP string = "6"
)

var (
	ipv4NumBlock      = `(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)`
	ipv4Address       = `(` + ipv4NumBlock + `\.){3}` + ipv4NumBlock
	ipv6Address       = `([0-9A-Fa-f]{0,4}:){2,7}([0-9A-Fa-f]{0,4})(%\w+)?`
	nsRegexp          = regexp.MustCompile(`^\s*nameserver\s*((` + ipv4Address + `)|(` + ipv6Address + `))\s*$`)
	nsIPv4Regexpmatch = regexp.MustCompile(`^\s*nameserver\s*((` + ipv4Address + `))\s*$`)
	nsIPv6Regexpmatch = regexp.MustCompile(`^\s*nameserver\s*((` + ipv6Address + `))\s*$`)
	searchRegexp      = regexp.MustCompile(`^\s*search\s*(([^\s]+\s*)*)$`)

	detectSystemdResolvConfOnce sync.Once
	pathAfterSystemdDetection   = resolveDefaultPath
)

// File contains the resolv.conf content and its hash
type resolveFile struct {
	Content []byte
	Hash    string
}

// Get returns the contents of /etc/resolv.conf and its hash
func getResolvConf() (*resolveFile, error) {
	return getSpecific(resolvePath())
}

// hashData returns the sha256 sum of src.
// from https://github.com/moby/moby/blob/v20.10.24/pkg/ioutils/readers.go#L52-L59
func hashData(src io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, src); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// GetSpecific returns the contents of the user specified resolv.conf file and its hash
func getSpecific(path string) (*resolveFile, error) {
	resolv, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	hash, err := hashData(bytes.NewReader(resolv))
	if err != nil {
		return nil, err
	}
	return &resolveFile{Content: resolv, Hash: hash}, nil
}

// GetNameservers returns nameservers (if any) listed in /etc/resolv.conf
func getNameservers(resolvConf []byte, kind int) []string {
	nameservers := []string{}
	for _, line := range getLines(resolvConf, []byte("#")) {
		var ns [][]byte
		if kind == types.IP {
			ns = nsRegexp.FindSubmatch(line)
		} else if kind == types.IPv4 {
			ns = nsIPv4Regexpmatch.FindSubmatch(line)
		} else if kind == types.IPv6 {
			ns = nsIPv6Regexpmatch.FindSubmatch(line)
		}
		if len(ns) > 0 {
			nameservers = append(nameservers, string(ns[1]))
		}
	}
	return nameservers
}

// GetSearchDomains returns search domains (if any) listed in /etc/resolv.conf
// If more than one search line is encountered, only the contents of the last
// one is returned.
func getSearchDomains(resolvConf []byte) []string {
	domains := []string{}
	for _, line := range getLines(resolvConf, []byte("#")) {
		match := searchRegexp.FindSubmatch(line)
		if match == nil {
			continue
		}
		domains = strings.Fields(string(match[1]))
	}
	return domains
}

// getLines parses input into lines and strips away comments.
func getLines(input []byte, commentMarker []byte) [][]byte {
	lines := bytes.Split(input, []byte("\n"))
	var output [][]byte
	for _, currentLine := range lines {
		commentIndex := bytes.Index(currentLine, commentMarker)
		if commentIndex == -1 {
			output = append(output, currentLine)
		} else {
			output = append(output, currentLine[:commentIndex])
		}
	}
	return output
}

func resolvePath() string {
	detectSystemdResolvConfOnce.Do(func() {
		candidateResolvConf, err := os.ReadFile(resolveDefaultPath)
		if err != nil {
			// silencing error as it will resurface at next calls trying to read defaultPath
			return
		}
		ns := getNameservers(candidateResolvConf, types.IP)
		if len(ns) == 1 && ns[0] == "127.0.0.53" {
			pathAfterSystemdDetection = resolveAlternatePath
		}
	})
	return pathAfterSystemdDetection
}

func createISO9660runtime(
	diskPath string,
	command []string,
	args []string,
	sysctl []string,
	env []vmv1.EnvVar,
	disks []vmv1.Disk,
	enableSSH bool,
	swapSize *resource.Quantity,
	shmsize *resource.Quantity,
) error {
	writer, err := iso9660.NewWriter()
	if err != nil {
		return err
	}
	defer writer.Cleanup() //nolint:errcheck // Nothing to do with the error, maybe log it ? TODO

	if len(sysctl) != 0 {
		err = writer.AddFile(bytes.NewReader([]byte(strings.Join(sysctl, "\n"))), "sysctl.conf")
		if err != nil {
			return err
		}
	}

	if len(command) != 0 {
		err = writer.AddFile(bytes.NewReader([]byte(shellescape.QuoteCommand(command))), "command.sh")
		if err != nil {
			return err
		}
	}

	if len(args) != 0 {
		err = writer.AddFile(bytes.NewReader([]byte(shellescape.QuoteCommand(args))), "args.sh")
		if err != nil {
			return err
		}
	}

	if len(env) != 0 {
		envstring := []string{}
		for _, e := range env {
			envstring = append(envstring, fmt.Sprintf(`export %s=%s`, e.Name, shellescape.Quote(e.Value)))
		}
		envstring = append(envstring, "")
		err = writer.AddFile(bytes.NewReader([]byte(strings.Join(envstring, "\n"))), "env.sh")
		if err != nil {
			return err
		}
	}

	mounts := []string{
		"set -euxo pipefail",
	}
	if enableSSH {
		mounts = append(mounts, "/neonvm/bin/mkdir -p /mnt/ssh")
		mounts = append(mounts, "/neonvm/bin/mount  -t iso9660 -o ro,mode=0644 $(/neonvm/bin/blkid -L ssh-authorized-keys) /mnt/ssh")
	}

	if swapSize != nil {
		mounts = append(mounts, fmt.Sprintf("/neonvm/bin/sh /neonvm/runtime/resize-swap-internal.sh %d", swapSize.Value()))
	}

	if len(disks) != 0 {
		for _, disk := range disks {
			if disk.MountPath != "" {
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mkdir -p %s`, disk.MountPath))
			}
			switch {
			case disk.EmptyDisk != nil:
				opts := ""
				if disk.EmptyDisk.Discard {
					opts = "-o discard"
				}

				if disk.EmptyDisk.EnableQuotas {
					mounts = append(mounts, fmt.Sprintf(`tune2fs -Q prjquota $(/neonvm/bin/blkid -L %s)`, disk.Name))
					mounts = append(mounts, fmt.Sprintf(`tune2fs -E mount_opts=prjquota $(/neonvm/bin/blkid -L %s)`, disk.Name))
				}

				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mount %s $(/neonvm/bin/blkid -L %s) %s`, opts, disk.Name, disk.MountPath))
				// Note: chmod must be after mount, otherwise it gets overwritten by mount.
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/chmod 0777 %s`, disk.MountPath))
			case disk.ConfigMap != nil || disk.Secret != nil:
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mount -t iso9660 -o ro,mode=0644 $(/neonvm/bin/blkid -L %s) %s`, disk.Name, disk.MountPath))
			case disk.Tmpfs != nil:
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/chmod 0777 %s`, disk.MountPath))
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mount -t tmpfs -o size=%d %s %s`, disk.Tmpfs.Size.Value(), disk.Name, disk.MountPath))
			default:
				// do nothing
			}
		}
	}

	if shmsize != nil {
		mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mount -o remount,size=%d /dev/shm`, shmsize.Value()))
	}

	mounts = append(mounts, "")
	err = writer.AddFile(bytes.NewReader([]byte(strings.Join(mounts, "\n"))), "mounts.sh")
	if err != nil {
		return err
	}

	if swapSize != nil {
		lines := []string{
			`#!/neonvm/bin/sh`,
			`set -euxo pipefail`,
			// this script may be run as root, so we should avoid potentially-malicious path
			// injection
			`export PATH="/neonvm/bin"`,
			fmt.Sprintf(`swapdisk="$(/neonvm/bin/blkid -L %s)"`, swapName),
			// disable swap. Allow it to fail if it's already disabled.
			`swapoff "$swapdisk" || true`,
			// if the requested size is zero, then... just exit. There's nothing we need to do.
			`new_size="$1"`,
			`if [ "$new_size" = '0' ]; then exit 0; fi`,
			// re-make the swap.
			// mkswap expects the size to be given in KiB, so divide the new size by 1K
			fmt.Sprintf(`mkswap -L %s "$swapdisk" $(( new_size / 1024 ))`, swapName),
			// ... and then re-enable the swap
			//
			// nb: busybox swapon only supports '-d', not its long form '--discard'.
			`swapon -d "$swapdisk"`,
		}
		err = writer.AddFile(bytes.NewReader([]byte(strings.Join(lines, "\n"))), "resize-swap-internal.sh")
		if err != nil {
			return err
		}
	}

	outputFile, err := os.OpenFile(diskPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}

	// uid=36(qemu) gid=34(kvm) groups=34(kvm)
	err = outputFile.Chown(36, 34)
	if err != nil {
		return err
	}

	err = writer.WriteTo(outputFile, "vmruntime")
	if err != nil {
		return err
	}

	err = outputFile.Close()
	if err != nil {
		return err
	}

	return nil
}

func calcDirUsage(dirPath string) (int64, error) {
	stat, err := os.Lstat(dirPath)
	if err != nil {
		return 0, err
	}

	size := stat.Size()

	if !stat.IsDir() {
		return size, nil
	}

	dir, err := os.Open(dirPath)
	if err != nil {
		return size, err
	}
	defer dir.Close()

	files, err := dir.Readdir(-1)
	if err != nil {
		return size, err
	}

	for _, file := range files {
		if file.Name() == "." || file.Name() == ".." {
			continue
		}
		s, err := calcDirUsage(dirPath + "/" + file.Name())
		if err != nil {
			return size, err
		}
		size += s
	}
	return size, nil
}

func createSwap(diskPath string, swapSize *resource.Quantity) error {
	tmpRawFile := "swap.raw"

	if err := execFg(qemuImgBin, "create", "-q", "-f", "raw", tmpRawFile, fmt.Sprintf("%d", swapSize.Value())); err != nil {
		return err
	}
	if err := execFg("mkswap", "-L", swapName, tmpRawFile); err != nil {
		return err
	}

	if err := execFg(qemuImgBin, "convert", "-q", "-f", "raw", "-O", "qcow2", "-o", "cluster_size=2M,lazy_refcounts=on", tmpRawFile, diskPath); err != nil {
		return err
	}

	if err := execFg("rm", "-f", tmpRawFile); err != nil {
		return err
	}

	// uid=36(qemu) gid=34(kvm) groups=34(kvm)
	if err := execFg("chown", "36:34", diskPath); err != nil {
		return err
	}

	return nil
}

func createQCOW2(diskName string, diskPath string, diskSize *resource.Quantity, contentPath *string) error {
	ext4blocksMin := int64(64)
	ext4blockSize := int64(4096)
	ext4blockCount := int64(0)

	if diskSize != nil {
		ext4blockCount = diskSize.Value() / ext4blockSize
	} else if contentPath != nil {
		dirSize, err := calcDirUsage(*contentPath)
		if err != nil {
			return err
		}
		ext4blockCount = int64(math.Ceil(float64(ext4blocksMin) + float64((dirSize / ext4blockSize))))
	} else {
		return errors.New("diskSize or contentPath should be specified")
	}

	mkfsArgs := []string{
		"-q", // quiet
		"-L", // volume-label
		diskName,
	}

	if contentPath != nil {
		// [ -d root-directory|tarball ]
		mkfsArgs = append(mkfsArgs, "-d", *contentPath)
	}

	mkfsArgs = append(
		mkfsArgs,
		"-b", // block-size
		fmt.Sprintf("%d", ext4blockSize),
		"ext4.raw",                        // device
		fmt.Sprintf("%d", ext4blockCount), // fs-size
	)

	if err := execFg("mkfs.ext4", mkfsArgs...); err != nil {
		return err
	}

	if err := execFg(qemuImgBin, "convert", "-q", "-f", "raw", "-O", "qcow2", "-o", "cluster_size=2M,lazy_refcounts=on", "ext4.raw", diskPath); err != nil {
		return err
	}

	if err := execFg("rm", "-f", "ext4.raw"); err != nil {
		return err
	}

	// uid=36(qemu) gid=34(kvm) groups=34(kvm)
	if err := execFg("chown", "36:34", diskPath); err != nil {
		return err
	}

	return nil
}

func createISO9660FromPath(logger *zap.Logger, diskName string, diskPath string, contentPath string) error {
	writer, err := iso9660.NewWriter()
	if err != nil {
		return err
	}
	defer writer.Cleanup() //nolint:errcheck // Nothing to do with the error, maybe log it ? TODO

	dir, err := os.Open(contentPath)
	if err != nil {
		return err
	}
	dirEntrys, err := dir.ReadDir(0)
	if err != nil {
		return err
	}

	for _, file := range dirEntrys {
		fileName := fmt.Sprintf("%s/%s", contentPath, file.Name())
		outputPath := file.Name()

		if file.IsDir() {
			continue
		}
		// try to resolve symlink and check resolved file IsDir
		resolved, err := filepath.EvalSymlinks(fileName)
		if err != nil {
			return err
		}
		resolvedOpen, err := os.Open(resolved)
		if err != nil {
			return err
		}
		resolvedStat, err := resolvedOpen.Stat()
		if err != nil {
			return err
		}
		if resolvedStat.IsDir() {
			continue
		}

		// run the file handling logic in a closure, so the defers happen within the loop body,
		// rather than the outer function.
		err = func() error {
			logger.Info("adding file to ISO9660 disk", zap.String("path", outputPath))
			fileToAdd, err := os.Open(fileName)
			if err != nil {
				return err
			}
			defer fileToAdd.Close()

			return writer.AddFile(fileToAdd, outputPath)
		}()
		if err != nil {
			return err
		}
	}

	outputFile, err := os.OpenFile(diskPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	// uid=36(qemu) gid=34(kvm) groups=34(kvm)
	err = outputFile.Chown(36, 34)
	if err != nil {
		return err
	}

	err = writer.WriteTo(outputFile, diskName)
	if err != nil {
		return err
	}

	err = outputFile.Close()
	if err != nil {
		return err
	}

	return nil
}

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

type NetworkMonitoringMetrics struct {
	IngressBytes, EgressBytes, Errors prometheus.Counter
	IngressBytesRaw, EgressBytesRaw   uint64 // Absolute values to calc increments for Counters
}

func NewMonitoringMetrics(reg *prometheus.Registry) *NetworkMonitoringMetrics {
	m := &NetworkMonitoringMetrics{
		IngressBytes: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "runner_vm_ingress_bytes",
				Help: "Number of bytes received by the VM from the open internet",
			},
		)),
		EgressBytes: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "runner_vm_egress_bytes",
				Help: "Number of bytes sent by the VM to the open internet",
			},
		)),
		IngressBytesRaw: 0,
		EgressBytesRaw:  0,
		Errors: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "runner_vm_network_fetch_errors_total",
				Help: "Number of errors while fetching network monitoring data",
			},
		)),
	}
	return m
}

func shouldBeIgnored(ip net.IP) bool {
	// We need to measure only external traffic to/from vm, so we filter internal traffic
	// Don't filter on isUnspecified as it's an iptables rule, not a real ip
	return ip.IsLoopback() || ip.IsPrivate()
}

func getNetworkBytesCounter(iptables *iptables.IPTables, chain string) (uint64, error) {
	cnt := uint64(0)
	rules, err := iptables.Stats("filter", chain)
	if err != nil {
		return cnt, err
	}

	for _, rawStat := range rules {
		stat, err := iptables.ParseStat(rawStat)
		if err != nil {
			return cnt, err
		}
		src, dest := stat.Source.IP, stat.Destination.IP
		if stat.Protocol == protocolTCP && !shouldBeIgnored(src) && !shouldBeIgnored(dest) {
			cnt += stat.Bytes
		}
	}
	return cnt, nil
}

func (m *NetworkMonitoringMetrics) update(logger *zap.Logger) {
	// Rules configured at github.com/neondatabase/cloud/blob/main/compute-init/compute-init.sh#L98
	iptables, err := iptables.New()
	if err != nil {
		logger.Error("initializing iptables failed", zap.Error(err))
		m.Errors.Inc()
		return
	}

	ingress, err := getNetworkBytesCounter(iptables, "INPUT")
	if err != nil {
		logger.Error("getting iptables input counter failed", zap.Error(err))
		m.Errors.Inc()
		return
	}
	m.IngressBytes.Add(float64(ingress - m.IngressBytesRaw))
	m.IngressBytesRaw = ingress

	egress, err := getNetworkBytesCounter(iptables, "OUTPUT")
	if err != nil {
		logger.Error("getting iptables output counter failed", zap.Error(err))
		m.Errors.Inc()
		return
	}
	m.EgressBytes.Add(float64(egress - m.EgressBytesRaw))
	m.EgressBytesRaw = egress
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

	// create iso9660 disk with runtime options (command, args, envs, mounts)
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

func resizeRootDisk(logger *zap.Logger, vmSpec *vmv1.VirtualMachineSpec) error {
	// resize rootDisk image of size specified and new size more than current
	type QemuImgOutputPartial struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	// get current disk size by qemu-img info command
	qemuImgOut, err := exec.Command(qemuImgBin, "info", "--output=json", rootDiskPath).Output()
	if err != nil {
		return fmt.Errorf("could not get root image size: %w", err)
	}
	var imageSize QemuImgOutputPartial
	if err := json.Unmarshal(qemuImgOut, &imageSize); err != nil {
		return fmt.Errorf("failed to unmarshal QEMU image size: %w", err)
	}
	imageSizeQuantity := resource.NewQuantity(imageSize.VirtualSize, resource.BinarySI)

	// going to resize
	if !vmSpec.Guest.RootDisk.Size.IsZero() {
		if vmSpec.Guest.RootDisk.Size.Cmp(*imageSizeQuantity) == 1 {
			logger.Info(fmt.Sprintf("resizing rootDisk from %s to %s", imageSizeQuantity.String(), vmSpec.Guest.RootDisk.Size.String()))
			if err := execFg(qemuImgBin, "resize", rootDiskPath, fmt.Sprintf("%d", vmSpec.Guest.RootDisk.Size.Value())); err != nil {
				return fmt.Errorf("failed to resize rootDisk: %w", err)
			}
		} else {
			logger.Info(fmt.Sprintf("rootDisk.size (%s) is less than than image size (%s)", vmSpec.Guest.RootDisk.Size.String(), imageSizeQuantity.String()))
		}
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
		"-serial", "stdio",
		"-msg", "timestamp=on",
		"-qmp", fmt.Sprintf("tcp:0.0.0.0:%d,server,wait=off", vmSpec.QMP),
		"-qmp", fmt.Sprintf("tcp:0.0.0.0:%d,server,wait=off", vmSpec.QMPManual),
		"-qmp", fmt.Sprintf("unix:%s,server,wait=off", qmpUnixSocketForSigtermHandler),
		"-device", "virtio-serial",
		"-chardev", fmt.Sprintf("socket,path=%s,server=on,wait=off,id=log", logSerialSocket),
		"-device", "virtserialport,chardev=log,name=tech.neon.log.0",
	}

	// disk details
	qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=rootdisk,file=%s,if=virtio,media=disk,index=0,%s", rootDiskPath, cfg.diskCacheSettings))
	qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=runtime,file=%s,if=virtio,media=cdrom,readonly=on,cache=none", runtimeDiskPath))

	if enableSSH {
		name := "ssh-authorized-keys"
		if err := createISO9660FromPath(logger, name, sshAuthorizedKeysDiskPath, sshAuthorizedKeysMountPoint); err != nil {
			return nil, fmt.Errorf("Failed to create ISO9660 image: %w", err)
		}
		qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=%s,file=%s,if=virtio,media=cdrom,cache=none", name, sshAuthorizedKeysDiskPath))
	}

	if swapSize != nil {
		dPath := fmt.Sprintf("%s/swapdisk.qcow2", mountedDiskPath)
		logger.Info("creating QCOW2 image for swap", zap.String("diskPath", dPath))
		if err := createSwap(dPath, swapSize); err != nil {
			return nil, fmt.Errorf("Failed to create swap disk: %w", err)
		}
		qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=%s,file=%s,if=virtio,media=disk,%s,discard=unmap", swapName, dPath, cfg.diskCacheSettings))
	}

	if cfg.architecture == architectureArm64 {
		qemuCmd = append(qemuCmd, "-bios", "/vm/QEMU_EFI_ARM.fd")
	}

	for _, disk := range vmSpec.Disks {
		switch {
		case disk.EmptyDisk != nil:
			logger.Info("creating QCOW2 image with empty ext4 filesystem", zap.String("diskName", disk.Name))
			dPath := fmt.Sprintf("%s/%s.qcow2", mountedDiskPath, disk.Name)
			if err := createQCOW2(disk.Name, dPath, &disk.EmptyDisk.Size, nil); err != nil {
				return nil, fmt.Errorf("Failed to create QCOW2 image: %w", err)
			}
			discard := ""
			if disk.EmptyDisk.Discard {
				discard = ",discard=unmap"
			}
			qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=%s,file=%s,if=virtio,media=disk,%s%s", disk.Name, dPath, cfg.diskCacheSettings, discard))
		case disk.ConfigMap != nil || disk.Secret != nil:
			dPath := fmt.Sprintf("%s/%s.iso", mountedDiskPath, disk.Name)
			mnt := fmt.Sprintf("/vm/mounts%s", disk.MountPath)
			logger.Info("creating iso9660 image", zap.String("diskPath", dPath), zap.String("diskName", disk.Name), zap.String("mountPath", mnt))
			if err := createISO9660FromPath(logger, disk.Name, dPath, mnt); err != nil {
				return nil, fmt.Errorf("Failed to create ISO9660 image: %w", err)
			}
			qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=%s,file=%s,if=virtio,media=cdrom,cache=none", disk.Name, dPath))
		default:
			// do nothing
		}
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

	// default (pod) net details
	macDefault, err := defaultNetwork(logger, defaultNetworkCIDR, vmSpec.Guest.Ports)
	if err != nil {
		return nil, fmt.Errorf("Failed to set up default network: %w", err)
	}
	qemuCmd = append(qemuCmd, "-netdev", fmt.Sprintf("tap,id=default,ifname=%s,script=no,downscript=no,vhost=on", defaultNetworkTapName))
	qemuCmd = append(qemuCmd, "-device", fmt.Sprintf("virtio-net-pci,netdev=default,mac=%s", macDefault.String()))

	// overlay (multus) net details
	if vmSpec.ExtraNetwork != nil && vmSpec.ExtraNetwork.Enable {
		macOverlay, err := overlayNetwork(vmSpec.ExtraNetwork.Interface)
		if err != nil {
			return nil, fmt.Errorf("Failed to set up overlay network: %w", err)
		}
		qemuCmd = append(qemuCmd, "-netdev", fmt.Sprintf("tap,id=overlay,ifname=%s,script=no,downscript=no,vhost=on", overlayNetworkTapName))
		qemuCmd = append(qemuCmd, "-device", fmt.Sprintf("virtio-net-pci,netdev=overlay,mac=%s", macOverlay.String()))
	}

	// kernel details
	qemuCmd = append(
		qemuCmd,
		"-kernel", cfg.kernelPath,
		"-append", makeKernelCmdline(cfg, vmSpec, vmStatus, hostname),
	)

	// should runner receive migration ?
	if os.Getenv("RECEIVE_MIGRATION") == "true" {
		qemuCmd = append(qemuCmd, "-incoming", fmt.Sprintf("tcp:0:%d", vmv1.MigrationPort))
	}

	return qemuCmd, nil
}

const (
	baseKernelCmdline          = "panic=-1 init=/neonvm/bin/init console=ttyS1 loglevel=7 root=/dev/vda rw"
	kernelCmdlineDIMMSlots     = "memhp_default_state=online_movable"
	kernelCmdlineVirtioMemTmpl = "memhp_default_state=online memory_hotplug.online_policy=auto-movable memory_hotplug.auto_movable_ratio=%s"
)

func makeKernelCmdline(cfg *Config, vmSpec *vmv1.VirtualMachineSpec, vmStatus *vmv1.VirtualMachineStatus, hostname string) string {
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

	if cfg.architecture == architectureArm64 {
		// explicitly enable acpi if we run on arm
		cmdlineParts = append(cmdlineParts, "acpi=on")
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
		return "virt"
	case architectureAmd64:
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

func calcIPs(cidr string) (net.IP, net.IP, net.IPMask, error) {
	_, ipv4Net, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, nil, nil, err
	}

	ip0 := ipv4Net.IP.To4()
	mask := ipv4Net.Mask

	ip1 := append(net.IP{}, ip0...)
	ip1[3]++
	ip2 := append(net.IP{}, ip1...)
	ip2[3]++

	return ip1, ip2, mask, nil
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

func defaultNetwork(logger *zap.Logger, cidr string, ports []vmv1.Port) (mac.MAC, error) {
	// gerenare random MAC for default Guest interface
	mac, err := mac.GenerateRandMAC()
	if err != nil {
		logger.Fatal("could not generate random MAC", zap.Error(err))
		return nil, err
	}

	// create an configure linux bridge
	logger.Info("setup bridge interface", zap.String("name", defaultNetworkBridgeName))
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: defaultNetworkBridgeName,
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		logger.Fatal("could not create bridge interface", zap.Error(err))
		return nil, err
	}
	ipPod, ipVm, mask, err := calcIPs(cidr)
	if err != nil {
		logger.Fatal("could not parse IP", zap.Error(err))
		return nil, err
	}
	bridgeAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ipPod,
			Mask: mask,
		},
	}
	if err := netlink.AddrAdd(bridge, bridgeAddr); err != nil {
		logger.Fatal("could not parse IP", zap.Error(err))
		return nil, err
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		logger.Fatal("could not set up bridge", zap.Error(err))
		return nil, err
	}

	// create an configure TAP interface
	if !checkDevTun() {
		logger.Info("create /dev/net/tun")
		if err := execFg("mkdir", "-p", "/dev/net"); err != nil {
			return nil, err
		}
		if err := execFg("mknod", "/dev/net/tun", "c", "10", "200"); err != nil {
			return nil, err
		}
		if err := execFg("chown", "qemu:kvm", "/dev/net/tun"); err != nil {
			return nil, err
		}
	}

	logger.Info("setup tap interface", zap.String("name", defaultNetworkTapName))
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: defaultNetworkTapName,
		},
		Mode:  netlink.TUNTAP_MODE_TAP,
		Flags: netlink.TUNTAP_DEFAULTS,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		logger.Error("could not add tap device", zap.Error(err))
		return nil, err
	}
	if err := netlink.LinkSetMaster(tap, bridge); err != nil {
		logger.Error("could not set up tap as master", zap.Error(err))
		return nil, err
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		logger.Error("could not set up tap device", zap.Error(err))
		return nil, err
	}

	// setup masquerading outgoing (from VM) traffic
	logger.Info("setup masquerading for outgoing traffic")
	if err := execFg("iptables", "-t", "nat", "-A", "POSTROUTING", "-o", "eth0", "-j", "MASQUERADE"); err != nil {
		logger.Error("could not setup masquerading for outgoing traffic", zap.Error(err))
		return nil, err
	}

	// pass incoming traffic to .Guest.Spec.Ports into VM
	var iptablesArgs []string
	for _, port := range ports {
		logger.Info(fmt.Sprintf("setup DNAT rule for incoming traffic to port %d", port.Port))
		iptablesArgs = []string{
			"-t", "nat", "-A", "PREROUTING",
			"-i", "eth0", "-p", fmt.Sprint(port.Protocol), "--dport", fmt.Sprint(port.Port),
			"-j", "DNAT", "--to", fmt.Sprintf("%s:%d", ipVm.String(), port.Port),
		}
		if err := execFg("iptables", iptablesArgs...); err != nil {
			logger.Error("could not set up DNAT rule for incoming traffic", zap.Error(err))
			return nil, err
		}
		logger.Info(fmt.Sprintf("setup DNAT rule for traffic originating from localhost to port %d", port.Port))
		iptablesArgs = []string{
			"-t", "nat", "-A", "OUTPUT",
			"-m", "addrtype", "--src-type", "LOCAL", "--dst-type", "LOCAL",
			"-p", fmt.Sprint(port.Protocol), "--dport", fmt.Sprint(port.Port),
			"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", ipVm.String(), port.Port),
		}
		if err := execFg("iptables", iptablesArgs...); err != nil {
			logger.Error("could not set up DNAT rule for traffic from localhost", zap.Error(err))
			return nil, err
		}
		logger.Info(fmt.Sprintf("setup ACCEPT rule for traffic originating from localhost to port %d", port.Port))
		iptablesArgs = []string{
			"-A", "OUTPUT",
			"-s", "127.0.0.1", "-d", ipVm.String(),
			"-p", fmt.Sprint(port.Protocol), "--dport", fmt.Sprint(port.Port),
			"-j", "ACCEPT",
		}
		if err := execFg("iptables", iptablesArgs...); err != nil {
			logger.Error("could not set up ACCEPT rule for traffic from localhost", zap.Error(err))
			return nil, err
		}
	}
	logger.Info("setup MASQUERADE rule for traffic originating from localhost")
	iptablesArgs = []string{
		"-t", "nat", "-A", "POSTROUTING",
		"-m", "addrtype", "--src-type", "LOCAL", "--dst-type", "UNICAST",
		"-j", "MASQUERADE",
	}
	if err := execFg("iptables", iptablesArgs...); err != nil {
		logger.Error("could not set up MASQUERADE rule for traffic from localhost", zap.Error(err))
		return nil, err
	}

	// get dns details from /etc/resolv.conf
	resolvConf, err := getResolvConf()
	if err != nil {
		logger.Error("could not get DNS details", zap.Error(err))
		return nil, err
	}
	dns := getNameservers(resolvConf.Content, types.IP)[0]
	dnsSearch := strings.Join(getSearchDomains(resolvConf.Content), ",")

	// prepare dnsmask command line (instead of config file)
	logger.Info("run dnsmasq for interface", zap.String("name", defaultNetworkBridgeName))
	dnsMaskCmd := []string{
		// No DNS, DHCP only
		"--port=0",

		// Because we don't provide DNS, no need to load resolv.conf. This helps to
		// avoid "dnsmasq: failed to create inotify: No file descriptors available"
		// errors.
		"--no-resolv",

		"--bind-interfaces",
		"--dhcp-authoritative",
		fmt.Sprintf("--interface=%s", defaultNetworkBridgeName),
		fmt.Sprintf("--dhcp-range=%s,static,%d.%d.%d.%d", ipVm.String(), mask[0], mask[1], mask[2], mask[3]),
		fmt.Sprintf("--dhcp-host=%s,%s,infinite", mac.String(), ipVm.String()),
		fmt.Sprintf("--dhcp-option=option:router,%s", ipPod.String()),
		fmt.Sprintf("--dhcp-option=option:dns-server,%s", dns),
		fmt.Sprintf("--dhcp-option=option:domain-search,%s", dnsSearch),
		fmt.Sprintf("--shared-network=%s,%s", defaultNetworkBridgeName, ipVm.String()),
	}

	// run dnsmasq for default Guest interface
	if err := execFg("dnsmasq", dnsMaskCmd...); err != nil {
		logger.Error("could not run dnsmasq", zap.Error(err))
		return nil, err
	}

	// Adding VM's IP address to the /etc/hosts, so we can access it easily from
	// the pod. This is particularly useful for ssh into the VM from the runner
	// pod.
	f, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	record := fmt.Sprintf("%v guest-vm\n", ipVm)
	if _, err := f.WriteString(record); err != nil {
		return nil, err
	}

	return mac, nil
}

func overlayNetwork(iface string) (mac.MAC, error) {
	// gerenare random MAC for overlay Guest interface
	mac, err := mac.GenerateRandMAC()
	if err != nil {
		return nil, err
	}

	// create and configure linux bridge
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: overlayNetworkBridgeName,
			Protinfo: &netlink.Protinfo{
				Learning: false,
			},
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		return nil, err
	}

	// create an configure TAP interface
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: overlayNetworkTapName,
		},
		Mode:  netlink.TUNTAP_MODE_TAP,
		Flags: netlink.TUNTAP_DEFAULTS,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetMaster(tap, bridge); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		return nil, err
	}

	// add overlay interface to bridge as well
	overlayLink, err := netlink.LinkByName(iface)
	if err != nil {
		return nil, err
	}
	// firsly delete IP address(es) (it it exist) from overlay interface
	overlayAddrs, err := netlink.AddrList(overlayLink, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	for _, a := range overlayAddrs {
		ip := a.IPNet
		if ip != nil {
			if err := netlink.AddrDel(overlayLink, &a); err != nil {
				return nil, err
			}
		}
	}
	// and now add overlay link to bridge
	if err := netlink.LinkSetMaster(overlayLink, bridge); err != nil {
		return nil, err
	}

	return mac, nil
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
