package ipam

import (
	"encoding/json"
	"fmt"
	"net"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/prometheus/client_golang/prometheus"
)

// IPAMParams is passed from neonvm-controller to setup IPAM.
type IPAMParams struct {
	NadName      string
	NadNamespace string
	MetricsReg   prometheus.Registerer
}

type Nad struct {
	IPAM *IPAMConfig `json:"ipam"`
}

// IPAMConfig describes the expected json configuration for this plugin
type IPAMConfig struct {
	Routes           []*cnitypes.Route    `json:"routes"`
	IPRanges         []RangeConfiguration `json:"ipRanges"`
	NetworkNamespace string
	NetworkName      string             `json:"network_name,omitempty"`
	ManagerConfig    *IPAMManagerConfig `json:"manager_config,omitempty"`
}

type RangeConfiguration struct {
	OmitRanges []string `json:"exclude,omitempty"`
	Range      string   `json:"range"`
	RangeStart net.IP   `json:"range_start,omitempty"`
	RangeEnd   net.IP   `json:"range_end,omitempty"`
}

func loadFromNad(nadConfig string, nadNamespace string) (*IPAMConfig, error) {
	var n Nad
	if err := json.Unmarshal([]byte(nadConfig), &n); err != nil {
		return nil, fmt.Errorf("json parsing error: %w", err)
	}

	if n.IPAM == nil {
		return nil, fmt.Errorf("missing 'ipam' key")
	}

	// check IP ranges
	for idx, rangeConfig := range n.IPAM.IPRanges {
		firstip, ipNet, err := net.ParseCIDR(rangeConfig.Range)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %s: %w", rangeConfig.Range, err)
		}
		rangeConfig.Range = ipNet.String()
		if rangeConfig.RangeStart == nil {
			firstip = net.ParseIP(firstip.Mask(ipNet.Mask).String()) // get real first IP from cidr
			rangeConfig.RangeStart = firstip
		}
		if rangeConfig.RangeStart != nil && !ipNet.Contains(rangeConfig.RangeStart) {
			return nil, fmt.Errorf("range_start IP %s not in IP Range %s",
				rangeConfig.RangeStart.String(),
				rangeConfig.Range)
		}
		if rangeConfig.RangeEnd != nil && !ipNet.Contains(rangeConfig.RangeEnd) {
			return nil, fmt.Errorf("range_end IP %s not in IP Range %s",
				rangeConfig.RangeEnd.String(),
				rangeConfig.Range)
		}

		n.IPAM.IPRanges[idx] = rangeConfig
	}

	// set network namespace
	n.IPAM.NetworkNamespace = nadNamespace

	return n.IPAM, nil
}
