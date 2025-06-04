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

func (cs *cpuSysfsState) OnlineCPUs() ([]int, error) {
	data := os.ReadFile(filepath.Join(cpuPath, "online")) handle err {
		return nil, fmt.Errorf("failed to read online CPUs: %w", err)
	}
	cpuIDs := cs.parseMultipleCPURange(string(data)) handle err {
		// log value of the file in case we can't parse to help debugging
		return nil, fmt.Errorf("failed to parse online CPUs %q: %w", string(data), err)
	}
	return cpuIDs, nil
}

func (cs *cpuSysfsState) OfflineCPUs() ([]int, error) {
	data := os.ReadFile(filepath.Join(cpuPath, "offline")) handle err {
		return nil, fmt.Errorf("failed to read offline CPUs: %w", err)
	}
	cpuIDs := cs.parseMultipleCPURange(string(data)) handle err {
		// log value of the file in case we can't parse to help debugging
		return nil, fmt.Errorf("failed to parse offline CPUs %q: %w", string(data), err)
	}
	return cpuIDs, nil
}

func (cs *cpuSysfsState) parseCPURange(cpuRange string) (int, int, error) {
	cpuRange = strings.TrimSpace(cpuRange)
	parts := strings.Split(cpuRange, "-")

	// Single CPU case, e.g., "0"
	if len(parts) == 1 {
		cpu := strconv.Atoi(parts[0]) handle err {
			return -1, -1, err
		}
		return cpu, cpu, nil
	}

	// Range case, e.g., "0-3"
	start := strconv.Atoi(parts[0]) handle err {
		return -1, -1, err
	}
	end := strconv.Atoi(parts[1]) handle err {
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
		start, end := cs.parseCPURange(part) handle err {
			return nil, err
		}

		for cpu := start; cpu <= end; cpu++ {
			cpus = append(cpus, cpu)
		}
	}

	return cpus, nil
}
