package main

// crictl abstraction and commands

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"go.uber.org/zap"
)

type Crictl struct {
	endpoint string
}

// Pods calls 'crictl pods' and, if successful, returns the parsed output
//
// This command lists all running pods.
func (c *Crictl) Pods(logger *zap.Logger) (*CrictlPods, error) {
	var pods CrictlPods
	if err := c.run(logger, &pods, "pods", "-o", "json"); err != nil {
		return nil, err
	}
	return &pods, nil
}

// CrictlPods represents the JSON output of 'crictl pods', limited to the subset we care about.
type CrictlPods struct {
	Items []CrictlPod `json:"items"`
}
type CrictlPod struct {
	ID       string            `json:"id"`
	Metadata CrictlPodMetadata `json:"metadata"`
}
type CrictlPodMetadata struct {
	UID string `json:"uid"`
}

// Ps calls 'crictl ps -p <podID>' and, if successful, returns the parsed output
//
// This command lists the containers in the pod.
func (c *Crictl) Ps(logger *zap.Logger, podID string) (*CrictlContainers, error) {
	var containers CrictlContainers
	if err := c.run(logger, &containers, "ps", "-p", podID, "-o", "json"); err != nil {
		return nil, err
	}
	return &containers, nil
}

// CrictlContainers represents the JSON output of 'crictl ps', limited to the subset we care about.
type CrictlContainers struct {
	Containers []CrictlContainer `json:"containers"`
}
type CrictlContainer struct {
	ID       string                  `json:"id"`
	Metadata CrictlContainerMetadata `json:"metadata"`
}
type CrictlContainerMetadata struct {
	Name string `json:"name"`
}

func (c *Crictl) Update(logger *zap.Logger, containerID string, values CrictlContainerUpdate) error {
	return c.run(
		logger, nil,
		"update",
		"--cpu-share", strconv.Itoa(values.cpuShares),
		"--cpu-quota", strconv.FormatInt(values.cpuQuota, 10),
		"--cpu-period", strconv.FormatInt(values.cpuPeriod, 10),
		containerID,
	)
}

type CrictlContainerUpdate struct {
	cpuShares int
	cpuQuota  int64
	cpuPeriod int64
}

func (c *Crictl) Inspect(logger *zap.Logger, containerID string) (*CrictlContainerInspect, error) {
	var container CrictlContainerInspect
	if err := c.run(logger, &container, "inspect", containerID); err != nil {
		return nil, err
	}
	return &container, nil
}

func (c *Crictl) InspectPod(logger *zap.Logger, podID string) (*CrictlPodInspect, error) {
	var pod CrictlPodInspect
	if err := c.run(logger, &pod, "inspectp", podID); err != nil {
		return nil, err
	}
	return &pod, nil
}

// CrictlContainerInspect represents the JSON output of 'crictl inspect', limited to the subset we
// care about.
type CrictlContainerInspect struct {
	Info CrictlContainerInfo `json:"info"`
}
type CrictlContainerInfo struct {
	RuntimeSpec CrictlContainerRuntimeSpec `json:"runtimeSpec"`
}
type CrictlContainerRuntimeSpec struct {
	Linux CrictlContainerRuntimeSpecLinux `json:"linux"`
}
type CrictlContainerRuntimeSpecLinux struct {
	Resources CrictlContainerResources `json:"resources"`
}
type CrictlContainerResources struct {
	CPU CrictlContainerResourcesCPU `json:"cpu"`
}
type CrictlContainerResourcesCPU struct {
	Period uint64 `json:"period"`
	Quota  uint64 `json:"quota"`
	Shares uint64 `json:"shares"`
}

// CrictlPodInspect represents the JSON output of `crictl inspectp`, limited to the subset we care about.
type CrictlPodInspect struct {
	Info CrictlPodInfo `json:"info"`
}

type CrictlPodInfo struct {
	Config CrictlPodConfig `json:"config"`
}

type CrictlPodConfig struct {
	Linux CrictlPodLinux `json:"linux"`
}

type CrictlPodLinux struct {
	CgroupParent string `json:"cgroup_parent"`
}

func (c *Crictl) run(logger *zap.Logger, output any, args ...string) error {
	actualArgs := []string{
		"--runtime-endpoint",
		c.endpoint,
	}
	actualArgs = append(actualArgs, args...)

	logger.Info("running crictl", zap.Strings("args", actualArgs))

	cmd := exec.Command("/usr/bin/crictl", actualArgs...)

	stderr, err := os.OpenFile("/dev/stderr", os.O_RDWR, 0 /* unused */)
	if err != nil {
		panic(fmt.Errorf("failed to open /dev/stderr: %w", err))
	}
	cmd.Stderr = stderr

	if output == nil {
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to run command: %w", err)
		}
		return nil
	} else {
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("failed to run command: %w", err)
		}

		if err := json.Unmarshal(out, output); err != nil {
			return fmt.Errorf("error parsing JSON: %w", err)
		}
		return nil
	}
}
