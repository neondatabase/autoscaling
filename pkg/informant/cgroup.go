package informant

// Informant-specific usage and logic around cgroups, using CgroupManager.

import (
	"fmt"
	"sync"
	"time"

	sysinfo "github.com/elastic/go-sysinfo"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/util"
)

// CgroupState provides the high-level cgroup handling logic, building upon the low-level plumbing
// provided by CgroupManager.
type CgroupState struct {
	// updateLock guards access to *external* operations on the cgroup - like setting memory.high.
	//
	// All fields of this struct are immutable, so do not require holding updateLock.
	updateLock sync.Mutex

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
// This method MUST be called while holding s.updateLock, or without s.handleCgroupSignals()
// running (or any other method that may call this).
func (s *CgroupState) setMemoryHigh() error {
	var newMemHigh uint64
	mib := float64(1 << 20) // Size of 1 MiB = 2^20 = 1 << 20

	totalSystemMem, err := getTotalSystemMemory()
	if err != nil {
		return fmt.Errorf("Error getting system memory: %w", err)
	}

	newMemHigh = s.config.calculateMemoryHighValue(totalSystemMem)

	klog.Infof(
		"Total system memory is %d bytes (%v MiB). Setting cgroup memory.high to %d bytes (%v MiB)",
		totalSystemMem, float64(totalSystemMem)/mib, newMemHigh, float64(newMemHigh)/mib,
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
	//
	// FIXME: enforce a minimum wait period between requesting scale-ups (otherwise can be bad when
	// memory usage is oscillating around memory.high but there aren't available resources)

	for {
		// Wait for a new signal
		select {
		case err := <-s.mgr.ErrCh:
			panic(fmt.Errorf("Error listening for cgroup signals: %w", err))
		case <-s.upscaleEventsRecvr.Recv():
			klog.Infof("Received upscale event. Updating cgroup memory.high")
			func() {
				s.updateLock.Lock()
				defer s.updateLock.Unlock()

				if err := s.setMemoryHigh(); err != nil {
					panic(fmt.Errorf("Error handling upscale signal: %w", err))
				}
			}()
		case <-s.mgr.MemoryHighEvent.Recv():
			// Double-check that there isn't currently an upscale event.
			select {
			case <-s.upscaleEventsRecvr.Recv():
				klog.Warningf("Received memory high event, but also upscale event. Handling upscale and ignoring memory high")
				func() {
					s.updateLock.Lock()
					defer s.updateLock.Unlock()

					if err := s.setMemoryHigh(); err != nil {
						panic(fmt.Errorf("Error handling upscale signal: %w", err))
					}
				}()
			default:
				func() {
					s.updateLock.Lock()
					defer s.updateLock.Unlock()

					if err := s.handleMemoryHighEvent(config); err != nil {
						panic(fmt.Errorf("Error handling memory high event: %w", err))
					}
				}()
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
//
// This method MUST be called while holding s.updateLock.
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
		klog.Infof(
			"Received notification of upscale after %s. Updating memory.high before thawing cgroup",
			totalWait,
		)

		if err := s.setMemoryHigh(); err != nil {
			return fmt.Errorf("Error setting cgroup memory.high: %w", err)
		}

		klog.Infof("cgroup's memory.high updated. Thawing cgroup")
	case <-mustThaw:
		totalWait := time.Since(startTime)
		klog.Warningf("Timed out after %s waiting for upscale. Thawing cgroup", totalWait)
	}

	if err := s.mgr.Thaw(); err != nil {
		return fmt.Errorf("Error thawing cgroup: %w", err)
	}

	return nil
}

// calculateMemoryHighValue calculates the new value for the cgroup's memory.high based on the total
// system memory.
func (c *CgroupConfig) calculateMemoryHighValue(totalSystemMem uint64) uint64 {
	return util.SaturatingSub(totalSystemMem, c.OOMBufferBytes)
}

// getCgroupCurrentMemory fetches the current total memory usgae of the cgroup, in bytes
//
// Callers are not required to hold s.updateLock, but it may be useful to do so.
func (s *CgroupState) getCurrentMemory() (uint64, error) {
	return s.mgr.CurrentMemoryUsage()
}

// getTotalSystemMemory fetches the system's total memory, in bytes
func getTotalSystemMemory() (uint64, error) {
	host, err := sysinfo.Host()
	if err != nil {
		return 0, fmt.Errorf("Error getting host info: %w", err)
	}

	mem, err := host.Memory()
	if err != nil {
		return 0, fmt.Errorf("Error getting host memory info: %w", err)
	}

	return mem.Total, nil
}
