package main

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/cilium/cilium/pkg/mac"
	"github.com/coreos/go-iptables/iptables"
	"github.com/docker/docker/libnetwork/resolvconf"
	"github.com/docker/libnetwork/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink"
	"go.uber.org/zap"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/util"
)

const (
	defaultNetworkBridgeName = "br-def"
	defaultNetworkTapName    = "tap-def"
	defaultNetworkCIDR       = "169.254.254.252/30"

	overlayNetworkBridgeName = "br-overlay"
	overlayNetworkTapName    = "tap-overlay"

	protocolTCP string = "6"
)

// setupVMNetworks creates the networks for the VM and returns the appropriate QMEU args
func setupVMNetworks(logger *zap.Logger, ports []vmv1.Port, extraNetwork *vmv1.ExtraNetwork) ([]string, error) {
	// Create network tap devices.
	//
	// It is important to enable multiqueue support for virtio-net-pci devices as we seen them choking on
	// traffic and dropping packets. Set queues=4 and vectors=10 as a reasonable default. `vectors` should
	// be to 2*queues + 2 as per https://www.linux-kvm.org/page/Multiqueue. We also enable multiqueue support
	// for all VM sizes, even to a small ones. As of time of writing, it seems to not worth a trouble to
	// dynamically adjust number of queues based on VM size.

	var qemuCmd []string

	// default (pod) net details
	macDefault, err := defaultNetwork(logger, defaultNetworkCIDR, ports)
	if err != nil {
		return nil, fmt.Errorf("failed to set up default network: %w", err)
	}
	qemuCmd = append(qemuCmd, "-netdev", fmt.Sprintf("tap,id=default,ifname=%s,queues=4,script=no,downscript=no,vhost=on", defaultNetworkTapName))
	qemuCmd = append(qemuCmd, "-device", fmt.Sprintf("virtio-net-pci,mq=on,vectors=10,netdev=default,mac=%s", macDefault.String()))

	// overlay (multus) net details
	if extraNetwork != nil && extraNetwork.Enable {
		macOverlay, err := overlayNetwork(extraNetwork.Interface)
		if err != nil {
			return nil, fmt.Errorf("failed to set up overlay network: %w", err)
		}
		qemuCmd = append(qemuCmd, "-netdev", fmt.Sprintf("tap,id=overlay,ifname=%s,queues=4,script=no,downscript=no,vhost=on", overlayNetworkTapName))
		qemuCmd = append(qemuCmd, "-device", fmt.Sprintf("virtio-net-pci,mq=on,vectors=10,netdev=overlay,mac=%s", macOverlay.String()))
	}

	return qemuCmd, nil
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
		Flags: netlink.TUNTAP_MULTI_QUEUE_DEFAULTS,
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
		logger.Debug(fmt.Sprintf("setup DNAT rule for incoming traffic to port %d", port.Port))
		iptablesArgs = []string{
			"-t", "nat", "-A", "PREROUTING",
			"-i", "eth0", "-p", fmt.Sprint(port.Protocol), "--dport", fmt.Sprint(port.Port),
			"-j", "DNAT", "--to", fmt.Sprintf("%s:%d", ipVm.String(), port.Port),
		}
		if err := execFg("iptables", iptablesArgs...); err != nil {
			logger.Error("could not set up DNAT rule for incoming traffic", zap.Error(err))
			return nil, err
		}
		logger.Debug(fmt.Sprintf("setup DNAT rule for traffic originating from localhost to port %d", port.Port))
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
		logger.Debug(fmt.Sprintf("setup ACCEPT rule for traffic originating from localhost to port %d", port.Port))
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
	logger.Debug("setup MASQUERADE rule for traffic originating from localhost")
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
	resolvConf, err := resolvconf.Get()
	if err != nil {
		logger.Error("could not get DNS details", zap.Error(err))
		return nil, err
	}
	dns := resolvconf.GetNameservers(resolvConf.Content, types.IP)[0]
	dnsSearch := strings.Join(resolvconf.GetSearchDomains(resolvConf.Content), ",")

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
		Flags: netlink.TUNTAP_MULTI_QUEUE_DEFAULTS,
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

type NetworkMonitoringMetrics struct {
	IngressBytes, EgressBytes, Errors prometheus.Counter
	IngressBytesRaw, EgressBytesRaw   uint64 // Absolute values to calc increments for Counters
}

func NewMonitoringMetrics(reg *prometheus.Registry) *NetworkMonitoringMetrics {
	m := &NetworkMonitoringMetrics{
		IngressBytes: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "runner_vm_ingress_bytes_total",
				Help: "Number of bytes received by the VM from the open internet",
			},
		)),
		EgressBytes: util.RegisterMetric(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "runner_vm_egress_bytes_total",
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
