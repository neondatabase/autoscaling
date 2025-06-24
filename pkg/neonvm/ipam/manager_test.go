package ipam_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"k8s.io/apimachinery/pkg/types"

	nfake "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned/fake"
	helpers "github.com/neondatabase/autoscaling/pkg/agent/core/testhelpers"
	"github.com/neondatabase/autoscaling/pkg/neonvm/ipam"
)

type managerTest struct {
	t   *testing.T
	ctx context.Context

	manager *ipam.Manager

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
	poolClient, err := ipam.NewPoolClient(neonvmClient, rangeCfg, "test", "default")
	require.NoError(t, err)

	logger := testr.New(t)
	ctx := log.IntoContext(context.Background(), logger)

	clock := helpers.NewFakeClock(t)

	manager, err := ipam.NewManager(ctx, clock.Now, managerCfg, poolClient)
	require.NoError(t, err)
	return &managerTest{
		t:   t,
		ctx: ctx,

		manager: manager,

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
				m.ipStates[ip.String()] = ipSt
			}
			if ipSt.vmID != "" {
				m.t.Fatalf("IP %s is already allocated to %s", ip.String(), ipSt.vmID)
			}
			if ipSt.cooldownDeadline.After(m.clock.Now()) {
				m.t.Fatalf("cooldown deadline not met for %s", step.vmID)
			}
			ipSt.vmID = step.vmID
			ipSt.cooldownDeadline = m.clock.Now().Add(step.sleep)

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
			err := m.manager.Release(m.ctx, step.vmID, state.ip)
			if err != nil {
				m.t.Fatalf("failed to release IP: %v", err)
			}
		case sleep:
			m.clock.Inc(step.sleep)
		}
	}
}

func TestManager(t *testing.T) {
	manager := newManagerTest(t)
	steps := generateAcquireRelease(0, 10, 2, 3)
	fmt.Println(steps)
	manager.run(steps)

	manager.t.Logf("vmStates: %v", manager.vmStates)
	manager.t.Logf("ipStates: %v", manager.ipStates)
}
