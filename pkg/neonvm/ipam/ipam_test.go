package ipam_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kfake "k8s.io/client-go/kubernetes/fake"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	nfake "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned/fake"
	"github.com/neondatabase/autoscaling/pkg/neonvm/ipam"
	"github.com/neondatabase/autoscaling/pkg/util/taskgroup"
)

func makeIPAM(t *testing.T, cfg string) *ipam.IPAM {
	client := ipam.Client{
		KubeClient: kfake.NewSimpleClientset(),
		VMClient:   nfake.NewSimpleClientset(),
		NADClient:  nadfake.NewSimpleClientset(),
	}
	_, err := client.NADClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("default").Create(context.Background(), &nadv1.NetworkAttachmentDefinition{
		TypeMeta: metav1.TypeMeta{
			Kind:       "NetworkAttachmentDefinition",
			APIVersion: "k8s.cni.cncf.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "nad",
		},
		Spec: nadv1.NetworkAttachmentDefinitionSpec{
			Config: fmt.Sprintf(`{"ipam":%s}`, cfg),
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	ipam, err := ipam.NewWithClient(&client, "nad", "default", 1)
	require.NoError(t, err)

	return ipam
}

func TestIPAM(t *testing.T) {
	ipam := makeIPAM(t,
		`{
			"ipRanges": [
				{
					"range":"10.100.123.0/24",
					"range_start":"10.100.123.1",
					"range_end":"10.100.123.254",
					"network_name":"nad"
				}
			]
		}`,
	)

	defer ipam.Close()

	name := types.NamespacedName{
		Namespace: "default",
		Name:      "vm",
	}

	ip1, err := ipam.AcquireIP(context.Background(), name)
	require.NoError(t, err)
	require.NotNil(t, ip1)
	assert.Equal(t, "10.100.123.1/24", ip1.String())

	// Same VM - same IP
	ipResult, err := ipam.AcquireIP(context.Background(), name)
	require.NoError(t, err)
	require.NotNil(t, ipResult)
	assert.Equal(t, ip1, ipResult)

	// Different VM - different IP
	name.Name = "vm2"
	ip2, err := ipam.AcquireIP(context.Background(), name)
	require.NoError(t, err)
	require.NotNil(t, ip2)
	assert.Equal(t, "10.100.123.2/24", ip2.String())

	// Release the second IP
	ipResult, err = ipam.ReleaseIP(context.Background(), name)
	require.NoError(t, err)
	require.Equal(t, ip2, ipResult)

	// Allocate it again
	name.Name = "vm3"
	ip3, err := ipam.AcquireIP(context.Background(), name)
	require.NoError(t, err)
	require.NotNil(t, ip3)
	assert.Equal(t, ip2, ip3)
}

func TestIPAMCleanup(t *testing.T) {
	ipam := makeIPAM(t,
		`{
			"ipRanges": [
				{
					"range":"10.100.123.0/24",
					"range_start":"10.100.123.1",
					"range_end":"10.100.123.254"
				}
			],
			"network_name":"nadNetworkName"
		}`,
	)

	defer ipam.Close()

	name := types.NamespacedName{
		Namespace: "default",
		Name:      "vm",
	}

	ip, err := ipam.AcquireIP(context.Background(), name)
	require.NoError(t, err)
	require.NotNil(t, ip)

	lst, err := ipam.Client.VMClient.NeonvmV1().IPPools("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.NotNil(t, lst)

	getIPPool := func() *vmv1.IPPool {
		ipPool, err := ipam.Client.VMClient.NeonvmV1().IPPools("default").Get(context.Background(), "nadNetworkName-10.100.123.0-24", metav1.GetOptions{})
		require.NoError(t, err)
		require.NotNil(t, ipPool)
		return ipPool
	}

	ipPool := getIPPool()

	assert.Equal(t, ipPool.Spec.Range, "10.100.123.0/24")
	assert.Equal(t, map[string]vmv1.IPAllocation{
		// IP offset: allocation
		"1": {
			ContainerID: "default/vm",
			PodRef:      "",
		},
	}, ipPool.Spec.Allocations)

	name2 := types.NamespacedName{
		Namespace: "default",
		Name:      "vm2",
	}
	ip2, err := ipam.AcquireIP(context.Background(), name2)
	require.NoError(t, err)
	require.NotNil(t, ip2)
	assert.Equal(t, ip2.String(), "10.100.123.2/24")

	// Let's create only the second VM
	// The cleanup will release the first IP, but will keep the second
	result, err := ipam.Client.VMClient.NeonvmV1().VirtualMachines("default").Create(context.Background(), &vmv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vm2",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NotNil(t, result)

	ipPool = getIPPool()
	assert.Len(t, ipPool.Spec.Allocations, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tg := taskgroup.NewGroup(zap.NewNop())

	defer tg.Wait() //nolint:errcheck // always nil

	tg.Go("cleanup", func(logger *zap.Logger) error {
		err := ipam.Cleaner("default")(ctx)
		require.ErrorContains(t, err, "context canceled")
		return nil
	})

	for i := 0; i < 100; i++ {
		ipPool = getIPPool()
		if len(ipPool.Spec.Allocations) == 1 {
			// Test succeeded
			assert.Equal(t, ipPool.Spec.Allocations["2"].ContainerID, "default/vm2")
			cancel()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.Fail(t, "cleanup did not finish", ipPool.Spec.Allocations)
}
