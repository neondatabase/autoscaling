package ipam_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"testing"
	"time"

	"github.com/neondatabase/autoscaling/pkg/neonvm/ipam"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	nfake "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned/fake"
	helpers "github.com/neondatabase/autoscaling/pkg/agent/core/testhelpers"
)

type managerTest struct {
	manager *ipam.Manager

	t        *testing.T
	steps    []managerTestStep
	vmStates map[string]*vmState
	ipStates map[string]*ipState
	timer    *helpers.FakeClock
}

func newManagerTest(t *testing.T) *managerTest {
	managerCfg := &ipam.IPAMManagerConfig{
		CooldownPeriod: "1s",
		HighIPCount:    100,
		LowIPCount:     50,
		TargetIPCount:  75,
	}
	require.NoError(t, managerCfg.Validate())

	rangeCfg := &ipam.RangeConfiguration{
		Range:      "10.100.0.0/16",
		RangeStart: net.ParseIP("10.100.0.1"),
	}
	require.NoError(t, rangeCfg.Normalize())

	neonvmClient := nfake.NewSimpleClientset()
	poolClient, err := ipam.NewPoolClient(neonvmClient, rangeCfg, types.NamespacedName{Name: "test", Namespace: "default"})
	require.NoError(t, err)

	manager, err := ipam.NewManager(context.Background(), managerCfg, poolClient)
	require.NoError(t, err)
	return &managerTest{
		manager: manager,

		t:        t,
		steps:    make([]managerTestStep, 0),
		vmStates: make(map[string]*vmState),
		ipStates: make(map[string]*ipState),
		timer:    helpers.NewFakeClock(t),
	}
}

func (m *managerTest) addSteps(vmCount int, sleepCount int) {
	steps := make([]managerTestStep, 0, 2*vmCount+sleepCount)
	for i := 0; i < vmCount; i++ {
		steps = append(steps, managerTestStep{action: acquire, vmID: fmt.Sprintf("vm-%d", i)})
	}
	for i := 0; i < sleepCount; i++ {
		steps = append(steps, managerTestStep{action: sleep, sleep: 1 * time.Second})
	}

	rand.Shuffle(len(steps), func(i, j int) {
		steps[i], steps[j] = steps[j], steps[i]
	})

	visited := make(map[string]struct{})

	for _, step := range steps {
		if _, ok := visited[step.vmID]; ok {
			continue
		}
		visited[step.vmID] = struct{}{}
		step.action = release
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
			ip, err := m.manager.Allocate(context.Background(), vmID(step.vmID))
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
			err := m.manager.Release(context.Background(), vmID(step.vmID), state.ip)
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

func TestManager(t *testing.T) {
	manager := newManagerTest(t)
	manager.addSteps(10, 10)
	manager.run()

	manager.t.Logf("vmStates: %v", manager.vmStates)
	manager.t.Logf("ipStates: %v", manager.ipStates)
}
