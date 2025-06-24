package ipam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	UnnamedNetwork string = ""

	// kubernetes client-go rate limiter settings
	// https://pkg.go.dev/k8s.io/client-go@v0.27.2/rest#Config
	KubernetesClientQPS   = 100
	KubernetesClientBurst = 200

	// RequestTimeout for IPAM queries
	IpamRequestTimeout = 10 * time.Second
)

var ErrAgain = errors.New("Try again later.")

type IPAM struct {
	Client
	Config   IPAMConfig
	metrics  *IPAMMetrics
	managers []*Manager
}

type IPAMParams struct {
	NadName      string
	NadNamespace string
	MetricsReg   prometheus.Registerer
}

type ipamAction string

const (
	IPAMAcquire   ipamAction = "acquire"
	IPAMRelease   ipamAction = "release"
	IPAMRebalance ipamAction = "rebalance"
	IPAMSetActive ipamAction = "set-active"
)

// AcquireIP is idempotent - can be called multiple times with the same vmName.
func (i *IPAM) AcquireIP(ctx context.Context, vmName types.NamespacedName) (net.IPNet, error) {
	var ip net.IPNet

	err := i.runIPAMMetered(ctx, IPAMAcquire, func(manager *Manager) error {
		var err error
		ip, err = manager.Allocate(ctx, VMID(vmName))
		return err
	})
	if err != nil {
		return net.IPNet{}, fmt.Errorf("failed to acquire IP: %w", err)
	}
	return ip, nil
}

// ReleaseIP is not idempotent - have to be called exactly once after the VM state was updated.
func (i *IPAM) ReleaseIP(ctx context.Context, vmName types.NamespacedName, ip netip.Addr) {
	err := i.runIPAMMetered(ctx, IPAMRelease, func(manager *Manager) error {
		return manager.Release(ctx, VMID(vmName), ip)
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to release IP", "vmName", vmName, "ip", ip)
		// We can only log the error, because method is not failable.
		// If it ever happens - it means we have a bug.
	}
}

// New returns a new IPAM object with ipam config and k8s/crd clients
func New(params IPAMParams) (*IPAM, error) {
	// get Kubernetes client config
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("error building kubernetes configuration: %w", err)
	}

	// tune Kubernetes client performance
	cfg.QPS = KubernetesClientQPS
	cfg.Burst = KubernetesClientBurst

	kClient, err := NewKubeClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes client: %w", err)
	}
	return FromClient(kClient, params)
}

func FromClient(kClient *Client, params IPAMParams) (*IPAM, error) {
	ctx, cancel := context.WithTimeout(context.Background(), IpamRequestTimeout)
	defer cancel()

	// read network-attachment-definition from Kubernetes
	nad, err := kClient.NADClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(params.NadNamespace).Get(ctx, params.NadName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if len(nad.Spec.Config) == 0 {
		return nil, fmt.Errorf("network-attachment-definition %s hasn't IPAM config section", nad.Name)
	}

	ipamConfig, err := LoadFromNad(nad.Spec.Config, params.NadNamespace)
	if err != nil {
		return nil, fmt.Errorf("network-attachment-definition IPAM config parse error: %w", err)
	}
	if len(ipamConfig.IPRanges) == 0 {
		return nil, fmt.Errorf("network-attachment-definition %s has no IP ranges", nad.Name)
	}

	if ipamConfig.ManagerConfig == nil {
		return nil, fmt.Errorf("network-attachment-definition %s has no manager config", nad.Name)
	}

	var managers []*Manager
	for _, rangeConfig := range ipamConfig.IPRanges {
		poolClient, err := newIPPoolClient(kClient.VMClient, &rangeConfig, ipamConfig)
		if err != nil {
			return nil, fmt.Errorf("error creating pool client: %w", err)
		}

		manager, err := NewManager(ctx, ipamConfig.ManagerConfig, poolClient)
		if err != nil {
			return nil, fmt.Errorf("error creating manager: %w", err)
		}
		managers = append(managers, manager)
	}

	return &IPAM{
		Config:   *ipamConfig,
		Client:   *kClient,
		metrics:  NewIPAMMetrics(params.MetricsReg),
		managers: managers,
	}, nil
}

// Load Network Attachment Definition and parse config to fill IPAM config
func LoadFromNad(nadConfig string, nadNamespace string) (*IPAMConfig, error) {
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

func (i *IPAM) runIPAMMetered(ctx context.Context, action ipamAction, cb func(*Manager) error) error {
	timer := i.metrics.StartTimer(string(action))
	// This is if we get a panic
	defer timer.Finish(IPAMPanic)

	err := i.runIPAM(ctx, action, cb)
	if err != nil {
		timer.Finish(IPAMFailure)
	} else {
		timer.Finish(IPAMSuccess)
	}
	return err
}

// Performing IPAM actions
func (i *IPAM) runIPAM(ctx context.Context, action ipamAction, cb func(*Manager) error) error {
	var errorList []error
	log := log.FromContext(ctx)

	// Try all Managers until success
	for _, manager := range i.managers {

		err := cb(manager)

		if err == nil {
			return nil
		}

		log.Error(err, "ipam action failed", "action", action, "pool", manager.pool.PoolName())
		errorList = append(errorList, err)
	}
	return errors.Join(errorList...)
}
