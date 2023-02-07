package informant

// Informant-specific usage and logic around cgroups, using CgroupManager.

import (
	"fmt"
	"sync"
	"time"

	sysinfo "github.com/elastic/go-sysinfo"
	sysinfotypes "github.com/elastic/go-sysinfo/types"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/util"
)

// CgroupState provides the high-level cgroup handling logic, building upon the low-level plumbing
// provided by CgroupManager.
type CgroupState struct {
	// updateMemHighLock guards access to setting the cgroup's memory.high
	updateMemHighLock sync.Mutex

	mgr    *CgroupManager
	config CgroupConfig

	upscaleEventsSendr util.CondChannelSender
	upscaleEventsRecvr util.CondChannelReceiver

	requestUpscale func()
}

// CgroupConfig provides some configuration options for State cgroup handling
type CgroupConfig struct {
	// OOMBufferBytes gives the amount of memory, in bytes, below system memory that the cgroup's
	// memory.high should be set to.
	//
	// In other words, memory.high + OOMBufferBytes will equal total system memory.
	OOMBufferBytes uint64

	// SysBufferBytes gives the estimated amount of memory, in bytes, that the kernel uses before
	// handing out the rest to userspace. This value is the estimated difference between the
	// *actual* physical memory and the amount reported by `grep MemTotal /proc/meminfo`.
	//
	// For more information, refer to `man 5 proc`, which defines MemTotal as "Total usable RAM
	// (i.e., physical RAM minus a few reserved bits and the kernel binary code)".
	//
	// We only use SysBufferBytes when calculating the system memory from the *external* memory
	// size, rather than the self-reported memory size, according to the kernel.
	//
	// TODO: this field is only necessary while we still have to trust the autoscaler-agent's
	// upscale resource amounts (because we might not *actually* have been upscaled yet). This field
	// should be removed once we have a better solution there.
	SysBufferBytes uint64

	// MemoryHighBufferBytes gives the amount of memory, in bytes, below a proposed new value for
	// memory.high that the cgroup's memory usage must be for us to downscale
	//
	// In other words, we can downscale only when:
	//
	//   memory.current + MemoryHighBufferBytes < (proposed) memory.high
	//
	// TODO: there's some minor issues with this approach -- in particular, that we might have
	// memory in use by the kernel's page cache that we're actually ok with getting rid of.
	MemoryHighBufferBytes uint64

	// MaxUpscaleWaitMillis gives the maximum duration, in milliseconds, that we're allowed to pause
	// the cgroup for while waiting for the autoscaler-agent to upscale us
	MaxUpscaleWaitMillis uint
}

// ReceivedUpscale notifies s.upscaleEventsRecvr
//
// Typically, (*AgentSet).ReceivedUpscale() is also called alongside this method.
func (s *CgroupState) ReceivedUpscale() {
	s.upscaleEventsSendr.Send()
}

// setMemoryHigh updates the cgroup's value of memory.high, according to the current total
// system memory.
//
// This method MUST be called while holding s.updateMemHighLock.
func (s *CgroupState) setMemoryHigh() error {
	var newMemHigh uint64
	mib := float64(1 << 20) // Size of 1 MiB = 2^20 = 1 << 20

	systemMem, err := getTotalSystemMemory()
	if err != nil {
		return fmt.Errorf("Error getting system memory: %w", err)
	}

	newMemHigh = s.config.calculateMemoryHighValue(systemMem.Total)

	klog.Infof(
		"Total system memory is %d bytes (%g MiB). Setting cgroup memory.high to %d bytes (%g MiB)",
		systemMem.Total, float64(systemMem.Total)/mib, newMemHigh, float64(newMemHigh)/mib,
	)

	s.mgr.MemoryHighEvent.Consume()

	if err := s.mgr.SetHighMem(newMemHigh); err != nil {
		return fmt.Errorf("Error setting cgroup memory.high: %w", err)
	}

	klog.Infof("Successfully set cgroup memory.high")
	return nil
}

// handleCgroupSignals is an internal function that handles "memory high" signals from the cgroup
func (s *CgroupState) handleCgroupSignalsLoop(config CgroupConfig) {
	// FIXME: the process might exit after freezing the cgroup but before thawing it. This isn't
	// really feasible to fix here; the parent process should handle the "thaw if frozen" check on
	// exit.
	//
	// FIXME: we should have "proper" error handling instead of just panicking. It's hard to
	// determine what the correct behavior should be if a cgroup operation fails, though.

	// FIXME: make minWaitDuration configurable.
	minWaitDuration := time.Second
	mustWait := false

	for {
		if mustWait {
			timer := time.After(minWaitDuration)
			ignored := 0
			for {
				select {
				case <-s.upscaleEventsRecvr.Recv():
					ignored += 1
					continue
				case <-timer:
				}
				break
			}
			if ignored != 0 {
				plural := ""
				if ignored > 1 {
					plural = "s"
				}
				klog.Warningf("Ignoring memory event%s that occurred while respecting minimum wait", plural)
			}
		}

		// Wait for a new signal
		select {
		case err := <-s.mgr.ErrCh:
			panic(fmt.Errorf("Error listening for cgroup signals: %w", err))
		case <-s.upscaleEventsRecvr.Recv():
			klog.Infof("Received upscale event")
			s.mgr.MemoryHighEvent.Consume()
		case <-s.mgr.MemoryHighEvent.Recv():
			if err := s.handleMemoryHighEvent(config); err != nil {
				panic(fmt.Errorf("Error handling memory high event: %w", err))
			}
		}

	}
}

// handleMemoryHighEvent performs the "freeze cgroup, request upscale, thaw cgroup" operation, in
// response to a "memory high" event for the cgroup
//
// This method waits on s.agents.UpscaleEvents(), so incorrect behavior will occur if it's called at
// the same time as anything else that waits on the upscale events. For that reason, both this
// function and s.setMemoryHigh() are dispatched from within s.handleCgroupSignalsLoop().
func (s *CgroupState) handleMemoryHighEvent(config CgroupConfig) error {
	klog.Infof("Received memory high event. Freezing cgroup")

	// Immediately freeze the cgroup before doing anything else.
	if err := s.mgr.Freeze(); err != nil {
		return fmt.Errorf("Error freezing cgroup: %w", err)
	}

	startTime := time.Now()

	// Start a timer for the maximum time we'll leave the cgroup frozen for:
	maxWaitBeforeThaw := time.Millisecond * time.Duration(config.MaxUpscaleWaitMillis)
	mustThaw := time.After(maxWaitBeforeThaw)

	klog.Infof("Sending request for immediate upscaling, waiting for at most %s", maxWaitBeforeThaw)

	s.requestUpscale()

	select {
	case <-s.upscaleEventsRecvr.Recv():
		totalWait := time.Since(startTime)
		klog.Infof("Received notification that upscale occurred after %s. Thawing cgroup", totalWait)
	case <-mustThaw:
		totalWait := time.Since(startTime)
		klog.Warningf("Timed out after %s waiting for upscale. Thawing cgroup", totalWait)
	}

	if err := s.mgr.Thaw(); err != nil {
		return fmt.Errorf("Error thawing cgroup: %w", err)
	}

	s.mgr.MemoryHighEvent.Consume()

	return nil
}

// calculateMemoryHighValue calculates the new value for the cgroup's memory.high based on the total
// system memory.
func (c *CgroupConfig) calculateMemoryHighValue(totalSystemMem uint64) uint64 {
	return util.SaturatingSub(totalSystemMem, c.OOMBufferBytes)
}

// getCgroupCurrentMemory fetches the current total memory usgae of the cgroup, in bytes
func (s *CgroupState) getCurrentMemory() (uint64, error) {
	return s.mgr.CurrentMemoryUsage()
}

// getTotalSystemMemory fetches the system's total memory, in bytes
func getTotalSystemMemory() (*sysinfotypes.HostMemoryInfo, error) {
	host, err := sysinfo.Host()
	if err != nil {
		return nil, fmt.Errorf("Error getting host info: %w", err)
	}

	mem, err := host.Memory()
	if err != nil {
		return nil, fmt.Errorf("Error getting host memory info: %w", err)
	}

	return mem, nil
}
