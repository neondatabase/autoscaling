package ipam

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	neonvm "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
)

type PoolClient struct {
	vmClient    neonvm.Interface
	rangeConfig *RangeConfiguration
	networkName string
	namespace   string
	metrics     *IPAMMetrics
}

func NewPoolClient(vmClient neonvm.Interface, rangeConfig *RangeConfiguration, networkName string, namespace string, metrics *IPAMMetrics) (*PoolClient, error) {
	c := &PoolClient{
		vmClient:    vmClient,
		rangeConfig: rangeConfig,
		networkName: networkName,
		namespace:   namespace,
		metrics:     metrics,
	}

	metrics.poolIPCount.WithLabelValues(c.PoolName(), "total").Set(float64(c.rangeConfig.IPCountTotal()))

	return c, nil
}

func (c *PoolClient) PoolName() string {
	// for IP range 10.11.22.0/24 poll name will be
	// "10.11.22.0-24" if no network name in ipam spec, or
	// "samplenet-10.11.22.0-24" if nametwork name is `samplenet`

	slug := strings.ReplaceAll(c.rangeConfig.Range, "/", "-")
	if c.networkName == UnnamedNetwork {
		return slug
	}

	return fmt.Sprintf("%s-%s", c.networkName, slug)
}

func (c *PoolClient) Get(ctx context.Context) (*vmv1.IPPool, error) {
	pool, err := c.vmClient.NeonvmV1().IPPools(c.namespace).Get(ctx, c.PoolName(), metav1.GetOptions{})
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
			Namespace: c.namespace,
		},
		Spec: vmv1.IPPoolSpec{
			Range:       c.rangeConfig.Range,
			Managed:     make(map[string]vmv1.Unit),
			Allocations: nil,
		},
	}
	ipPool, err := c.vmClient.NeonvmV1().IPPools(c.namespace).Create(ctx, newPool, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error creating IP pool: %w", err)
	}
	pool = ipPool

	return pool, nil
}

func (c *PoolClient) Commit(ctx context.Context, pool *vmv1.IPPool) error {
	_, err := c.vmClient.NeonvmV1().IPPools(c.namespace).Update(ctx, pool, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating IP pool: %w", err)
	}

	c.metrics.poolIPCount.WithLabelValues(c.PoolName(), "managed").Set(float64(len(pool.Spec.Managed)))

	return err
}
