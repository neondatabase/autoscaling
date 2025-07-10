package ipam_test

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/go-logr/logr/testr"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	nfake "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned/fake"
	"github.com/neondatabase/autoscaling/pkg/neonvm/ipam"
)

type testParams struct {
	prom *prometheus.Registry
	ipam *ipam.IPAM
}

func makeIPAM(t *testing.T, cfg string) *testParams {
	client := ipam.Client{
		VMClient:  nfake.NewSimpleClientset(),
		NADClient: nadfake.NewSimpleClientset(),
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

	prom := prometheus.NewRegistry()
	ipam, err := ipam.FromClient(&client, ipam.IPAMParams{
		NADName:      "nad",
		NADNamespace: "default",
		MetricsReg:   prom,
	})

	require.NoError(t, err)

	return &testParams{
		prom: prom,
		ipam: ipam,
	}
}

const DefaultIPAMConfig = `
{
	"ipRanges": [
		{
			"range":"10.100.123.0/24",
			"range_start":"10.100.123.1",
			"range_end":"10.100.123.254",
			"network_name":"nad"
		}
	],
	"manager": {
		"cooldown_period": "60s",
		"high_ip_count": 10,
		"low_ip_count": 5,
		"target_ip_count": 7
	}
}`

type metricValue struct {
	Name    string
	Action  string
	Outcome string
	Value   float64
}

func collectMetrics(t *testing.T, reg *prometheus.Registry) []metricValue {
	metrics, err := reg.Gather()
	require.NoError(t, err)

	var result []metricValue

	// Check duration metrics
	for _, metric := range metrics {
		for _, m := range metric.GetMetric() {
			var action, outcome string

			for _, label := range m.GetLabel() {
				if label.GetName() == "action" {
					action = label.GetValue()
				}
				if label.GetName() == "outcome" {
					outcome = label.GetValue()
				}
			}
			var value float64
			if m.GetHistogram() != nil {
				value = float64(m.GetHistogram().GetSampleCount())
			} else if m.GetGauge() != nil {
				value = m.GetGauge().GetValue()
			}

			result = append(result, metricValue{
				Name:    metric.GetName(),
				Action:  action,
				Outcome: outcome,
				Value:   value,
			})
		}
	}

	return result
}

func ctx(t *testing.T) context.Context {
	return log.IntoContext(context.Background(), testr.New(t))
}

func TestIPAM(t *testing.T) {
	params := makeIPAM(t, DefaultIPAMConfig)
	ipam := params.ipam

	name := types.UID("vm1")

	ip1, err := ipam.AcquireIP(ctx(t), name)
	require.NoError(t, err)
	require.NotNil(t, ip1)
	assert.Equal(t, "10.100.123.1/24", ip1.String())

	// Same VM - same IP
	ipResult, err := ipam.AcquireIP(ctx(t), name)
	require.NoError(t, err)
	require.NotNil(t, ipResult)
	assert.Equal(t, ip1, ipResult)

	// Different VM - different IP
	name = types.UID("vm2")
	ip2, err := ipam.AcquireIP(ctx(t), name)
	require.NoError(t, err)
	require.NotNil(t, ip2)
	assert.Equal(t, "10.100.123.2/24", ip2.String())

	// Release the second IP
	ipam.ReleaseIP(ctx(t), name, net.ParseIP("10.100.123.2"))

	// Allocate one more IP
	name = types.UID("vm3")
	ip3, err := ipam.AcquireIP(ctx(t), name)
	require.NoError(t, err)
	require.NotNil(t, ip3)
	assert.Equal(t, "10.100.123.3/24", ip3.String())

	metrics := collectMetrics(t, params.prom)
	assert.Subset(t, metrics, []metricValue{
		{Name: "ipam_request_duration_seconds", Action: "acquire", Outcome: "success", Value: 4},
		{Name: "ipam_request_duration_seconds", Action: "release", Outcome: "success", Value: 1},
		{Name: "ipam_ongoing_requests", Action: "acquire", Outcome: "", Value: 0},
		{Name: "ipam_ongoing_requests", Action: "release", Outcome: "", Value: 0},
	})
}

func TestIPAMMetricsOnError(t *testing.T) {
	params := makeIPAM(t, DefaultIPAMConfig)
	ipam := params.ipam

	// Create a context that's already canceled to force an error
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	name := types.UID("vm1")

	// This should fail because the context is already canceled
	_, err := ipam.AcquireIP(ctx, name)
	require.Error(t, err)

	metrics := collectMetrics(t, params.prom)
	assert.Subset(t, metrics, []metricValue{
		{Name: "ipam_request_duration_seconds", Action: "acquire", Outcome: "failure", Value: 1},
		{Name: "ipam_ongoing_requests", Action: "acquire", Outcome: "", Value: 0},
	})
}
