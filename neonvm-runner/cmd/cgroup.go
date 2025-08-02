package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/opencontainers/runtime-spec/specs-go"
	"go.uber.org/zap"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

const (
	// cgroupPeriod is the period for evaluating cgroup quota
	// in microseconds. Min 1000 microseconds, max 1 second
	cgroupPeriod     = uint64(100000)
	cgroupMountPoint = "/sys/fs/cgroup"

	// cpuLimitOvercommitFactor sets the amount above the VM's spec.guest.cpus.use that we set the
	// QEMU cgroup's CPU limit to. e.g. if cpuLimitOvercommitFactor = 3 and the VM is using 0.5
	// CPUs, we set the cgroup to limit QEMU+VM to 1.5 CPUs.
	//
	// This exists because setting the cgroup exactly equal to the VM's CPU value is overly
	// pessimistic, results in a lot of unused capacity on the host, and particularly impacts
	// operations that parallelize between the VM and QEMU, like heavy disk access.
	//
	// See also: https://neondb.slack.com/archives/C03TN5G758R/p1693462680623239
	cpuLimitOvercommitFactor = 4
)

// setupQEMUCgroup sets up a cgroup for us to run QEMU in, returning the path of that cgroup
func setupQEMUCgroup(logger *zap.Logger, selfPodName string, initialCPU vmv1.MilliCPU) (string, error) {
	selfCgroupPath, err := getSelfCgroupPath(logger)
	if err != nil {
		return "", fmt.Errorf("failed to get self cgroup path: %w", err)
	}
	// Sometimes we'll get just '/' as our cgroup path. If that's the case, we should reset it so
	// that the cgroup '/neonvm-qemu-...' still works.
	if selfCgroupPath == "/" {
		selfCgroupPath = ""
	}
	// ... but also we should have some uniqueness just in case, so we're not sharing a root level
	// cgroup if that *is* what's happening. This *should* only be relevant for local clusters.
	//
	// We don't want to just use the VM spec's .status.PodName because during migrations that will
	// be equal to the source pod, not this one, which may be... somewhat confusing.
	cgroupPath := fmt.Sprintf("%s/neonvm-qemu-%s", selfCgroupPath, selfPodName)

	logger.Info("Determined QEMU cgroup path", zap.String("path", cgroupPath))

	if err := setCgroupLimit(logger, initialCPU, cgroupPath); err != nil {
		return "", fmt.Errorf("failed to set cgroup limit: %w", err)
	}

	return cgroupPath, nil
}

func getSelfCgroupPath(logger *zap.Logger) (string, error) {
	// There's some fun stuff here. For general information, refer to `man 7 cgroups` - specifically
	// the section titled "/proc files" - for "/proc/cgroups" and "/proc/pid/cgroup".
	//
	// In general, the idea is this: If we start QEMU outside of the cgroup for the container we're
	// running in, we run into multiple problems - it won't show up in metrics, and we'll have to
	// clean up the cgroup ourselves. (not good!).
	//
	// So we'd like to start it in the same cgroup - the question is just how to find the name of
	// the cgroup we're running in. Thankfully, this is visible in `/proc/self/cgroup`!
	// The only difficulty is the file format.
	//
	// In cgroup v1 (which is what we have on EKS [as of 2023-07]), the contents of
	// /proc/<pid>/cgroup tend to look like:
	//
	//   11:cpuset:/path/to/cgroup
	//   10:perf_event:/path/to/cgroup
	//   9:hugetlb:/path/to/cgroup
	//   8:blkio:/path/to/cgroup
	//   7:pids:/path/to/cgroup
	//   6:freezer:/path/to/cgroup
	//   5:memory:/path/to/cgroup
	//   4:net_cls,net_prio:/path/to/cgroup
	//   3:cpu,cpuacct:/path/to/cgroup
	//   2:devices:/path/to/cgroup
	//   1:name=systemd:/path/to/cgroup
	//
	// For cgroup v2, we have:
	//
	//   0::/path/to/cgroup
	//
	// The file format is defined to have 3 fields, separated by colons. The first field gives the
	// Hierarchy ID, which is guaranteed to be 0 if the cgroup is part of a cgroup v2 ("unified")
	// hierarchy.
	// The second field is a comma-separated list of the controllers. Or, if it's cgroup v2, nothing.
	// The third field is the "pathname" of the cgroup *in its hierarchy*, relative to the mount
	// point of the hierarchy.
	//
	// So we're looking for EITHER:
	//  1. an entry like '<N>:<controller...>,cpu,<controller...>:/path/to/cgroup (cgroup v1); OR
	//  2. an entry like '0::/path/to/cgroup', and we'll return the path (cgroup v2)
	// We primarily care about the 'cpu' controller, so for cgroup v1, we'll search for that instead
	// of e.g. "name=systemd", although it *really* shouldn't matter because the paths will be the
	// same anyways.
	//
	// Now: Technically it's possible to run a "hybrid" system with both cgroup v1 and v2
	// hierarchies. If this is the case, it's possible for /proc/self/cgroup to show *some* v1
	// hierarchies attached, in addition to the v2 "unified" hierarchy, for the same cgroup. To
	// handle this, we should look for a cgroup v1 "cpu" controller, and if we can't find it, try
	// for the cgroup v2 unified entry.
	//
	// As far as I (@sharnoff) can tell, the only case where that might actually get messed up is if
	// the CPU controller isn't available for the cgroup we're running in, in which case there's
	// nothing we can do about it! (other than e.g. using a cgroup higher up the chain, which would
	// be really bad tbh).

	// ---
	// On to the show!

	procSelfCgroupContents, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("failed to read /proc/self/cgroup: %w", err)
	}
	logger.Info("Read /proc/self/cgroup", zap.String("contents", string(procSelfCgroupContents)))

	// Collect all candidate paths from the lines of the file. If there isn't exactly one,
	// something's wrong and we should make an error.
	var v1Candidates []string
	var v2Candidates []string
	for lineno, line := range strings.Split(string(procSelfCgroupContents), "\n") {
		if line == "" {
			continue
		}

		// Split into the three ':'-delimited fields
		fields := strings.Split(line, ":")
		if len(fields) != 3 {
			return "", fmt.Errorf("line %d of /proc/self/cgroup did not have 3 colon-delimited fields", lineno+1)
		}

		id := fields[0]
		controllers := fields[1]
		path := fields[2]
		if id == "0" {
			v2Candidates = append(v2Candidates, path)
			continue
		}

		// It's not cgroup v2, otherwise id would have been 0. So, check if the comma-separated list
		// of controllers contains 'cpu' as an entry.
		for _, c := range strings.Split(controllers, ",") {
			if c == "cpu" {
				v1Candidates = append(v1Candidates, path)
				break // ... and then continue to the next loop iteration
			}
		}
	}

	var errMsg string

	// Check v1, then v2
	if len(v1Candidates) == 1 {
		return v1Candidates[0], nil
	} else if len(v1Candidates) != 0 {
		errMsg = "More than one applicable cgroup v1 entry in /proc/self/cgroup"
	} else if len(v2Candidates) == 1 {
		return v2Candidates[0], nil
	} else if len(v2Candidates) != 0 {
		errMsg = "More than one applicable cgroup v2 entry in /proc/self/cgroup"
	} else {
		errMsg = "Couldn't find applicable entry in /proc/self/cgroup"
	}

	return "", errors.New(errMsg)
}

func setCgroupLimit(logger *zap.Logger, r vmv1.MilliCPU, cgroupPath string) error {
	r *= cpuLimitOvercommitFactor

	isV2 := cgroups.Mode() == cgroups.Unified
	period := cgroupPeriod
	// quota may be greater than period if the cgroup is allowed
	// to use more than 100% of a CPU.
	quota := int64(float64(r) / float64(1000) * float64(cgroupPeriod))
	logger.Info(fmt.Sprintf("setting cgroup CPU limit %v %v", quota, period))
	if isV2 {
		resources := cgroup2.Resources{
			CPU: &cgroup2.CPU{
				Max: cgroup2.NewCPUMax(&quota, &period),
			},
		}
		_, err := cgroup2.NewManager(cgroupMountPoint, cgroupPath, &resources)
		if err != nil {
			return err
		}
	} else {
		_, err := cgroup1.New(cgroup1.StaticPath(cgroupPath), &specs.LinuxResources{
			CPU: &specs.LinuxCPU{
				Quota:  &quota,
				Period: &period,
			},
		})
		if err != nil {
			return err
		}
	}

	return nil
}
