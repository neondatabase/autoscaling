package ipam

import (
	"net"

	cnitypes "github.com/containernetworking/cni/pkg/types"
)

type RangeConfiguration struct {
	OmitRanges []string `json:"exclude,omitempty"`
	Range      string   `json:"range"`
	RangeStart net.IP   `json:"range_start,omitempty"`
	RangeEnd   net.IP   `json:"range_end,omitempty"`
}

type Nad struct {
	IPAM *IPAMConfig `json:"ipam"`
}

// IPAMConfig describes the expected json configuration for this plugin
type IPAMConfig struct {
	Routes           []*cnitypes.Route    `json:"routes"`
	IPRanges         []RangeConfiguration `json:"ipRanges"`
	OmitRanges       []string             `json:"exclude,omitempty"`
	DNS              cnitypes.DNS         `json:"dns"`
	Range            string               `json:"range"`
	RangeStart       net.IP               `json:"range_start,omitempty"`
	RangeEnd         net.IP               `json:"range_end,omitempty"`
	NetworkNamespace string
	NetworkName      string `json:"network_name,omitempty"`
}
