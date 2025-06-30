package ipam

import (
	"context"
	"fmt"
	"iter"
	"net"
	"net/netip"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	neonvm "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
)

type ipPoolClient struct {
	vmClient    neonvm.Interface
	rangeConfig RangeConfiguration
	ipamConfig  *IPAMConfig

	ipnet *net.IPNet
	first netip.Addr
	last  netip.Addr
}

func newIPPoolClient(vmClient neonvm.Interface, rangeConfig *RangeConfiguration, ipamConfig *IPAMConfig) (*ipPoolClient, error) {
	_, ipnet, err := net.ParseCIDR(rangeConfig.Range)
	if err != nil {
		return nil, fmt.Errorf("error parsing IP range: %w", err)
	}

	first4 := rangeConfig.RangeStart.To4()
	if first4 == nil {
		return nil, fmt.Errorf("IP range is not IPv4")
	}
	first, _ := netip.AddrFromSlice(first4)

	last4 := rangeConfig.RangeEnd.To4()
	if last4 == nil {
		return nil, fmt.Errorf("IP range is not IPv4")
	}
	last, _ := netip.AddrFromSlice(last4)

	return &ipPoolClient{
		vmClient:    vmClient,
		rangeConfig: *rangeConfig,
		ipamConfig:  ipamConfig,

		ipnet: ipnet,
		first: first,
		last:  last,
	}, nil
}

func (c *ipPoolClient) PoolName() string {
	// for IP range 10.11.22.0/24 poll name will be
	// "10.11.22.0-24" if no network name in ipam spec, or
	// "samplenet-10.11.22.0-24" if nametwork name is `samplenet`

	slug := strings.ReplaceAll(c.rangeConfig.Range, "/", "-")
	if c.ipamConfig.NetworkName == UnnamedNetwork {
		return slug
	}

	return fmt.Sprintf("%s-%s", c.ipamConfig.NetworkName, slug)
}

func (c *ipPoolClient) Get(ctx context.Context) (*vmv1.IPPool, error) {
	pool, err := c.vmClient.NeonvmV1().IPPools(c.ipamConfig.NetworkNamespace).Get(ctx, c.PoolName(), metav1.GetOptions{})
	if err == nil {
		return pool, nil
	}

	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("error getting IP pool: %w", err)
	}

	// pool does not exist, create it
	newPool := &vmv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.PoolName(),
			Namespace: c.ipamConfig.NetworkNamespace,
		},
		Spec: vmv1.IPPoolSpec{
			Range:       c.rangeConfig.Range,
			Managed:     make(map[string]vmv1.Unit),
			Allocations: nil,
		},
	}
	ipPool, err := c.vmClient.NeonvmV1().IPPools(c.ipamConfig.NetworkNamespace).Create(ctx, newPool, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error creating IP pool: %w", err)
	}
	pool = ipPool

	return pool, nil
}

func (c *ipPoolClient) Commit(ctx context.Context, pool *vmv1.IPPool) error {
	_, err := c.vmClient.NeonvmV1().IPPools(c.ipamConfig.NetworkNamespace).Update(ctx, pool, metav1.UpdateOptions{})
	return err
}

func (c *ipPoolClient) AllIPs() iter.Seq[netip.Addr] {
	return func(yield func(netip.Addr) bool) {
		ip := c.first
		for {
			if !yield(ip) {
				return
			}

			ip = ip.Next()
			if c.last.Less(ip) {
				// last IP is reached
				return
			}
		}
	}
}
