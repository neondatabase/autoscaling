package ipam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/semaphore"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	neonvm "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
)

const (
	UnnamedNetwork string = ""

	// kubernetes client-go rate limiter settings
	// https://pkg.go.dev/k8s.io/client-go@v0.27.2/rest#Config
	KubernetesClientQPS   = 100
	KubernetesClientBurst = 200

	// RequestTimeout for IPAM queries
	IpamRequestTimeout = 10 * time.Second

	// QuarantinePeriod is the period that we quarantine IPs for.
	QuarantinePeriod = 300 * time.Second
)

var ErrAgain = errors.New("IPAM concurrency limit reached. Try again later.")

type IPAM struct {
	Client
	Config  IPAMConfig
	metrics *IPAMMetrics

	clock              func() time.Time
	mu                 sync.Mutex
	concurrencyLimiter *semaphore.Weighted
}

type IPAMParams struct {
	NadName          string
	NadNamespace     string
	ConcurrencyLimit int
	MetricsReg       prometheus.Registerer
	Clock            func() time.Time
}

func (i *IPAM) AcquireIP(ctx context.Context, vmName types.NamespacedName) (net.IPNet, error) {
	ip, err := i.runIPAMWithMetrics(ctx, IPAMAcquire, vmName.String())
	if err != nil {
		return net.IPNet{}, fmt.Errorf("failed to acquire IP: %w", err)
	}
	return ip, nil
}

func (i *IPAM) ReleaseIP(ctx context.Context, vmName types.NamespacedName) (net.IPNet, error) {
	ip, err := i.runIPAMWithMetrics(ctx, IPAMRelease, vmName.String())
	if err != nil {
		return net.IPNet{}, fmt.Errorf("failed to release IP: %w", err)
	}
	return ip, nil
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
	return NewWithClient(kClient, params)
}

func NewWithClient(kClient *Client, params IPAMParams) (*IPAM, error) {
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
		return nil, fmt.Errorf("network-attachment-definition %s has not IP ranges", nad.Name)
	}

	return &IPAM{
		Config:             *ipamConfig,
		Client:             *kClient,
		metrics:            NewIPAMMetrics(params.MetricsReg),
		mu:                 sync.Mutex{},
		concurrencyLimiter: semaphore.NewWeighted(int64(params.ConcurrencyLimit)),
		clock:              params.Clock,
	}, nil
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
		_, ipNet, err := net.ParseCIDR(rangeConfig.Range)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %s: %w", rangeConfig.Range, err)
		}
		rangeConfig.Range = ipNet.String()

		if len(rangeConfig.RangeStart) == 0 {
			rangeConfig.RangeStart = make([]byte, len(ipNet.IP))
			copy(rangeConfig.RangeStart, ipNet.IP)
		}
		if !ipNet.Contains(rangeConfig.RangeStart) {
			return nil, fmt.Errorf("range_start IP %s not in IP Range %s",
				rangeConfig.RangeStart.String(),
				rangeConfig.Range)
		}

		if len(rangeConfig.RangeEnd) == 0 {
			rangeConfig.RangeEnd = lastIP(ipNet)
		}
		if !ipNet.Contains(rangeConfig.RangeEnd) {
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

func (i *IPAM) runIPAMWithMetrics(ctx context.Context, action IPAMAction, vmName string) (net.IPNet, error) {
	timer := i.metrics.StartTimer(string(action))
	// This is if we get a panic
	defer timer.Finish(IPAMPanic)

	ip, err := i.runIPAM(ctx, action, vmName)
	if err != nil {
		timer.Finish(IPAMFailure)
	} else {
		timer.Finish(IPAMSuccess)
	}
	return ip, err
}

// Performing IPAM actions
func (i *IPAM) runIPAM(ctx context.Context, action IPAMAction, vmName string) (net.IPNet, error) {
	var err error
	var ip net.IPNet
	log := log.FromContext(ctx)

	// We have a semaphore to limit the number of concurrent IPAM requests.
	// Note that we use TryAcquire(), so we release the current reconcilliation worker
	// from waiting on a mutex below.
	ok := i.concurrencyLimiter.TryAcquire(1)
	if !ok {
		return net.IPNet{}, ErrAgain
	}
	defer i.concurrencyLimiter.Release(1)

	// We still want to access the IPPool one VM at a time.
	i.mu.Lock()
	defer i.mu.Unlock()

	// Now that we have the lock, maybe the context was canceled.
	select {
	case <-ctx.Done():
		return net.IPNet{}, ctx.Err()
	default:
	}

	ctx, ctxCancel := context.WithTimeout(ctx, IpamRequestTimeout)
	defer ctxCancel()

	// handle the ip add/del until successful
	for _, ipRange := range i.Config.IPRanges {
		// retry loop used to retry CRUD operations against Kubernetes
		// if we meet some issue then just do another attepmt
		ip, err = i.runIPAMRange(ctx, ipRange, action, vmName)
		// break ipRanges loop if ip was acquired/released
		if err == nil {
			return ip, nil
		}
		log.Error(err, "error acquiring/releasing IP from range", ipRange.Range)
	}
	return net.IPNet{}, err
}

func (i *IPAM) runIPAMRange(ctx context.Context, ipRange RangeConfiguration, action IPAMAction, vmName string) (net.IPNet, error) {
	// read IPPool from ipppols.vm.neon.tech custom resource
	pool, err := i.getNeonvmIPPool(ctx, ipRange)
	if err != nil {
		return net.IPNet{}, fmt.Errorf("error reading IP pool: %w", err)
	}

	var ip net.IPNet

	switch action {
	case IPAMAcquire:
		ip, err = pool.allocate(vmName)
		if err != nil {
			return net.IPNet{}, fmt.Errorf("error allocating IP: %w", err)
		}
	case IPAMRelease:
		ip, err = pool.release(vmName)
		if err != nil {
			return net.IPNet{}, fmt.Errorf("error releasing IP: %w", err)
		}
	}

	err = pool.Commit(ctx)
	if err != nil {
		return net.IPNet{}, fmt.Errorf("error updating IP pool: %w", err)
	}
	i.metrics.PoolChanged(pool.pool)
	return ip, nil
}

// Status do List() request to check NeonVM client connectivity
func (i *IPAM) Status(ctx context.Context) error {
	_, err := i.VMClient.NeonvmV1().IPPools(i.Config.NetworkNamespace).List(ctx, metav1.ListOptions{})
	return err
}

// TODO: think about
func (i *IPAM) Close() error {
	return nil
}

// NeonvmIPPool represents an IPPool resource and its parsed set of allocations
type NeonvmIPPool struct {
	vmClient neonvm.Interface
	pool     *vmv1.IPPool
	ipRange  RangeConfiguration
	ipNet    *net.IPNet

	clock func() time.Time
}

func (p *NeonvmIPPool) allocate(vmName string) (net.IPNet, error) {
	p.advanceQuarantine()

	offset := p.find(vmName)
	if offset != "" {
		return p.offsetStringFormat(offset)
	}

	// Note that we use p.ipRange.RangeStart rather than p.baseIP,
	startOffset := p.offsetParse(p.ipRange.RangeStart)
	endOffset := p.offsetParse(p.ipRange.RangeEnd)

	for offset := startOffset; offset <= endOffset; offset++ {
		offsetStr := fmt.Sprintf("%d", offset)
		if _, ok := p.pool.Spec.Allocations[offsetStr]; ok {
			continue
		}
		p.pool.Spec.Allocations[offsetStr] = vmv1.IPAllocation{
			OwnerID: vmName,
		}

		return p.offsetFormat(offset), nil
	}

	return net.IPNet{}, fmt.Errorf("no free IP in the pool: %s", p.ipRange.Range)
}

func (p *NeonvmIPPool) find(vmName string) string {
	for offset, value := range p.pool.Spec.Allocations {
		if value.OwnerID == vmName {
			return offset
		}
	}
	return ""
}

func ipSub(ip1, ip2 net.IP) uint64 {
	b1 := big.NewInt(0).SetBytes(ip1.To16())
	b2 := big.NewInt(0).SetBytes(ip2.To16())
	var result big.Int
	result.Sub(b1, b2)
	return result.Uint64()
}

func ipAdd(ip net.IP, offset uint64) net.IP {
	b1 := big.NewInt(0).SetBytes(ip.To16())
	b2 := big.NewInt(0).SetUint64(offset)
	var resultInt big.Int
	resultInt.Add(b1, b2)
	result := make(net.IP, 16)
	resultInt.FillBytes(result)
	return result
}

func (p *NeonvmIPPool) offsetParse(ip net.IP) uint64 {
	return ipSub(ip, p.ipNet.IP)
}

func (p *NeonvmIPPool) offsetStringFormat(offset string) (net.IPNet, error) {
	offsetInt, err := strconv.ParseUint(offset, 10, 64)
	if err != nil {
		return net.IPNet{}, err
	}
	return p.offsetFormat(offsetInt), nil
}

func (p *NeonvmIPPool) offsetFormat(offset uint64) net.IPNet {
	return net.IPNet{
		IP:   ipAdd(p.ipNet.IP, offset),
		Mask: p.ipNet.Mask,
	}
}

func (p *NeonvmIPPool) release(vmName string) (net.IPNet, error) {
	offset := p.find(vmName)
	if offset == "" {
		// IP was already freed
		return net.IPNet{
			IP:   nil,
			Mask: nil,
		}, nil
	}
	p.pool.Spec.Allocations[offset] = vmv1.IPAllocation{
		OwnerID: "quarantined",
	}
	p.pool.Spec.QuarantinedOffsetsPending = append(p.pool.Spec.QuarantinedOffsetsPending, offset)
	p.advanceQuarantine()
	return p.offsetStringFormat(offset)
}

func (p *NeonvmIPPool) advanceQuarantine() {
	now := p.clock().UTC()
	if p.pool.Spec.QuarantineStartTime.IsZero() {
		p.pool.Spec.QuarantineStartTime = metav1.NewTime(now)
	}
	if now.Sub(p.pool.Spec.QuarantineStartTime.Time) < QuarantinePeriod {
		return
	}

	// Clear QuarantinedOffsets - for them quarantine is over
	for _, offset := range p.pool.Spec.QuarantinedOffsets {
		delete(p.pool.Spec.Allocations, offset)
	}

	// Start new quarantine period
	p.pool.Spec.QuarantinedOffsets = p.pool.Spec.QuarantinedOffsetsPending
	p.pool.Spec.QuarantinedOffsetsPending = []string{}
	p.pool.Spec.QuarantineStartTime = metav1.NewTime(now)
}

// getNeonvmIPPool returns a NeonVM IPPool for the given IP range
func (i *IPAM) getNeonvmIPPool(ctx context.Context, ipRange RangeConfiguration) (*NeonvmIPPool, error) {
	// for IP range 10.11.22.0/24 poll name will be
	// "10.11.22.0-24" if no network name in ipam spec, or
	// "samplenet-10.11.22.0-24" if nametwork name is `samplenet`
	var poolName string
	if i.Config.NetworkName == UnnamedNetwork {
		poolName = strings.ReplaceAll(ipRange.Range, "/", "-")
	} else {
		poolName = fmt.Sprintf("%s-%s", i.Config.NetworkName, strings.ReplaceAll(ipRange.Range, "/", "-"))
	}

	pool, err := i.VMClient.NeonvmV1().IPPools(i.Config.NetworkNamespace).Get(ctx, poolName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("error getting IP pool: %w", err)
		}
		// pool does not exist, create it
		newPool := &vmv1.IPPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      poolName,
				Namespace: i.Config.NetworkNamespace,
			},
			Spec: vmv1.IPPoolSpec{
				Range:                     ipRange.Range,
				Allocations:               make(map[string]vmv1.IPAllocation),
				QuarantinedOffsets:        nil,
				QuarantinedOffsetsPending: nil,
				QuarantineStartTime:       metav1.NewTime(i.clock().UTC()),
			},
		}
		ipPool, err := i.VMClient.NeonvmV1().IPPools(i.Config.NetworkNamespace).Create(ctx, newPool, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("error creating IP pool: %w", err)
		}
		pool = ipPool
	}

	// get first IP in the pool
	_, ipNet, err := net.ParseCIDR(pool.Spec.Range)
	if err != nil {
		return nil, err
	}

	return &NeonvmIPPool{
		clock:    i.clock,
		vmClient: i.Client.VMClient,
		pool:     pool,
		ipRange:  ipRange,
		ipNet:    ipNet,
	}, nil
}

func (p *NeonvmIPPool) Commit(ctx context.Context) error {
	_, err := p.vmClient.NeonvmV1().IPPools(p.pool.Namespace).Update(ctx, p.pool, metav1.UpdateOptions{})
	return err
}
