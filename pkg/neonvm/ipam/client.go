package ipam

import (
	nad "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	neonvm "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
)

// Set of kubernetets clients
type Client struct {
	KubeClient kubernetes.Interface
	VMClient   neonvm.Interface
	NADClient  nad.Interface
}

func NewKubeClient(cfg *rest.Config) (*Client, error) {
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	vmClient, err := neonvm.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	nadClient, err := nad.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &Client{
		KubeClient: kubeClient,
		VMClient:   vmClient,
		NADClient:  nadClient,
	}, nil
}
