package ipam_test

import (
	"context"
	"fmt"
	"testing"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kfake "k8s.io/client-go/kubernetes/fake"

	nfake "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned/fake"
	"github.com/neondatabase/autoscaling/pkg/neonvm/ipam"
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

	ipam, err := ipam.NewWithClient(&client, "nad", "default")
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
