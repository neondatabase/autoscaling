package cpuscaling

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// CPU directory path
const cpuPath = "/sys/devices/system/cpu/"

type cpuSysfsState struct{}

func (cs *cpuSysfsState) SetState(cpuNum int, cpuState cpuState) error {
	var state string
	switch cpuState {
	case cpuOnline:
		state = "1"
	case cpuOffline:
		state = "0"
	}

	err := os.WriteFile(filepath.Join(cpuPath, fmt.Sprintf("cpu%d/online", cpuNum)), []byte(state), 0o644)
	if err != nil {
		return fmt.Errorf("failed to set CPU %d online status: %w", cpuNum, err)
	}

	return nil
}

func (cs *cpuSysfsState) GetState(cpuNum int) (cpuState, error) {
	onlineCPUs, err := cs.getAllOnlineCPUs()
	if err != nil {
		return cpuOffline, err
	}
	if slices.Contains(onlineCPUs, cpuNum) {
		return cpuOnline, nil
	}

	return cpuOffline, nil
}

func (cs *cpuSysfsState) getAllOnlineCPUs() ([]int, error) {
	data, err := os.ReadFile(filepath.Join(cpuPath, "online"))
	if err != nil {
		return nil, err
	}

	onlineCPUs, err := cs.parseMultipleCPURange(string(data))
	if err != nil {
		return onlineCPUs, err
	}

	return onlineCPUs, nil
}

// PossibleCPUs returns the start and end indexes of all possible CPUs.
func (cs *cpuSysfsState) PossibleCPUs() (int, int, error) {
	data, err := os.ReadFile(filepath.Join(cpuPath, "possible"))
	if err != nil {
		return -1, -1, err
	}

	return cs.parseCPURange(string(data))
}

// parseCPURange parses the CPU range string (e.g., "0-3") and returns start and end indexes.
func (cs *cpuSysfsState) parseCPURange(cpuRange string) (int, int, error) {
	cpuRange = strings.TrimSpace(cpuRange)
	parts := strings.Split(cpuRange, "-")

	// Single CPU case, e.g., "0"
	if len(parts) == 1 {
		cpu, err := strconv.Atoi(parts[0])
		if err != nil {
			return -1, -1, err
		}
		return cpu, cpu, nil
	}

	// Range case, e.g., "0-3"
	start, err := strconv.Atoi(parts[0])
	if err != nil {
		return -1, -1, err
	}
	end, err := strconv.Atoi(parts[1])
	if err != nil {
		return -1, -1, err
	}
	return start, end, nil
}

// parseMultipleCPURange parses the multiple CPU range string (e.g., "0-3,5-7") and returns a list of CPUs.
func (cs *cpuSysfsState) parseMultipleCPURange(cpuRanges string) ([]int, error) {
	cpuRanges = strings.TrimSpace(cpuRanges)
	parts := strings.Split(cpuRanges, ",")

	var cpus []int
	for _, part := range parts {
		start, end, err := cs.parseCPURange(part)
		if err != nil {
			return nil, err
		}

		for cpu := start; cpu <= end; cpu++ {
			cpus = append(cpus, cpu)
		}
	}

	return cpus, nil
}

// ActiveCPUsCount() returns the count of online CPUs.
func (c *cpuSysfsState) ActiveCPUsCount() (int, error) {
	start, end, err := c.PossibleCPUs()
	if err != nil {
		return 0, err
	}

	var onlineCount int
	for cpu := start; cpu <= end; cpu++ {
		state, err := c.GetState(cpu)
		if err != nil {
			return 0, err
		}
		if state == cpuOnline {
			onlineCount++
		}
	}

	return onlineCount, nil
}
