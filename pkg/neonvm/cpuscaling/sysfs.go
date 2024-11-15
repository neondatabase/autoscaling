package cpuscaling

import (
	"fmt"
	"os"
	"path/filepath"
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
	data, err := os.ReadFile(filepath.Join(cpuPath, fmt.Sprintf("cpu%d/online", cpuNum)))

	if os.IsNotExist(err) {
		// If the file doesn't exist for CPU 0, assume it's online
		if cpuNum == 0 {
			return cpuOnline, nil
		}
		return cpuOffline, err
	}

	if err != nil {
		return cpuOffline, err
	}

	online := strings.TrimSpace(string(data))
	if online == "1" {
		return cpuOnline, nil
	}
	return cpuOffline, nil
}

func (cs *cpuSysfsState) PossibleCPUs() (int, int, error) {
	data, err := os.ReadFile(filepath.Join(cpuPath, "possible"))
	if err != nil {
		return -1, -1, err
	}

	return cs.parseCPURange(string(data))
}

// parseCPURange parses the CPU range string (e.g., "0-3") and returns a list of CPUs.
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
