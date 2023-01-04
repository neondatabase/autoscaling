package main

import (
	"encoding/base64"
	"encoding/json"

	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/alessio/shellescape"
	"github.com/cilium/cilium/pkg/mac"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/libnetwork/types"
	"github.com/kdomanski/iso9660"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/api/resource"

	vmv1 "github.com/neondatabase/neonvm/apis/neonvm/v1"
)

const (
	QEMU_BIN      = "qemu-system-x86_64"
	QEMU_IMG_BIN  = "qemu-img"
	kernelPath    = "/vm/kernel/vmlinuz"
	kernelCmdline = "init=/neonvm/bin/init memhp_default_state=online_movable console=ttyS1 loglevel=7 root=/dev/vda rw"

	rootDiskPath    = "/vm/images/rootdisk.qcow2"
	runtimeDiskPath = "/vm/images/runtime.iso"
	mountedDiskPath = "/vm/images"

	defaultNetworkBridgeName = "br-def"
	defaultNetworkTapName    = "tap-def"
	defaultNetworkCIDR       = "10.255.255.252/30"

	// defaultPath is the default path to the resolv.conf that contains information to resolve DNS. See Path().
	resolveDefaultPath = "/etc/resolv.conf"
	// alternatePath is a path different from defaultPath, that may be used to resolve DNS. See Path().
	resolveAlternatePath = "/run/systemd/resolve/resolv.conf"
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
	resolv, err := ioutil.ReadFile(path)
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
		candidateResolvConf, err := ioutil.ReadFile(resolveDefaultPath)
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
		return fmt.Errorf("diskSize or contentPath should be specified")
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

		log.Printf("adding file: %s\n", outputPath)
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
	if mode&os.ModeCharDevice == os.ModeCharDevice {
		return true
	}
	return false
}

func main() {
	var vmSpecDump string
	flag.StringVar(&vmSpecDump, "vmdump", vmSpecDump, "Base64 encoded VirtualMachine json specification")
	flag.Parse()

	vmSpecJson, err := base64.StdEncoding.DecodeString(vmSpecDump)
	if err != nil {
		log.Fatalf("Failed to decode VirtualMachine dump: %s", err)
	}

	vmSpec := &vmv1.VirtualMachineSpec{}
	if err := json.Unmarshal(vmSpecJson, vmSpec); err != nil {
		log.Fatalf("Failed to unmarshal VM: %s", err)
	}

	cpus := []string{}
	cpus = append(cpus, fmt.Sprintf("cpus=%d", *vmSpec.Guest.CPUs.Min))
	if vmSpec.Guest.CPUs.Max != nil {
		cpus = append(cpus, fmt.Sprintf("maxcpus=%d,sockets=1,cores=%d,threads=1", *vmSpec.Guest.CPUs.Max, *vmSpec.Guest.CPUs.Max))
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
		log.Fatalln(err)
	}
	imageSize := QemuImgOutputPartial{}
	json.Unmarshal(qemuImgOut, &imageSize)
	imageSizeQuantity := resource.NewQuantity(imageSize.VirtualSize, resource.BinarySI)

	// going to resize
	if !vmSpec.Guest.RootDisk.Size.IsZero() {
		if vmSpec.Guest.RootDisk.Size.Cmp(*imageSizeQuantity) == 1 {
			log.Printf("resizing rootDisk from %s to %s\n", imageSizeQuantity.String(), vmSpec.Guest.RootDisk.Size.String())
			if err := execFg(QEMU_IMG_BIN, "resize", rootDiskPath, fmt.Sprintf("%d", vmSpec.Guest.RootDisk.Size.Value())); err != nil {
				log.Fatal(err)
			}
		} else {
			log.Printf("rootDisk.size (%s) should be more than size in image (%s)\n", vmSpec.Guest.RootDisk.Size.String(), imageSizeQuantity.String())
		}
	}

	// prepare qemu command line
	qemuCmd := []string{
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

	// kernel details
	qemuCmd = append(qemuCmd, "-kernel", kernelPath)
	qemuCmd = append(qemuCmd, "-append", kernelCmdline)

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
	if checkKVM() {
		log.Printf("using KVM acceleration\n")
		qemuCmd = append(qemuCmd, "-enable-kvm")
	}
	qemuCmd = append(qemuCmd, "-cpu", "max")
	qemuCmd = append(qemuCmd, "-smp", strings.Join(cpus, ","))

	// memory details
	qemuCmd = append(qemuCmd, "-m", strings.Join(memory, ","))

	// net details
	macDefault, err := defaultNetwork(defaultNetworkCIDR, vmSpec.Guest.Ports)
	if err != nil {
		log.Fatalf("can not setup default network: %s", err)
	}
	qemuCmd = append(qemuCmd, "-netdev", fmt.Sprintf("tap,id=default,ifname=%s,script=no,downscript=no,vhost=on", defaultNetworkTapName))
	qemuCmd = append(qemuCmd, "-device", fmt.Sprintf("virtio-net-pci,netdev=default,mac=%s", macDefault.String()))

	// should runner receive migration ?
	if os.Getenv("RECEIVE_MIGRATION") == "true" {
		qemuCmd = append(qemuCmd, "-incoming", fmt.Sprintf("tcp:0:%d", vmv1.MigrationPort))
	}

	if err := execFg(QEMU_BIN, qemuCmd...); err != nil {
		log.Fatal(err)
	}
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
		return nil, err
	}

	// create an configure linux bridge
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: defaultNetworkBridgeName,
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		return nil, err
	}
	ipPod, ipVm, mask, err := calcIPs(cidr)
	if err != nil {
		return nil, err
	}
	bridgeAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ipPod,
			Mask: mask,
		},
	}
	if err := netlink.AddrAdd(bridge, bridgeAddr); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		return nil, err
	}

	// create an configure TAP interface
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: defaultNetworkTapName,
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

	// enable routing
	if err := execFg("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return nil, err
	}

	// setup masquerading outgoing (from VM) traffic
	if err := execFg("iptables", "-t", "nat", "-A", "POSTROUTING", "-o", "eth0", "-j", "MASQUERADE"); err != nil {
		return nil, err
	}

	// pass incoming traffic to .Guest.Spec.Ports into VM
	for _, port := range ports {
		iptablesArgs := []string{
			"-t", "nat", "-A", "PREROUTING",
			"-i", "eth0", "-p", fmt.Sprint(port.Protocol), "--dport", fmt.Sprint(port.Port),
			"-j", "DNAT", "--to", fmt.Sprintf("%s:%d", ipVm.String(), port.Port),
		}
		if err := execFg("iptables", iptablesArgs...); err != nil {
			return nil, err
		}
	}

	// get dns details from /etc/resolv.conf
	resolvConf, err := getResolvConf()
	if err != nil {
		return nil, err
	}
	dns := getNameservers(resolvConf.Content, types.IP)[0]
	dnsSearch := strings.Join(getSearchDomains(resolvConf.Content), ",")

	// prepare dnsmask command line (instead of config file)
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
		return nil, err
	}

	return mac, nil
}
