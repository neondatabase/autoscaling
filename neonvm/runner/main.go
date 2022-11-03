package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/cilium/cilium/pkg/mac"
	"github.com/moby/libnetwork/resolvconf"
	"github.com/vishvananda/netlink"

	vmv1 "github.com/neondatabase/neonvm/api/v1"
)

const (
	QEMU_BIN      = "qemu-system-x86_64"
	kernelPath    = "/kernel/vmlinuz"
	kernelCmdline = "console=ttyS1 loglevel=6 root=/dev/vda rw"

	rootDiskPath = "/images/rootdisk.qcow2"

	defaultNetworkBridgeName = "br-def"
	defaultNetworkTapName    = "tap-def"
	defaultNetworkCIDR       = "10.255.255.252/30"
)

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
		memory = append(memory, fmt.Sprintf("slots=%d", *vmSpec.Guest.MemorySlots.Max))
		memory = append(memory, fmt.Sprintf("maxmem=%db", vmSpec.Guest.MemorySlotSize.Value()*int64(*vmSpec.Guest.MemorySlots.Max)))
	}

	// prepare qemu command line
	qemuCmd := []string{
		"-enable-kvm",
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

	// cpu details
	qemuCmd = append(qemuCmd, "-cpu", "host")
	qemuCmd = append(qemuCmd, "-smp", strings.Join(cpus, ","))

	// memory details
	qemuCmd = append(qemuCmd, "-m", strings.Join(memory, ","))

	// net details
	macDefault, err := defaultNetwork(defaultNetworkCIDR)
	if err != nil {
		log.Fatalf("can not setup default network: %s", err)
	}
	qemuCmd = append(qemuCmd, "-netdev", fmt.Sprintf("tap,id=default,ifname=%s,script=no,downscript=no,vhost=on", defaultNetworkTapName))
	qemuCmd = append(qemuCmd, "-device", fmt.Sprintf("virtio-net-pci,netdev=default,mac=%s", macDefault.String()))

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

func defaultNetwork(cidr string) (mac.MAC, error) {
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

	// get dns details from /etc/resolv.conf
	resolvConf, err := resolvconf.Get()
	if err != nil {
		return nil, err
	}
	dns := resolvconf.GetNameservers(resolvConf.Content)[0]
	dnsSearch := strings.Join(resolvconf.GetSearchDomains(resolvConf.Content), ",")

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
