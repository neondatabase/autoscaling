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
	t   *testing.T
	ctx context.Context

	manager *ipam.Manager
	prom    *prometheus.Registry

	vmStates map[types.UID]*vmState
	ipStates map[string]*ipState
	clock    *helpers.FakeClock
}

func newManagerTest(t *testing.T) *managerTest {
	managerCfg := &ipam.IPAMManagerConfig{
		CooldownPeriod: "70s",
		HighIPCount:    100,
		LowIPCount:     50,
		TargetIPCount:  75,
	}
	require.NoError(t, managerCfg.Normalize())

	rangeCfg := &ipam.RangeConfiguration{
		Range:      "10.100.0.0/16",
		RangeStart: net.ParseIP("10.100.0.1"),
		RangeEnd:   nil,
		OmitRanges: nil,
	}
	require.NoError(t, rangeCfg.Normalize())

	neonvmClient := nfake.NewSimpleClientset()
	prom := prometheus.NewRegistry()
	metrics := ipam.NewIPAMMetrics(prom)
	poolClient, err := ipam.NewPoolClient(neonvmClient, rangeCfg, "test", "default", metrics)
	require.NoError(t, err)

	logger := testr.New(t)
	ctx := log.IntoContext(context.Background(), logger)

	clock := helpers.NewFakeClock(t)

	manager, err := ipam.NewManager(ctx, clock.Now, managerCfg, poolClient)
	require.NoError(t, err)

	metrics.AddManager(manager)

	return &managerTest{
		t:   t,
		ctx: ctx,

		manager: manager,
		prom:    prom,

		vmStates: make(map[types.UID]*vmState),
		ipStates: make(map[string]*ipState),
		clock:    clock,
	}
}

func generateAcquire(vmIDfrom, vmIDto int, actionsFrom, actionsTo int) []managerTestStep {
	var steps []managerTestStep
	for i := vmIDfrom; i < vmIDto; i++ {
		uid := types.UID(fmt.Sprintf("vm-%d", i)) // This is not a real UUID, but it is not a problem.

		for j := 0; j < rand.IntN(actionsTo-actionsFrom)+actionsFrom; j++ {
			steps = append(steps, managerTestStep{action: acquire, vmID: uid, sleep: 0})
		}
	}

	rand.Shuffle(len(steps), func(i, j int) {
		steps[i], steps[j] = steps[j], steps[i]
	})

	return steps
}

func generateRelease(vmIDmin, vmIDmax int) []managerTestStep {
	steps := generateAcquire(vmIDmin, vmIDmax, 1, 2)

	for i := range steps {
		steps[i].action = release
	}

	return steps
}

func generateAcquireRelease(vmIDfrom, vmIDto int, actionsFrom, actionsTo int) []managerTestStep {
	steps := generateAcquire(vmIDfrom, vmIDto, actionsFrom, actionsTo)

	// Iterate in reverse to mark last step for each VM as release
	released := make(map[types.UID]struct{})
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

	return steps
}

type testAction string

const (
	acquire testAction = "acquire"
	release testAction = "release"
	sleep   testAction = "sleep"
)

type managerTestStep struct {
	action testAction
	vmID   types.UID
	sleep  time.Duration
}

type vmState struct {
	ip net.IP
}

type ipState struct {
	vmID             types.UID
	cooldownDeadline time.Time
}

func (m *managerTest) run(steps []managerTestStep) {
	for _, step := range steps {
		switch step.action {
		case acquire:
			ip, err := m.manager.Allocate(m.ctx, types.UID(step.vmID))
			if err != nil {
				m.t.Fatalf("failed to acquire IP: %v", err)
			}
			ipSt := m.ipStates[ip.String()]
			if ipSt == nil {
				ipSt = &ipState{
					vmID:             "",
					cooldownDeadline: time.Time{},
				}
				m.ipStates[ip.IP.String()] = ipSt
			}
			if ipSt.vmID != "" && ipSt.vmID != step.vmID {
				m.t.Fatalf("IP %s is already allocated to %s", ip.String(), ipSt.vmID)
			}
			if ipSt.cooldownDeadline.After(m.clock.Now()) {
				m.t.Fatalf("cooldown deadline not met for %s", step.vmID)
			}
			ipSt.vmID = step.vmID
			ipSt.cooldownDeadline = m.clock.Now().Add(step.sleep)

			vmSt := m.vmStates[step.vmID]
			if vmSt == nil {
				vmSt = &vmState{
					ip: net.IP{},
				}
				m.vmStates[step.vmID] = vmSt
			}
			vmSt.ip = ip.IP

		case release:
			vmSt := m.vmStates[step.vmID]
			if vmSt == nil {
				m.t.Fatalf("VM %s is not allocated", step.vmID)
			}
			err := m.manager.Release(m.ctx, step.vmID, vmSt.ip)
			if err != nil {
				m.t.Fatalf("failed to release IP: %v", err)
			}

			ipSt := m.ipStates[vmSt.ip.String()]
			ipSt.vmID = ""
			ipSt.cooldownDeadline = m.clock.Now().Add(time.Second * 10)

		case sleep:
			m.clock.Inc(step.sleep)
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

	metrics := manager.collectMetrics()
	assert.ElementsMatch(t, []managerMetricValue{
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "allocated", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "free", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "unknown", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "cooldown", Count: 0},

		{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "total", Count: 65535},
	}, metrics)

	steps := generateAcquireRelease(0, 10, 2, 5)
	manager.t.Logf("steps: %v", steps)
	manager.run(steps)

	metrics = manager.collectMetrics()
	assert.ElementsMatch(t, []managerMetricValue{
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "allocated", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "free", Count: 40},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "unknown", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "cooldown", Count: 10},

		{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "managed", Count: 50},
		{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "total", Count: 65535},
	}, metrics)

	manager.clock.Inc(100 * time.Second)

	manager.run(generateAcquire(0, 1, 1, 2))

	metrics = manager.collectMetrics()
	assert.ElementsMatch(t, []managerMetricValue{
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "allocated", Count: 1},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "free", Count: 49},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "unknown", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "cooldown", Count: 0},

		{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "managed", Count: 50},
		{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "total", Count: 65535},
	}, metrics)
}

func TestManagerProduction(t *testing.T) {
	manager := newManagerTest(t)

	computesPerMinute := 600
	computeLifeTimeMinutes := 5

	minutes := 30

	for minute := 0; minute < minutes; minute++ {
		fmt.Printf("minute: %d\n", minute)

		if minute < minutes-computeLifeTimeMinutes {
			steps := generateAcquire(minute*computesPerMinute, (minute+1)*computesPerMinute, 1, 2)
			manager.run(steps)
		}

		if minute == 15 {
			metrics := manager.collectMetrics()
			assert.ElementsMatch(t, []managerMetricValue{
				{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "allocated", Count: 3600},
				{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "free", Count: 0},
				{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "unknown", Count: 0},
				{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "cooldown", Count: 600},

				{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "managed", Count: 4200},
				{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "total", Count: 65535},
			}, metrics)
		}

		if minute >= computeLifeTimeMinutes {
			oldMinute := minute - computeLifeTimeMinutes
			steps := generateRelease(oldMinute*computesPerMinute, (oldMinute+1)*computesPerMinute)
			manager.run(steps)
		}

		manager.clock.Inc(time.Minute)
	}

	metrics := manager.collectMetrics()
	assert.ElementsMatch(t, []managerMetricValue{
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "allocated", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "free", Count: 3000},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "unknown", Count: 0},
		{Name: "ipam_manager_ip_count", Pool: "test-10.100.0.0-16", State: "cooldown", Count: 1200},

		{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "managed", Count: 4200},
		{Name: "ipam_pool_ip_count", Pool: "test-10.100.0.0-16", State: "total", Count: 65535},
	}, metrics)
}
