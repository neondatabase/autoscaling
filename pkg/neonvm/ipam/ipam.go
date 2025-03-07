package ipam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	whereaboutsallocate "github.com/k8snetworkplumbingwg/whereabouts/pkg/allocate"
	whereaboutstypes "github.com/k8snetworkplumbingwg/whereabouts/pkg/types"
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

	// DatastoreRetries defines how many retries are attempted when reading/updating the IP Pool
	DatastoreRetries      = 5
	DatastoreRetriesDelay = 100 * time.Millisecond
)

type Temporary interface {
	Temporary() bool
}

type IPAM struct {
	Client
	Config IPAMConfig

	mu sync.Mutex
}

func (i *IPAM) AcquireIP(ctx context.Context, vmName types.NamespacedName) (net.IPNet, error) {
	ip, err := i.runIPAM(ctx, makeAcquireAction(vmName))
	if err != nil {
		return net.IPNet{}, fmt.Errorf("failed to acquire IP: %w", err)
	}
	return ip, nil
}

func (i *IPAM) ReleaseIP(ctx context.Context, vmName types.NamespacedName) (net.IPNet, error) {
	ip, err := i.runIPAM(ctx, makeReleaseAction(vmName))
	if err != nil {
		return net.IPNet{}, fmt.Errorf("failed to release IP: %w", err)
	}
	return ip, nil
}

// New returns a new IPAM object with ipam config and k8s/crd clients
func New(nadName string, nadNamespace string) (*IPAM, error) {
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
	return NewWithClient(kClient, nadName, nadNamespace)
}

func NewWithClient(kClient *Client, nadName string, nadNamespace string) (*IPAM, error) {
	ctx, cancel := context.WithTimeout(context.Background(), IpamRequestTimeout)
	defer cancel()

	// read network-attachment-definition from Kubernetes
	nad, err := kClient.NADClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nadNamespace).Get(ctx, nadName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if len(nad.Spec.Config) == 0 {
		return nil, fmt.Errorf("network-attachment-definition %s hasn't IPAM config section", nad.Name)
	}

	ipamConfig, err := LoadFromNad(nad.Spec.Config, nadNamespace)
	if err != nil {
		return nil, fmt.Errorf("network-attachment-definition IPAM config parse error: %w", err)
	}
	if len(ipamConfig.IPRanges) == 0 {
		return nil, fmt.Errorf("network-attachment-definition %s has not IP ranges", nad.Name)
	}

	return &IPAM{
		Config: *ipamConfig,
		Client: *kClient,
		mu:     sync.Mutex{},
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

	// process old-style Range to Ranges array
	if n.IPAM.Range != "" {
		oldRange := RangeConfiguration{
			OmitRanges: n.IPAM.OmitRanges,
			Range:      n.IPAM.Range,
			RangeStart: n.IPAM.RangeStart,
			RangeEnd:   n.IPAM.RangeEnd,
		}
		n.IPAM.IPRanges = append([]RangeConfiguration{oldRange}, n.IPAM.IPRanges...)
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

	// delete old style settings
	n.IPAM.OmitRanges = nil
	n.IPAM.Range = ""
	n.IPAM.RangeStart = nil
	n.IPAM.RangeEnd = nil

	// check Excluded IP ranges
	for idx := range n.IPAM.OmitRanges {
		_, _, err := net.ParseCIDR(n.IPAM.OmitRanges[idx])
		if err != nil {
			return nil, fmt.Errorf("invalid exclude CIDR %s: %w", n.IPAM.OmitRanges[idx], err)
		}
	}

	// set network namespace
	n.IPAM.NetworkNamespace = nadNamespace

	return n.IPAM, nil
}

// Performing IPAM actions
func (i *IPAM) runIPAM(ctx context.Context, action ipamAction) (net.IPNet, error) {
	var err error
	var ip net.IPNet
	log := log.FromContext(ctx)

	i.mu.Lock()
	defer i.mu.Unlock()

	ctx, ctxCancel := context.WithTimeout(ctx, IpamRequestTimeout)
	defer ctxCancel()

	// handle the ip add/del until successful
	for _, ipRange := range i.Config.IPRanges {
		// retry loop used to retry CRUD operations against Kubernetes
		// if we meet some issue then just do another attepmt
		ip, err = i.runIPAMRange(ctx, ipRange, action)
		// break ipRanges loop if ip was acquired/released
		if err == nil {
			return ip, nil
		}
		log.Error(err, "error acquiring/releasing IP from range", ipRange.Range)
	}
	return net.IPNet{}, err
}

func (i *IPAM) runIPAMRange(ctx context.Context, ipRange RangeConfiguration, action ipamAction) (net.IPNet, error) {
	var ip net.IPNet
	for retry := 0; retry < DatastoreRetries; retry++ {
		select {
		case <-ctx.Done():
			return net.IPNet{}, ctx.Err()
		default:
			// live in retry loop until context not cancelled
		}

		// read IPPool from ipppols.vm.neon.tech custom resource
		pool, err := i.getNeonvmIPPool(ctx, ipRange.Range)
		if err != nil {
			if e, ok := err.(Temporary); ok && e.Temporary() {
				// retry attempt to read IPPool
				time.Sleep(DatastoreRetriesDelay)
				continue
			}
			return net.IPNet{}, fmt.Errorf("error reading IP pool: %w", err)
		}

		currentReservation := pool.Allocations(ctx)
		var newReservation []whereaboutstypes.IPReservation
		ip, newReservation, err = action(ipRange, currentReservation)
		if err != nil {
			return net.IPNet{}, err
		}

		// update IPPool with newReservation
		err = pool.Update(ctx, newReservation)
		if err != nil {
			if e, ok := err.(Temporary); ok && e.Temporary() {
				// retry attempt to update IPPool
				time.Sleep(DatastoreRetriesDelay)
				continue
			}
			return net.IPNet{}, fmt.Errorf("error updating IP pool: %w", err)
		}
		return ip, nil
	}
	return ip, errors.New("IPAMretries limit reached")
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
	firstip  net.IP
}

// Allocations returns the initially retrieved set of allocations for this pool
func (p *NeonvmIPPool) Allocations(ctx context.Context) []whereaboutstypes.IPReservation {
	return toIPReservation(ctx, p.pool.Spec.Allocations, p.firstip)
}

// getNeonvmIPPool returns a NeonVM IPPool for the given IP range
func (i *IPAM) getNeonvmIPPool(ctx context.Context, ipRange string) (*NeonvmIPPool, error) {
	// for IP range 10.11.22.0/24 poll name will be
	// "10.11.22.0-24" if no network name in ipam spec, or
	// "samplenet-10.11.22.0-24" if nametwork name is `samplenet`
	var poolName string
	if i.Config.NetworkName == UnnamedNetwork {
		poolName = strings.ReplaceAll(ipRange, "/", "-")
	} else {
		poolName = fmt.Sprintf("%s-%s", i.Config.NetworkName, strings.ReplaceAll(ipRange, "/", "-"))
	}

	pool, err := i.VMClient.NeonvmV1().IPPools(i.Config.NetworkNamespace).Get(ctx, poolName, metav1.GetOptions{})
	if err != nil && apierrors.IsNotFound(err) {
		// pool does not exist, create it
		newPool := &vmv1.IPPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      poolName,
				Namespace: i.Config.NetworkNamespace,
			},
			Spec: vmv1.IPPoolSpec{
				Range:       ipRange,
				Allocations: make(map[string]vmv1.IPAllocation),
			},
		}
		_, err = i.VMClient.NeonvmV1().IPPools(i.Config.NetworkNamespace).Create(ctx, newPool, metav1.CreateOptions{})
		if err != nil && apierrors.IsAlreadyExists(err) {
			// the pool was just created -- allow retry
			return nil, &temporaryError{err}
		} else if err != nil {
			return nil, err
		}
		// if the pool was created for the first time, trigger another retry of the allocation loop
		return nil, &temporaryError{errors.New("NeonvmIPPool was initialized")}
	} else if err != nil {
		return nil, err
	}

	// get first IP in the pool
	ip, _, err := net.ParseCIDR(pool.Spec.Range)
	if err != nil {
		return nil, err
	}

	return &NeonvmIPPool{
		vmClient: i.Client.VMClient,
		pool:     pool,
		firstip:  ip,
	}, nil
}

// Update NeonvmIPPool with new IP reservation
func (p *NeonvmIPPool) Update(ctx context.Context, reservation []whereaboutstypes.IPReservation) error {
	p.pool.Spec.Allocations = toAllocations(reservation, p.firstip)
	_, err := p.vmClient.NeonvmV1().IPPools(p.pool.Namespace).Update(ctx, p.pool, metav1.UpdateOptions{})
	if err != nil {
		if apierrors.IsConflict(err) {
			return &temporaryError{err}
		}
		return err
	}
	return nil
}

// taken from whereabouts code as it not exported
func toIPReservation(ctx context.Context, allocations map[string]vmv1.IPAllocation, firstip net.IP) []whereaboutstypes.IPReservation {
	log := log.FromContext(ctx)
	reservelist := []whereaboutstypes.IPReservation{}
	for offset, a := range allocations {
		numOffset, err := strconv.ParseInt(offset, 10, 64)
		if err != nil {
			// allocations that are invalid int64s should be ignored
			// toAllocationMap should be the only writer of offsets, via `fmt.Sprintf("%d", ...)``
			log.Error(err, "error decoding ip offset")
			continue
		}
		ip := whereaboutsallocate.IPAddOffset(firstip, uint64(numOffset))
		reservelist = append(reservelist, whereaboutstypes.IPReservation{
			IP:          ip,
			ContainerID: a.ContainerID,
			PodRef:      a.PodRef,
			IsAllocated: false,
		})
	}
	return reservelist
}

// taken from whereabouts code as it not exported
func toAllocations(reservelist []whereaboutstypes.IPReservation, firstip net.IP) map[string]vmv1.IPAllocation {
	allocations := make(map[string]vmv1.IPAllocation)
	for _, r := range reservelist {
		index := whereaboutsallocate.IPGetOffset(r.IP, firstip)
		allocations[fmt.Sprintf("%d", index)] = vmv1.IPAllocation{ContainerID: r.ContainerID, PodRef: r.PodRef}
	}
	return allocations
}
