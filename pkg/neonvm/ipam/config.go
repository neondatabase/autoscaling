package ipam

import (
	"encoding/json"
	"fmt"
	"iter"
	"net"
	"net/netip"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/prometheus/client_golang/prometheus"
)

// IPAMParams is passed from neonvm-controller to setup IPAM.
type IPAMParams struct {
	NADName      string
	NADNamespace string
	MetricsReg   prometheus.Registerer
}

// NAD comes from the network-attachment-definition CRD.
type NAD struct {
	IPAM *IPAMConfig `json:"ipam"`
}

type IPAMConfig struct {
	Routes        []*cnitypes.Route    `json:"routes"`
	IPRanges      []RangeConfiguration `json:"ipRanges"`
	NetworkName   string               `json:"network_name,omitempty"`
	ManagerConfig *IPAMManagerConfig   `json:"manager,omitempty"`
}

type RangeConfiguration struct {
	OmitRanges []string `json:"exclude,omitempty"`
	Range      string   `json:"range"`
	RangeStart net.IP   `json:"range_start,omitempty"`
	RangeEnd   net.IP   `json:"range_end,omitempty"`

	ipNet        *net.IPNet
	start4, end4 netip.Addr
}

func (r *RangeConfiguration) Normalize() error {
	firstip, ipNet, err := net.ParseCIDR(r.Range)
	if err != nil {
		return fmt.Errorf("invalid CIDR %s: %w", r.Range, err)
	}
	r.ipNet = ipNet

	r.Range = ipNet.String()
	if r.RangeStart == nil {
		firstip = net.ParseIP(firstip.Mask(ipNet.Mask).String()) // get real first IP from cidr
		r.RangeStart = firstip
	}
	if !ipNet.Contains(r.RangeStart) {
		return fmt.Errorf("range_start IP %s not in IP Range %s",
			r.RangeStart.String(),
			r.Range)
	}

	if r.RangeEnd == nil {
		r.RangeEnd = lastIP(ipNet)
	}
	if !ipNet.Contains(r.RangeEnd) {
		return fmt.Errorf("range_end IP %s not in IP Range %s",
			r.RangeEnd.String(),
			r.Range)
	}

	var ok bool
	r.start4, ok = netip.AddrFromSlice(r.RangeStart.To4())
	if !ok {
		return fmt.Errorf("IP %s is not IPv4", r.RangeStart.String())
	}
	r.end4, ok = netip.AddrFromSlice(r.RangeEnd.To4())
	if !ok {
		return fmt.Errorf("IP %s is not IPv4", r.RangeEnd.String())
	}

	return nil
}

func (r *RangeConfiguration) AllIPs() iter.Seq[netip.Addr] {
	return func(yield func(netip.Addr) bool) {
		ip := r.start4
		for {
			if !yield(ip) {
				return
			}

			ip = ip.Next()
			if r.end4.Less(ip) {
				// last IP is reached
				return
			}
		}
	}
}

func lastIP(ipNet *net.IPNet) net.IP {
	ip := ipNet.IP
	mask := ipNet.Mask
	if ip.To4() != nil {
		ip = ip.To4()
		mask = net.IPMask(net.IP(mask).To4())

	}
	lastIP := make(net.IP, len(ip))

	// ~mask has ones in places which would be variable in the subnet
	// so we OR it with the start IP to get the end of the range
	for i := range ip {
		lastIP[i] = ip[i] | ^mask[i]
	}
	return lastIP
}

func loadFromNad(nadConfig string) (*IPAMConfig, error) {
	var n NAD
	if err := json.Unmarshal([]byte(nadConfig), &n); err != nil {
		return nil, fmt.Errorf("json parsing error: %w", err)
	}

	if n.IPAM == nil {
		return nil, fmt.Errorf("missing 'ipam' key")
	}

	return n.IPAM, nil
}
