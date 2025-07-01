package ipam

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/Jille/contextcond"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/util/metricfunc"
)

type IPAMManagerConfig struct {
	CooldownPeriod string `json:"cooldown_period"`
	HighIPCount    int    `json:"high_ip_count"`
	LowIPCount     int    `json:"low_ip_count"`
	TargetIPCount  int    `json:"target_ip_count"`

	cooldownPeriod time.Duration
}

func (c *IPAMManagerConfig) Normalize() error {
	if c.CooldownPeriod == "" {
		return fmt.Errorf("cooldown period must be set")
	}

	if c.HighIPCount <= 0 {
		return fmt.Errorf("high IP count must be positive")
	}

	if c.LowIPCount <= 0 {
		return fmt.Errorf("low IP count must be positive")
	}

	if c.TargetIPCount <= 0 {
		return fmt.Errorf("target IP count must be positive")
	}

	if c.HighIPCount < c.TargetIPCount {
		return fmt.Errorf("high IP count must be greater than target IP count")
	}

	if c.LowIPCount > c.TargetIPCount {
		return fmt.Errorf("low IP count must be less than target IP count")
	}

	cooldownPeriod, err := time.ParseDuration(c.CooldownPeriod)
	if err != nil {
		return fmt.Errorf("invalid cooldown period: %w", err)
	}
	c.cooldownPeriod = cooldownPeriod

	return nil
}

type Manager struct {
	cfg  *IPAMManagerConfig
	pool *PoolClient

	allocations   map[types.UID]netip.Addr
	free          []netip.Addr
	unknown       map[netip.Addr]struct{}
	cooldownQueue []CooldownEntry

	now                func() time.Time
	rebalanceCondition *contextcond.Cond
	mu                 sync.Mutex
}

type CooldownEntry struct {
	ip       netip.Addr
	deadline time.Time
}

func NewManager(ctx context.Context, now func() time.Time, cfg *IPAMManagerConfig, poolClient *PoolClient) (*Manager, error) {
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}

	pool, err := poolClient.Get(ctx)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		cfg:  cfg,
		pool: poolClient,

		allocations:   make(map[types.UID]netip.Addr),
		unknown:       make(map[netip.Addr]struct{}),
		free:          nil,
		cooldownQueue: nil,

		now:                now,
		rebalanceCondition: nil,
		mu:                 sync.Mutex{},
	}

	for ip := range pool.Spec.Managed {
		ip, err := netip.ParseAddr(ip)
		if err != nil {
			return nil, fmt.Errorf("invalid IP in pool: %w", err)
		}
		m.unknown[ip] = struct{}{}
	}

	m.rebalanceCondition = contextcond.NewCond(&m.mu)

	return m, nil
}

func (m *Manager) Allocate(ctx context.Context, vmID types.UID) (net.IPNet, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ip, ok := m.allocations[vmID]; ok {
		return m.ipToNet(ip), nil
	}
	if ctx.Err() != nil {
		return net.IPNet{}, ctx.Err()
	}

	// If any IPs are cold already
	m.finishCooldown()

	if len(m.free) == 0 {
		// Do sync rebalance
		m.callRebalance(ctx)
	}

	if len(m.free) == 0 {
		// Didn't help
		return net.IPNet{}, fmt.Errorf("no IPs in the pool")
	}

	// Take last free IP
	// ip := m.free[len(m.free)-1]
	// m.free = m.free[:len(m.free)-1]
	ip := m.free[0]
	m.free = m.free[1:]
	// NOTE: maybe we should actually take IPs from the head of m.free, not tail?
	// This will grant us an additional time gap between end of CooldownPeriod, and new allocation of a particular IP.

	m.allocations[vmID] = ip

	// Trigger always
	m.asyncRebalance()

	return m.ipToNet(ip), nil
}

func (m *Manager) Release(_ context.Context, vmID types.UID, ip net.IP) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ipAddr, err := netip.ParseAddr(ip.String())
	if err != nil {
		return fmt.Errorf("invalid IP: %s", ip)
	}

	if _, ok := m.unknown[ipAddr]; ok {
		delete(m.unknown, ipAddr)
	} else if allocIP, ok := m.allocations[vmID]; ok {
		if allocIP.String() != ipAddr.String() {
			return fmt.Errorf("VM %s: attempted to release %s, allocated %s", vmID, ipAddr, allocIP)
		}
		delete(m.allocations, vmID)
	} else {
		return fmt.Errorf("attempt to release unfamiliar allocation %s %s", vmID, ip)
	}

	m.startCooldown(ipAddr)

	// Trigger always
	m.asyncRebalance()

	return nil
}

func (m *Manager) ipToNet(ip netip.Addr) net.IPNet {
	return net.IPNet{IP: ip.AsSlice(), Mask: m.pool.rangeConfig.ipNet.Mask}
}

func (m *Manager) startCooldown(ip netip.Addr) {
	m.cooldownQueue = append(m.cooldownQueue,
		CooldownEntry{
			ip:       ip,
			deadline: m.now().Add(m.cfg.cooldownPeriod),
		},
	)

	m.finishCooldown()
}

func (m *Manager) finishCooldown() {
	now := m.now()
	for len(m.cooldownQueue) > 0 {
		head := m.cooldownQueue[0]
		if head.deadline.After(now) {
			return
		}

		m.free = append(m.free, head.ip)
		m.cooldownQueue = m.cooldownQueue[1:]
	}
}

func (m *Manager) SetActive(active map[netip.Addr]types.UID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for ip := range m.unknown {
		vmID, ok := active[ip]
		if ok {
			m.allocations[vmID] = ip
		} else {
			m.startCooldown(ip)
		}
	}

	m.unknown = nil
}

func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	log := log.FromContext(ctx)

	for {
		err := m.rebalanceCondition.WaitContext(ctx)
		if err != nil {
			log.Info("context cancelled, stopping rebalance loop")
			return
		}

		m.callRebalance(ctx)
	}
}

func (m *Manager) asyncRebalance() {
	if m.needRebalance() {
		m.rebalanceCondition.Signal()
	}
}

func (m *Manager) needRebalance() bool {
	return len(m.free) > m.cfg.HighIPCount || len(m.free) < m.cfg.LowIPCount
}

func (m *Manager) callRebalance(ctx context.Context) {
	log := log.FromContext(ctx)

	err := m.rebalance(ctx)
	if err != nil {
		log.Error(err, "error rebalancing IP pool")
	} else {
		log.Info("rebalanced IP pool", "pool", m.pool.PoolName(), "free", len(m.free), "unknown", len(m.unknown))

		m.ipCountLog(ctx)
	}
}

func (m *Manager) rebalance(ctx context.Context) error {
	if !m.needRebalance() {
		return nil
	}

	pool, err := m.pool.Get(ctx)
	if err != nil {
		return err
	}

	// commit new free IPs only when
	// pool is commited
	var newFree []netip.Addr

	// We have too many IPs
	if len(m.free) > m.cfg.HighIPCount {
		for _, ip := range m.free[m.cfg.TargetIPCount:] {
			delete(pool.Spec.Managed, ip.String())
		}

		newFree = m.free[:m.cfg.TargetIPCount]
	}
	// We have not enough IPs
	if len(m.free) < m.cfg.LowIPCount {
		newFree = append(newFree, m.free...)
		for ip := range m.pool.rangeConfig.AllIPs() {
			if _, ok := pool.Spec.Managed[ip.String()]; ok {
				continue
			}
			newFree = append(newFree, ip)
			pool.Spec.Managed[ip.String()] = v1.Unit{}
			if len(newFree) >= m.cfg.LowIPCount {
				break
			}
		}
	}

	err = m.pool.Commit(ctx, pool)
	if err != nil {
		return err
	}

	m.free = newFree

	return nil
}

func (m *Manager) ipCountMetric() []metricfunc.MetricValue {
	var result []metricfunc.MetricValue
	add := func(state string, value int) {
		result = append(result, metricfunc.MetricValue{
			Labels: prometheus.Labels{PoolLabel: m.pool.PoolName(), StateLabel: state},
			Value:  float64(value),
		})
	}

	m.ipCount(add)

	return result
}

func (m *Manager) ipCountLog(ctx context.Context) {
	log := log.FromContext(ctx)

	m.ipCount(func(state string, value int) {
		log.Info("ipCounts", "pool", m.pool.PoolName(), "state", state, "value", value)
	})
}

func (m *Manager) ipCount(cb func(state string, value int)) {
	cb("allocated", len(m.allocations))
	cb("free", len(m.free))
	cb("unknown", len(m.unknown))
	cb("cooldown", len(m.cooldownQueue))
}
