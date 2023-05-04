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
	// updateMemLimitsLock guards access to setting the cgroup's memory.high and memory.max
	updateMemLimitsLock sync.Mutex

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

	// DoNotFreezeMoreOftenThanMillis gives a required minimum time, in milliseconds, that we must
	// wait before re-freezing the cgroup while waiting for the autoscaler-agent to upscale us.
	DoNotFreezeMoreOftenThanMillis uint

	// MemoryHighIncreaseByBytes gives the amount of memory, in bytes, that we should periodically
	// increase memory.high by while waiting for the autoscaler-agent to upscale us.
	//
	// This exists to avoid the excessive throttling that happens when a cgroup is above its
	// memory.high for too long. See more here:
	// https://github.com/neondatabase/autoscaling/issues/44#issuecomment-1522487217
	MemoryHighIncreaseByBytes uint64

	// MemoryHighIncreaseEveryMillis gives the period, in milliseconds, at which we should
	// repeatedly increase the value of the cgroup's memory.high while we're waiting on upscaling
	// and memory.high is still being hit.
	//
	// Technically speaking, this actually serves as a rate limit to moderate responding to
	// memory.high events, but these are roughly equivalent if the process is still allocating
	// memory.
	MemoryHighIncreaseEveryMillis uint
}

// ReceivedUpscale notifies s.upscaleEventsRecvr
//
// Typically, (*AgentSet).ReceivedUpscale() is also called alongside this method.
func (s *CgroupState) ReceivedUpscale() {
	s.upscaleEventsSendr.Send()
}

// setMemoryLimits updates the cgroup's value of memory.high and memory.max, according to the memory
// made available to the cgroup.
//
// This method MUST be called while holding s.updateMemLimitsLock.
func (s *CgroupState) setMemoryLimits(availableMemory uint64) error {
	var newMemHigh uint64
	mib := float64(1 << 20) // Size of 1 MiB = 2^20 = 1 << 20

	newMemHigh = s.config.calculateMemoryHighValue(availableMemory)

	klog.Infof(
		"Total memory available for cgroup is %d bytes (%g MiB). Setting cgroup memory.high to %d bytes (%g MiB)",
		availableMemory, float64(availableMemory)/mib, newMemHigh, float64(newMemHigh)/mib,
	)

	s.mgr.MemoryHighEvent.Consume()

	memLimits := memoryLimits{
		highBytes: newMemHigh,
		maxBytes:  availableMemory,
	}
	if err := s.mgr.SetMemLimits(memLimits); err != nil {
		return fmt.Errorf("Error setting cgroup memory limits: %w", err)
	}

	klog.Infof("Successfully set cgroup memory limits")
	return nil
}

// handleCgroupSignals is an internal function that handles "memory high" signals from the cgroup
func (s *CgroupState) handleCgroupSignalsLoop(config CgroupConfig) {
	// FIXME: we should have "proper" error handling instead of just panicking. It's hard to
	// determine what the correct behavior should be if a cgroup operation fails, though.

	waitingOnUpscale := false

	waitToIncreaseMemoryHigh := time.NewTimer(0)
	waitToFreeze := time.NewTimer(0)

	// hey! Before reading this function, have a read through the fields of CgroupConfig - it'll be
	// hard to understand the control flow that's going on here without that.
	for {
		// Wait for a new signal
		select {
		case err := <-s.mgr.ErrCh:
			panic(fmt.Errorf("Error listening for cgroup signals: %w", err))
		case <-s.upscaleEventsRecvr.Recv():
			klog.Infof("Received upscale event")
			s.mgr.MemoryHighEvent.Consume()

			// note: Don't reset the timers. We still want to be precise about our rate limit, if
			// upscale events are happening very frequently.

		case <-s.mgr.MemoryHighEvent.Recv():
			select {
			case <-waitToFreeze.C:
				var err error

				// Freeze the cgroup and request more memory (maybe duplicate - that'll be handled
				// internally so we're not spamming the agent)
				waitingOnUpscale, err = s.handleMemoryHighEvent(config)
				if err != nil {
					panic(fmt.Errorf("Error handling memory high event: %w", err))
				}
				waitToFreeze.Reset(time.Duration(config.DoNotFreezeMoreOftenThanMillis) * time.Millisecond)
			default:
				if !waitingOnUpscale {
					klog.Infof("Received memory.high event, but too soon to re-freeze. Requesting upscaling")

					// Too soon after the last freeze, but there's currently no unsatisfied
					// upscaling requests. We should send a new one:
					func() {
						s.updateMemLimitsLock.Lock()
						defer s.updateMemLimitsLock.Unlock()

						// Double-check we haven't already been upscaled (can happen if the agent
						// independently decides to upscale us again)
						select {
						case <-s.upscaleEventsRecvr.Recv():
							klog.Infof("No need to request upscaling because we were already upscaled")
							return
						default:
							s.requestUpscale()
						}
					}()
				} else {
					// Maybe increase memory.high to reduce throttling:
					select {
					case <-waitToIncreaseMemoryHigh.C:
						klog.Infof("Received memory.high event, too soon to re-freeze, but increasing memory.high")

						func() {
							s.updateMemLimitsLock.Lock()
							defer s.updateMemLimitsLock.Unlock()

							// Double-check we haven't already been upscaled (can happen if the
							// agent independently decides to upscale us again)
							select {
							case <-s.upscaleEventsRecvr.Recv():
								klog.Infof("No need to update memory.high because we were already upscaled")
								return
							default:
								s.requestUpscale()
							}

							memHigh, err := s.mgr.FetchMemoryHighBytes()
							if err != nil {
								panic(fmt.Errorf("Error fetching memory.high: %w", err))
							} else if memHigh == nil {
								panic(fmt.Errorf("memory.high is unset (equal to 'max') but should have been set to a value already"))
							}

							newMemHigh := *memHigh + config.MemoryHighIncreaseByBytes
							klog.Infof(
								"Updating memory.high from %g MiB -> %g MiB",
								float64(*memHigh)/float64(1<<20), float64(newMemHigh)/float64(1<<20),
							)

							if err := s.mgr.SetMemHighBytes(newMemHigh); err != nil {
								panic(fmt.Errorf("Error setting memory limits: %w", err))
							}
						}()

						waitToIncreaseMemoryHigh.Reset(time.Duration(config.MemoryHighIncreaseEveryMillis) * time.Millisecond)
					default:
						// Can't do anything.
					}
				}
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
func (s *CgroupState) handleMemoryHighEvent(config CgroupConfig) (waitingOnUpscale bool, _ error) {
	locked := true
	s.updateMemLimitsLock.Lock()
	defer func() {
		if locked {
			s.updateMemLimitsLock.TryLock()
		}
	}()

	// If we've actually already received an upscale event, then we should ignore this memory.high
	// event for the time being:
	select {
	case <-s.upscaleEventsRecvr.Recv():
		klog.Infof("Skipping memory.high event because there was an upscale event")
		return false, nil
	default:
	}

	klog.Infof("Received memory high event. Freezing cgroup")

	// Immediately freeze the cgroup before doing anything else.
	if err := s.mgr.Freeze(); err != nil {
		return false, fmt.Errorf("Error freezing cgroup: %w", err)
	}

	startTime := time.Now()

	// Start a timer for the maximum time we'll leave the cgroup frozen for:
	maxWaitBeforeThaw := time.Millisecond * time.Duration(config.MaxUpscaleWaitMillis)
	mustThaw := time.After(maxWaitBeforeThaw)

	klog.Infof("Sending request for immediate upscaling, waiting for at most %s", maxWaitBeforeThaw)

	s.requestUpscale()

	// Unlock before waiting:
	locked = false
	s.updateMemLimitsLock.Unlock()

	var upscaled bool

	select {
	case <-s.upscaleEventsRecvr.Recv():
		totalWait := time.Since(startTime)
		klog.Infof("Received notification that upscale occurred after %s. Thawing cgroup", totalWait)
		upscaled = true
	case <-mustThaw:
		totalWait := time.Since(startTime)
		klog.Warningf("Timed out after %s waiting for upscale. Thawing cgroup", totalWait)
	}

	if err := s.mgr.Thaw(); err != nil {
		return false, fmt.Errorf("Error thawing cgroup: %w", err)
	}

	s.mgr.MemoryHighEvent.Consume()

	return !upscaled, nil
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
