package ipam_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"k8s.io/apimachinery/pkg/types"

	nfake "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned/fake"
	helpers "github.com/neondatabase/autoscaling/pkg/agent/core/testhelpers"
	"github.com/neondatabase/autoscaling/pkg/neonvm/ipam"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type managerTest struct {
	prom    *prometheus.Registry
	manager *ipam.Manager

	t   *testing.T
	ctx context.Context

	steps    []managerTestStep
	vmStates map[string]*vmState
	ipStates map[string]*ipState
	timer    *helpers.FakeClock
}

func newManagerTest(t *testing.T) *managerTest {
	managerCfg := &ipam.IPAMManagerConfig{
		CooldownPeriod: "10s",
		HighIPCount:    100,
		LowIPCount:     50,
		TargetIPCount:  75,
	}
	require.NoError(t, managerCfg.Normalize())

	rangeCfg := &ipam.RangeConfiguration{
		Range:      "10.100.0.0/16",
		RangeStart: net.ParseIP("10.100.0.1"),
		OmitRanges: []string{},
		RangeEnd:   nil,
	}
	require.NoError(t, rangeCfg.Normalize())

	neonvmClient := nfake.NewSimpleClientset()
	prom := prometheus.NewRegistry()
	metrics := ipam.NewIPAMMetrics(prom)
	poolClient, err := ipam.NewPoolClient(neonvmClient, rangeCfg, types.NamespacedName{Name: "test", Namespace: "default"}, metrics)
	require.NoError(t, err)

	logger := testr.New(t)
	ctx := log.IntoContext(context.Background(), logger)

	manager, err := ipam.NewManager(ctx, managerCfg, poolClient)
	require.NoError(t, err)

	metrics.AddManager(manager)

	return &managerTest{
		prom:    prom,
		manager: manager,

		t:   t,
		ctx: ctx,

		steps:    make([]managerTestStep, 0),
		vmStates: make(map[string]*vmState),
		ipStates: make(map[string]*ipState),
		timer:    helpers.NewFakeClock(t),
	}
}

func (m *managerTest) addSteps(vmCount int, sleepCount int) {
	steps := make([]managerTestStep, 0, 2*vmCount+sleepCount)
	for i := 0; i < vmCount; i++ {
		steps = append(steps, managerTestStep{action: acquire, vmID: fmt.Sprintf("vm-%d", i), sleep: 0})
		steps = append(steps, managerTestStep{action: acquire, vmID: fmt.Sprintf("vm-%d", i), sleep: 0})
	}
	for i := 0; i < sleepCount; i++ {
		steps = append(steps, managerTestStep{action: sleep, sleep: 1 * time.Second, vmID: ""})
	}

	rand.Shuffle(len(steps), func(i, j int) {
		steps[i], steps[j] = steps[j], steps[i]
	})

	// Iterate in reverse to mark last step for each VM as release
	released := make(map[string]struct{})
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].action != acquire {
			continue
		}
		if _, ok := released[steps[i].vmID]; ok {
			continue
		}
		released[steps[i].vmID] = struct{}{}
		steps[i].action = release
	}

	m.steps = append(m.steps, steps...)
}

type testAction string

const (
	acquire testAction = "acquire"
	release testAction = "release"
	sleep   testAction = "sleep"
)

type managerTestStep struct {
	action testAction
	vmID   string
	sleep  time.Duration
}

type vmState struct {
	ip net.IP
}

type ipState struct {
	vmID             string
	cooldownDeadline time.Time
}

func (m *managerTest) run() {
	for _, step := range m.steps {
		switch step.action {
		case acquire:
			ip, err := m.manager.Allocate(m.ctx, vmID(step.vmID))
			if err != nil {
				m.t.Fatalf("failed to acquire IP: %v", err)
			}
			state := m.ipStates[ip.String()]
			if state == nil {
				state = &ipState{
					vmID:             "",
					cooldownDeadline: time.Time{},
				}
				m.ipStates[ip.String()] = state
			}
			if state.vmID != "" {
				m.t.Fatalf("IP %s is already allocated to %s", ip.String(), state.vmID)
			}
			if state.cooldownDeadline.After(m.timer.Now()) {
				m.t.Fatalf("cooldown deadline not met for %s", step.vmID)
			}
			state.vmID = step.vmID
			state.cooldownDeadline = m.timer.Now().Add(step.sleep)

			otherState := m.vmStates[step.vmID]
			if otherState == nil {
				otherState = &vmState{
					ip: net.IP{},
				}
				m.vmStates[step.vmID] = otherState
			}
			otherState.ip = ip.IP

		case release:
			state := m.vmStates[step.vmID]
			if state == nil {
				m.t.Fatalf("VM %s is not allocated", step.vmID)
			}
			err := m.manager.Release(m.ctx, vmID(step.vmID), state.ip)
			if err != nil {
				m.t.Fatalf("failed to release IP: %v", err)
			}
		case sleep:
			m.timer.Inc(step.sleep)
		}
	}
}

func vmID(name string) util.NamespacedName {
	return util.NamespacedName{
		Namespace: "default",
		Name:      name,
	}
}

type managerMetricValue struct {
	Name  string
	Pool  string
	State string
	Count float64
}

func (m *managerTest) collectMetrics() []managerMetricValue {
	metrics, err := m.prom.Gather()
	require.NoError(m.t, err)

	var result []managerMetricValue

	for _, metric := range metrics {
		name := metric.GetName()

		for _, sample := range metric.Metric {
			var pool, state string
			for _, label := range sample.GetLabel() {
				if label.GetName() == "pool" {
					pool = label.GetValue()
				}
				if label.GetName() == "state" {
					state = label.GetValue()
				}
			}

			result = append(result, managerMetricValue{
				Name:  name,
				Pool:  pool,
				State: state,
				Count: sample.GetGauge().GetValue(),
			})
		}
	}

	return result
}

func TestManager(t *testing.T) {
	manager := newManagerTest(t)
	manager.addSteps(10, 10)

	manager.t.Logf("steps: %v", manager.steps)

	metrics := manager.collectMetrics()
	assert.ElementsMatch(t, []managerMetricValue{
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "allocated", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "free", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "unknown", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "cooldown", Count: 0},
		{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "total", Count: 65535},
	}, metrics)

	manager.run()

	metrics = manager.collectMetrics()
	assert.ElementsMatch(t, []managerMetricValue{
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "allocated", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "free", Count: 40},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "unknown", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "cooldown", Count: 10},

		{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "managed", Count: 50},
		{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "total", Count: 65535},
	}, metrics)
}
