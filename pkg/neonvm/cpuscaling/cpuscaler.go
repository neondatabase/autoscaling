package cpuscaling

import "fmt"

type CPUStater interface {
	PossibleCPUs() (start int, end int, err error)
	SetState(cpuNum int, cpuState cpuState) error
	GetState(cpuNum int) (cpuState, error)
}

type cpuState string

const (
	cpuOnline  cpuState = "online"
	cpuOffline cpuState = "offline"
)

type CPUScaler struct {
	cpuState CPUStater
}

func NewCPUScaler() *CPUScaler {
	return &CPUScaler{
		cpuState: &cpuSysfsState{},
	}
}

func (c *CPUScaler) ReconcileOnlineCPU(targetCount int) error {
	start, end, err := c.cpuState.PossibleCPUs()
	if err != nil {
		return err
	}

	if start == end {
		// we can't scale only one CPU
		// so we return early
		return nil
	}

	onlineCount, err := c.ActiveCPUsCount()
	if err != nil {
		return err
	}

	targetCpuStateToSet := cpuOnline
	if onlineCount < targetCount {
		targetCpuStateToSet = cpuOnline
	} else if onlineCount > targetCount {
		targetCpuStateToSet = cpuOffline
	}
	return c.reconcileToState(int(onlineCount), targetCount, targetCpuStateToSet, start, end)
}

func (c *CPUScaler) reconcileToState(onlineCount int, targetCount int, targetState cpuState, start int, end int) error {
	for cpu := start; cpu <= end; cpu++ {

		// Skip CPU 0 as it is always online and can't be offed
		if cpu == 0 && targetState == cpuOffline {
			continue
		}

		cpuState, err := c.cpuState.GetState(cpu)
		if err != nil {
			return err
		}

		if cpuState != targetState {
			// mark cpu with targetState
			err := c.cpuState.SetState(cpu, targetState)
			if err != nil {
				return err
			}

			// update counter
			if targetState == cpuOnline {
				onlineCount++
			} else {
				onlineCount--
			}
		}

		// Stop when we reach the target count
		if onlineCount == targetCount {
			break
		}
	}

	if onlineCount != targetCount {
		return fmt.Errorf("failed to ensure %d CPUs are online, current online CPUs: %d", targetCount, onlineCount)
	}

	return nil
}

// ActiveCPUsCount() returns the count of online CPUs.
func (c *CPUScaler) ActiveCPUsCount() (int, error) {
	start, end, err := c.cpuState.PossibleCPUs()
	if err != nil {
		return 0, err
	}

	var onlineCount int
	for cpu := start; cpu <= end; cpu++ {
		state, err := c.cpuState.GetState(cpu)
		if err != nil {
			return 0, err
		}
		if state == cpuOnline {
			onlineCount++
		}
	}

	return onlineCount, nil
}
