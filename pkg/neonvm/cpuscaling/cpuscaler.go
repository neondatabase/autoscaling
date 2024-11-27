package cpuscaling

import (
	"errors"
	"slices"
)

type CPUStater interface {
	OnlineCPUs() ([]int, error)
	OfflineCPUs() ([]int, error)
	SetState(cpuID int, cpuState cpuState) error
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
	online, err := c.cpuState.OnlineCPUs()
	if err != nil {
		return err
	}

	if len(online) == targetCount {
		return nil
	}

	if len(online) > targetCount {
		diff := len(online) - targetCount
		// offline 'diff' CPUs that are currently online
		// reverse online slice so that we offline in the reverse order of onlining.
		slices.Reverse(online)
		return c.setStateTo(cpuOffline, diff, online)

	} else if len(online) < targetCount {
		offline, err := c.cpuState.OfflineCPUs()
		if err != nil {
			return nil
		}

		diff := targetCount - len(online)
		// online 'diff' CPUs that are currently offline
		return c.setStateTo(cpuOnline, diff, offline)
	}

	return nil
}

func (c *CPUScaler) setStateTo(state cpuState, count int, candidateCPUs []int) error {
	for _, cpuID := range candidateCPUs {
		if cpuID == 0 {
			// Not allowed to change the status of CPU 0
			continue
		}

		if err := c.cpuState.SetState(cpuID, state); err != nil {
			return err
		}

		count -= 1
		// nothing left to do
		if count <= 0 {
			return nil
		}
	}

	// Got through the entire list but didn't change the state of enough CPUs
	return errors.New("could not change the state of enough CPUs")
}

// ActiveCPUsCount() returns the count of online CPUs.
func (c *CPUScaler) ActiveCPUsCount() (int, error) {
	onlineCPUs, err := c.cpuState.OnlineCPUs()
	if err != nil {
		return 0, err
	}
	return len(onlineCPUs), nil
}
