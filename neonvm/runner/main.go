package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"bytes"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alessio/shellescape"
	"github.com/cilium/cilium/pkg/mac"
	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/digitalocean/go-qemu/qmp"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/libnetwork/types"
	"github.com/kdomanski/iso9660"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/api/resource"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
)

const (
	QEMU_BIN      = "qemu-system-x86_64"
	QEMU_IMG_BIN  = "qemu-img"
	kernelPath    = "/vm/kernel/vmlinuz"
	kernelCmdline = "panic=-1 init=/neonvm/bin/init memhp_default_state=online_movable console=ttyS1 loglevel=7 root=/dev/vda rw"

	rootDiskPath    = "/vm/images/rootdisk.qcow2"
	runtimeDiskPath = "/vm/images/runtime.iso"
	mountedDiskPath = "/vm/images"

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

// GetSpecific returns the contents of the user specified resolv.conf file and its hash
func getSpecific(path string) (*resolveFile, error) {
	resolv, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	hash, err := ioutils.HashData(bytes.NewReader(resolv))
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
		var commentIndex = bytes.Index(currentLine, commentMarker)
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

func createISO9660runtime(diskPath string, command []string, args []string, env []vmv1.EnvVar, disks []vmv1.Disk) error {
	writer, err := iso9660.NewWriter()
	if err != nil {
		return err
	}
	defer writer.Cleanup()

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

	if len(disks) != 0 {
		mounts := []string{}
		for _, disk := range disks {
			mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mkdir -p %s`, disk.MountPath))
			switch {
			case disk.EmptyDisk != nil:
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mount $(/neonvm/bin/blkid -L %s) %s`, disk.Name, disk.MountPath))
			case disk.ConfigMap != nil || disk.Secret != nil:
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mount -o ro,mode=0644 $(/neonvm/bin/blkid -L %s) %s`, disk.Name, disk.MountPath))
			case disk.Tmpfs != nil:
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/chmod 0777 %s`, disk.MountPath))
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mount -t tmpfs -o size=%d %s %s`, disk.Tmpfs.Size.Value(), disk.Name, disk.MountPath))
			default:
				// do nothing
			}
		}
		mounts = append(mounts, "")
		err = writer.AddFile(bytes.NewReader([]byte(strings.Join(mounts, "\n"))), "mounts.sh")
		if err != nil {
			return err
		}
	}

	outputFile, err := os.OpenFile(diskPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0644)
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

	if contentPath == nil {
		if err := execFg("mkfs.ext4", "-q", "-L", diskName, "-b", fmt.Sprintf("%d", ext4blockSize), "ext4.raw", fmt.Sprintf("%d", ext4blockCount)); err != nil {
			return err
		}
	} else {
		if err := execFg("mkfs.ext4", "-q", "-L", diskName, "-d", *contentPath, "-b", fmt.Sprintf("%d", ext4blockSize), "ext4.raw", fmt.Sprintf("%d", ext4blockCount)); err != nil {
			return err
		}
	}

	if err := execFg(QEMU_IMG_BIN, "convert", "-q", "-f", "raw", "-O", "qcow2", "-o", "cluster_size=2M,lazy_refcounts=on", "ext4.raw", diskPath); err != nil {
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

func createISO9660FromPath(diskName string, diskPath string, contentPath string) error {
	writer, err := iso9660.NewWriter()
	if err != nil {
		return err
	}
	defer writer.Cleanup()

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

		log.Printf("adding file: %s", outputPath)
		fileToAdd, err := os.Open(fileName)
		if err != nil {
			return err
		}
		defer fileToAdd.Close()

		err = writer.AddFile(fileToAdd, outputPath)
		if err != nil {
			return err
		}
	}

	outputFile, err := os.OpenFile(diskPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0644)
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

func main() {
	var vmSpecDump string
	var vmStatusDump string
	flag.StringVar(&vmSpecDump, "vmspec", vmSpecDump, "Base64 encoded VirtualMachine json specification")
	flag.StringVar(&vmStatusDump, "vmstatus", vmStatusDump, "Base64 encoded VirtualMachine json status")
	flag.Parse()

	selfPodName, ok := os.LookupEnv("K8S_POD_NAME")
	if !ok {
		log.Fatalf("environment variable K8S_POD_NAME missing")
	}

	vmSpecJson, err := base64.StdEncoding.DecodeString(vmSpecDump)
	if err != nil {
		log.Fatalf("Failed to decode VirtualMachine Spec dump: %s", err)
	}
	vmStatusJson, err := base64.StdEncoding.DecodeString(vmStatusDump)
	if err != nil {
		log.Fatalf("Failed to decode VirtualMachine Status dump: %s", err)
	}

	vmSpec := &vmv1.VirtualMachineSpec{}
	if err := json.Unmarshal(vmSpecJson, vmSpec); err != nil {
		log.Fatalf("Failed to unmarshal VM Spec: %s", err)
	}
	vmStatus := &vmv1.VirtualMachineStatus{}
	if err := json.Unmarshal(vmStatusJson, vmStatus); err != nil {
		log.Fatalf("Failed to unmarshal VM Status: %s", err)
	}

	qemuCPUs := processCPUs(vmSpec.Guest.CPUs)

	cpus := []string{}
	cpus = append(cpus, fmt.Sprintf("cpus=%d", qemuCPUs.min))
	if qemuCPUs.max != nil {
		cpus = append(cpus, fmt.Sprintf("maxcpus=%d,sockets=1,cores=%d,threads=1", *qemuCPUs.max, *qemuCPUs.max))
	}

	memory := []string{}
	memory = append(memory, fmt.Sprintf("size=%db", vmSpec.Guest.MemorySlotSize.Value()*int64(*vmSpec.Guest.MemorySlots.Min)))
	if vmSpec.Guest.MemorySlots.Max != nil {
		memory = append(memory, fmt.Sprintf("slots=%d", *vmSpec.Guest.MemorySlots.Max-*vmSpec.Guest.MemorySlots.Min))
		memory = append(memory, fmt.Sprintf("maxmem=%db", vmSpec.Guest.MemorySlotSize.Value()*int64(*vmSpec.Guest.MemorySlots.Max)))
	}

	// create iso9660 disk with runtime options (command, args, envs, mounts)
	if err = createISO9660runtime(runtimeDiskPath, vmSpec.Guest.Command, vmSpec.Guest.Args, vmSpec.Guest.Env, vmSpec.Disks); err != nil {
		log.Fatalln(err)
	}

	// resize rootDisk image of size specified and new size more than current
	type QemuImgOutputPartial struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	// get current disk size by qemu-img info command
	qemuImgOut, err := exec.Command(QEMU_IMG_BIN, "info", "--output=json", rootDiskPath).Output()
	if err != nil {
		log.Fatalf("could not get root image size: %s", err)
	}
	imageSize := QemuImgOutputPartial{}
	json.Unmarshal(qemuImgOut, &imageSize)
	imageSizeQuantity := resource.NewQuantity(imageSize.VirtualSize, resource.BinarySI)

	// going to resize
	if !vmSpec.Guest.RootDisk.Size.IsZero() {
		if vmSpec.Guest.RootDisk.Size.Cmp(*imageSizeQuantity) == 1 {
			log.Printf("resizing rootDisk from %s to %s", imageSizeQuantity.String(), vmSpec.Guest.RootDisk.Size.String())
			if err := execFg(QEMU_IMG_BIN, "resize", rootDiskPath, fmt.Sprintf("%d", vmSpec.Guest.RootDisk.Size.Value())); err != nil {
				log.Fatal(err)
			}
		} else {
			log.Printf("rootDisk.size (%s) should be more than size in image (%s)", vmSpec.Guest.RootDisk.Size.String(), imageSizeQuantity.String())
		}
	}

	// prepare qemu command line
	qemuCmd := []string{
		"-runas", "qemu",
		"-machine", "q35",
		"-nographic",
		"-no-reboot",
		"-nodefaults",
		"-only-migratable",
		"-audiodev", "none,id=noaudio",
		"-serial", "pty",
		"-serial", "stdio",
		"-msg", "timestamp=on",
		"-qmp", fmt.Sprintf("tcp:0.0.0.0:%d,server,wait=off", vmSpec.QMP),
	}

	// disk details
	qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=rootdisk,file=%s,if=virtio,media=disk,index=0,cache=none", rootDiskPath))
	qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=runtime,file=%s,if=virtio,media=cdrom,readonly=on,cache=none", runtimeDiskPath))
	for _, disk := range vmSpec.Disks {
		switch {
		case disk.EmptyDisk != nil:
			log.Printf("creating QCOW2 image '%s' with empty ext4 filesystem", disk.Name)
			dPath := fmt.Sprintf("%s/%s.qcow2", mountedDiskPath, disk.Name)
			if err := createQCOW2(disk.Name, dPath, &disk.EmptyDisk.Size, nil); err != nil {
				log.Fatalln(err)
			}
			qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=%s,file=%s,if=virtio,media=disk,cache=none", disk.Name, dPath))
		case disk.ConfigMap != nil || disk.Secret != nil:
			dPath := fmt.Sprintf("%s/%s.qcow2", mountedDiskPath, disk.Name)
			mnt := fmt.Sprintf("/vm/mounts%s", disk.MountPath)
			log.Printf("creating iso9660 image '%s' for '%s' from path '%s'", dPath, disk.Name, mnt)
			if err := createISO9660FromPath(disk.Name, dPath, mnt); err != nil {
				log.Fatalln(err)
			}
			qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=%s,file=%s,if=virtio,media=cdrom,cache=none", disk.Name, dPath))
		default:
			// do nothing
		}
	}

	// cpu details
	if vmSpec.EnableAcceleration && checkKVM() {
		log.Println("using KVM acceleration")
		qemuCmd = append(qemuCmd, "-enable-kvm")
	}
	qemuCmd = append(qemuCmd, "-cpu", "max")
	qemuCmd = append(qemuCmd, "-smp", strings.Join(cpus, ","))

	// memory details
	qemuCmd = append(qemuCmd, "-m", strings.Join(memory, ","))

	// default (pod) net details
	macDefault, err := defaultNetwork(defaultNetworkCIDR, vmSpec.Guest.Ports)
	if err != nil {
		log.Fatalf("can not setup default network: %s", err)
	}
	qemuCmd = append(qemuCmd, "-netdev", fmt.Sprintf("tap,id=default,ifname=%s,script=no,downscript=no,vhost=on", defaultNetworkTapName))
	qemuCmd = append(qemuCmd, "-device", fmt.Sprintf("virtio-net-pci,netdev=default,mac=%s", macDefault.String()))

	// overlay (multus) net details
	if vmSpec.ExtraNetwork != nil && vmSpec.ExtraNetwork.Enable {
		macOverlay, err := overlayNetwork(vmSpec.ExtraNetwork.Interface)
		if err != nil {
			log.Fatalf("can not setup overlay network: %s", err)
		}
		qemuCmd = append(qemuCmd, "-netdev", fmt.Sprintf("tap,id=overlay,ifname=%s,script=no,downscript=no,vhost=on", overlayNetworkTapName))
		qemuCmd = append(qemuCmd, "-device", fmt.Sprintf("virtio-net-pci,netdev=overlay,mac=%s", macOverlay.String()))
	}

	// kernel details
	qemuCmd = append(qemuCmd, "-kernel", kernelPath)
	if vmSpec.ExtraNetwork != nil && vmSpec.ExtraNetwork.Enable {
		qemuCmd = append(qemuCmd, "-append", fmt.Sprintf("ip=%s:::%s:%s:eth1:off %s", vmStatus.ExtraNetIP, vmStatus.ExtraNetMask, vmStatus.PodName, kernelCmdline))
	} else {
		qemuCmd = append(qemuCmd, "-append", kernelCmdline)
	}

	// should runner receive migration ?
	if os.Getenv("RECEIVE_MIGRATION") == "true" {
		qemuCmd = append(qemuCmd, "-incoming", fmt.Sprintf("tcp:0:%d", vmv1.MigrationPort))
	}

	selfCgroupPath, err := getSelfCgroupPath()
	if err != nil {
		log.Fatalf("Failed to get self cgroup path: %s", err)
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
	cgroupPath := fmt.Sprintf("%s/neonvm-qemu-%s", selfCgroupPath, selfPodName)

	log.Printf("Using QEMU cgroup path %q", cgroupPath)

	if err := setCgroupLimit(qemuCPUs.use, cgroupPath); err != nil {
		log.Fatalf("Failed to set cgroup limit: %s", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}

	wg.Add(1)
	go terminateQemuOnSigterm(ctx, vmSpec.QMP, &wg)
	wg.Add(1)
	go listenForCPUChanges(ctx, vmSpec.RunnerPort, cgroupPath, &wg)

	args := append([]string{"-g", fmt.Sprintf("cpu:%s", cgroupPath), QEMU_BIN}, qemuCmd...)
	log.Printf("using cgexec args: %v", args)
	if err := execFg("cgexec", args...); err != nil {
		log.Printf("Qemu exited: %s", err)
	}

	cancel()
	wg.Wait()
}

func handleCPUChange(w http.ResponseWriter, r *http.Request, cgroupPath string) {
	if r.Method != "POST" {
		log.Printf("unexpected method: %s", r.Method)
		w.WriteHeader(400)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("could not read body: %s", err)
		w.WriteHeader(400)
		return
	}

	parsed := api.VCPUChange{}
	err = json.Unmarshal(body, &parsed)
	if err != nil {
		log.Printf("could not parse body: %s", err)
		w.WriteHeader(400)
		return
	}

	// update cgroup
	log.Printf("got CPU update %v", parsed.VCPUs.AsFloat64())
	err = setCgroupLimit(parsed.VCPUs, cgroupPath)
	if err != nil {
		log.Printf("could not set cgroup limit: %s", err)
		w.WriteHeader(500)
		return
	}

	w.WriteHeader(200)
}

func handleCPUCurrent(w http.ResponseWriter, r *http.Request, cgroupPath string) {
	if r.Method != "GET" {
		log.Printf("unexpected method: %s", r.Method)
		w.WriteHeader(400)
		return
	}

	cpus, err := getCgroupQuota(cgroupPath)
	if err != nil {
		log.Printf("could not get cgroup quota: %s", err)
		w.WriteHeader(500)
		return
	}
	resp := api.VCPUCgroup{VCPUs: *cpus}
	body, err := json.Marshal(resp)
	if err != nil {
		log.Printf("could not marshal body: %s", err)
		w.WriteHeader(500)
		return
	}

	w.Header().Add("Content-Type", "application/json")
	w.Write(body)
}

func listenForCPUChanges(ctx context.Context, port int32, cgroupPath string, wg *sync.WaitGroup) {
	defer wg.Done()
	mux := http.NewServeMux()
	mux.HandleFunc("/cpu_change", func(w http.ResponseWriter, r *http.Request) {
		handleCPUChange(w, r, cgroupPath)
	})
	mux.HandleFunc("/cpu_current", func(w http.ResponseWriter, r *http.Request) {
		handleCPUCurrent(w, r, cgroupPath)
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
			log.Println("cpu_change server closed")
		} else if err != nil {
			log.Fatalf("error starting server: %s", err)
		}
	case <-ctx.Done():
		err := server.Shutdown(context.Background())
		log.Printf("shut down cpu_change server: %v", err)
	}
}

func getSelfCgroupPath() (string, error) {
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

	// Collect all candidate paths from the lines of the file. If there isn't exactly one,
	// something's wrong and we should make an error.
	var candidates []string
	for lineno, line := range strings.Split(string(procSelfCgroupContents), "\n") {
		if line == "" {
			continue
		}

		// Split into the three ':'-delimited fields
		fields := strings.Split(line, ":")
		if len(fields) != 3 {
			log.Printf("Contents of /proc/self/cgroup:\n%s", procSelfCgroupContents)
			return "", fmt.Errorf("line %d of /proc/self/cgroup did not have 3 colon-delimited fields", lineno+1)
		}

		id := fields[0]
		controllers := fields[1]
		path := fields[2]
		if id == "0" {
			candidates = append(candidates, path)
			continue
		}

		// It's not cgroup v2, otherwise id would have been 0. So, check if the comma-separated list
		// of controllers contains 'cpu' as an entry.
		for _, c := range strings.Split(controllers, ",") {
			if c == "cpu" {
				candidates = append(candidates, path)
				break // ... and then continue to the next loop iteration
			}
		}
	}

	// Success:
	if len(candidates) == 1 {
		return candidates[0], nil
	}

	// Failure cases:
	log.Printf("Contents of /proc/self/cgroup:\n%s", procSelfCgroupContents)
	if len(candidates) == 0 {
		return "", errors.New("Couldn't find applicable entry in /proc/self/cgroup")
	} else {
		return "", errors.New("More than one applicable entry in /proc/self/cgroup")
	}
}

func setCgroupLimit(r vmv1.MilliCPU, cgroupPath string) error {
	isV2 := cgroups.Mode() == cgroups.Unified
	period := cgroupPeriod
	// quota may be greater than period if the cgroup is allowed
	// to use more than 100% of a CPU.
	quota := int64(float64(r) / float64(1000) * float64(cgroupPeriod))
	log.Printf("setting cgroup to %v %v", quota, period)
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

func getCgroupQuota(cgroupPath string) (*vmv1.MilliCPU, error) {
	isV2 := cgroups.Mode() == cgroups.Unified
	var path string
	if isV2 {
		path = filepath.Join(cgroupMountPoint, cgroupPath, "cpu.max")
	} else {
		path = filepath.Join(cgroupMountPoint, "cpu", cgroupPath, "cpu.cfs_quota_us")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	arr := strings.Split(strings.Trim(string(data), "\n"), " ")
	if len(arr) == 0 {
		return nil, fmt.Errorf("unexpected cgroup data")
	}
	quota, err := strconv.ParseUint(arr[0], 10, 64)
	if err != nil {
		return nil, err
	}
	cpu := vmv1.MilliCPU(uint32(quota * 1000 / cgroupPeriod))
	return &cpu, nil
}

type QemuCPUs struct {
	max *int
	min int
	use vmv1.MilliCPU
}

func processCPUs(cpus vmv1.CPUs) QemuCPUs {
	min := int(cpus.Min.RoundedUp())
	use := *cpus.Min
	if cpus.Use != nil {
		use = *cpus.Use
	}

	var max *int
	if cpus.Max != nil {
		val := int(cpus.Max.RoundedUp())
		max = &val
	}
	return QemuCPUs{
		max: max,
		min: min,
		use: use,
	}
}

func terminateQemuOnSigterm(ctx context.Context, qmpPort int32, wg *sync.WaitGroup) {
	defer wg.Done()
	log.Println("watching OS signals")
	c := make(chan os.Signal, 1) // we need to reserve to buffer size 1, so the notifier are not blocked
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	select {
	case <-c:
	case <-ctx.Done():
	}

	log.Println("got signal, sending powerdown command to QEMU")

	mon, err := qmp.NewSocketMonitor("tcp", fmt.Sprintf("127.0.0.1:%d", qmpPort), 2*time.Second)
	if err != nil {
		log.Println(err)
		return
	}

	if err := mon.Connect(); err != nil {
		log.Println(err)
		return
	}
	defer mon.Disconnect()

	qmpcmd := []byte(`{"execute": "system_powerdown"}`)
	_, err = mon.Run(qmpcmd)
	if err != nil {
		log.Println(err)
		return
	}

	log.Println("system_powerdown command sent to QEMU")

	return
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

func defaultNetwork(cidr string, ports []vmv1.Port) (mac.MAC, error) {
	// gerenare random MAC for default Guest interface
	mac, err := mac.GenerateRandMAC()
	if err != nil {
		log.Fatalf("could not generate random MAC: %s", err)
		return nil, err
	}

	// create an configure linux bridge
	log.Printf("setup bridge interface %s", defaultNetworkBridgeName)
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: defaultNetworkBridgeName,
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		log.Fatalf("could not create bridge interface: %s", err)
		return nil, err
	}
	ipPod, ipVm, mask, err := calcIPs(cidr)
	if err != nil {
		log.Fatalf("could not parse IP: %s", err)
		return nil, err
	}
	bridgeAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ipPod,
			Mask: mask,
		},
	}
	if err := netlink.AddrAdd(bridge, bridgeAddr); err != nil {
		log.Fatalf("could not parse IP: %s", err)
		return nil, err
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		log.Fatalf("could not setup bridge: %s", err)
		return nil, err
	}

	// create an configure TAP interface
	if !checkDevTun() {
		log.Printf("create /dev/net/tun")
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

	log.Printf("setup tap interface %s", defaultNetworkTapName)
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: defaultNetworkTapName,
		},
		Mode:  netlink.TUNTAP_MODE_TAP,
		Flags: netlink.TUNTAP_DEFAULTS,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		log.Printf("could not add tap device: %s", err)
		return nil, err
	}
	if err := netlink.LinkSetMaster(tap, bridge); err != nil {
		log.Printf("could not setup tap as master: %s", err)
		return nil, err
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		log.Printf("could not setup tap device: %s", err)
		return nil, err
	}

	// setup masquerading outgoing (from VM) traffic
	log.Println("setup masquerading for outgoing traffic")
	if err := execFg("iptables", "-t", "nat", "-A", "POSTROUTING", "-o", "eth0", "-j", "MASQUERADE"); err != nil {
		log.Printf("could not setup masquerading for outgoing traffic: %s", err)
		return nil, err
	}

	// pass incoming traffic to .Guest.Spec.Ports into VM
	for _, port := range ports {
		log.Printf("setup DNAT for incoming traffic, port %d", port.Port)
		iptablesArgs := []string{
			"-t", "nat", "-A", "PREROUTING",
			"-i", "eth0", "-p", fmt.Sprint(port.Protocol), "--dport", fmt.Sprint(port.Port),
			"-j", "DNAT", "--to", fmt.Sprintf("%s:%d", ipVm.String(), port.Port),
		}
		if err := execFg("iptables", iptablesArgs...); err != nil {
			log.Printf("could not setup DNAT for incoming traffic: %s", err)
			return nil, err
		}
	}

	// get dns details from /etc/resolv.conf
	resolvConf, err := getResolvConf()
	if err != nil {
		log.Printf("could not get DNS details: %s", err)
		return nil, err
	}
	dns := getNameservers(resolvConf.Content, types.IP)[0]
	dnsSearch := strings.Join(getSearchDomains(resolvConf.Content), ",")

	// prepare dnsmask command line (instead of config file)
	log.Printf("run dnsmqsq for interface %s", defaultNetworkBridgeName)
	dnsMaskCmd := []string{
		"--port=0",
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
		log.Printf("could not run dnsmasq: %s", err)
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
