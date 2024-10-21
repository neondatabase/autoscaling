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

type CPUSysFsStateScaler struct {
}

func (c *CPUSysFsStateScaler) EnsureOnlineCPUs(targetCount int) error {
	cpus, err := getAllCPUs()
	if err != nil {
		return err
	}

	onlineCount, err := c.GetActiveCPUsCount()
	if err != nil {
		return err
	}

	if onlineCount < uint32(targetCount) {
		for _, cpu := range cpus {
			if cpu == 0 {
				// Skip CPU 0 as it is always online and can't be toggled
				continue
			}

			online, err := isCPUOnline(cpu)
			if err != nil {
				return err
			}

			if !online {
				// Mark CPU as online
				err := setCPUOnline(cpu, true)
				if err != nil {
					return err
				}
				onlineCount++
			}

			// Stop when we reach the target count
			if onlineCount == uint32(targetCount) {
				break
			}
		}
	} else if onlineCount > uint32(targetCount) {
		// Remove CPUs if there are more than X online
		for i := len(cpus) - 1; i >= 0; i-- {
			cpu := cpus[i]
			if cpu == 0 {
				// Skip CPU 0 as it cannot be taken offline
				continue
			}

			online, err := isCPUOnline(cpu)
			if err != nil {
				return err
			}

			if online {
				// Mark CPU as offline
				err := setCPUOnline(cpu, false)
				if err != nil {
					return err
				}
				onlineCount--
			}

			// Stop when we reach the target count
			if onlineCount == uint32(targetCount) {
				break
			}
		}
	}

	if onlineCount != uint32(targetCount) {
		return fmt.Errorf("failed to ensure %d CPUs are online, current online CPUs: %d", targetCount, onlineCount)
	}

	return nil
}

// GetActiveCPUsCount() returns the count of online CPUs.
func (c *CPUSysFsStateScaler) GetActiveCPUsCount() (uint32, error) {
	cpus, err := getAllCPUs()
	if err != nil {
		return 0, err
	}

	var onlineCount uint32
	for _, cpu := range cpus {
		online, err := isCPUOnline(cpu)
		if err != nil {
			return 0, err
		}
		if online {
			onlineCount++
		}
	}

	return onlineCount, nil
}

// GetTotalCPUsCount returns the total number of CPUs (online + offline).
func (c *CPUSysFsStateScaler) GetTotalCPUsCount() (uint32, error) {
	cpus, err := getAllCPUs()
	if err != nil {
		return 0, err
	}

	return uint32(len(cpus)), nil
}

// Helper functions

// getAllCPUs returns a list of all CPUs that are physically present in the system.
func getAllCPUs() ([]int, error) {
	data, err := os.ReadFile(filepath.Join(cpuPath, "possible"))
	if err != nil {
		return nil, err
	}

	return parseCPURange(string(data))
}

// parseCPURange parses the CPU range string (e.g., "0-3") and returns a list of CPUs.
func parseCPURange(cpuRange string) ([]int, error) {
	var cpus []int
	cpuRange = strings.TrimSpace(cpuRange)
	parts := strings.Split(cpuRange, "-")

	if len(parts) == 1 {
		// Single CPU case, e.g., "0"
		cpu, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, err
		}
		cpus = append(cpus, cpu)
	} else if len(parts) == 2 {
		// Range case, e.g., "0-3"
		start, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, err
		}
		end, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, err
		}
		for i := start; i <= end; i++ {
			cpus = append(cpus, i)
		}
	}

	return cpus, nil
}

// isCPUOnline checks if a given CPU is online.
func isCPUOnline(cpu int) (bool, error) {
	data, err := os.ReadFile(filepath.Join(cpuPath, fmt.Sprintf("cpu%d/online", cpu)))
	if os.IsNotExist(err) {
		// If the file doesn't exist for CPU 0, assume it's online
		if cpu == 0 {
			return true, nil
		}
		return false, err
	}
	if err != nil {
		return false, err
	}

	online := strings.TrimSpace(string(data))
	return online == "1", nil
}

// setCPUOnline sets the given CPU as online (true) or offline (false).
func setCPUOnline(cpu int, online bool) error {
	state := "0"
	if online {
		state = "1"
	}

	err := os.WriteFile(filepath.Join(cpuPath, fmt.Sprintf("cpu%d/online", cpu)), []byte(state), 0644)
	if err != nil {
		return fmt.Errorf("failed to set CPU %d online status: %w", cpu, err)
	}

	return nil
}
