package informant

// Informant-specific usage and logic around cgroups, using CgroupManager.

/*

import (
	"fmt"
	"sync"
	"time"

	sysinfo "github.com/elastic/go-sysinfo"
	sysinfotypes "github.com/elastic/go-sysinfo/types"
	"go.uber.org/zap"

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

	requestUpscale func(*zap.Logger)
}

// CgroupConfig provides some configuration options for State cgroup handling
type CgroupConfig struct {
	// OOMBufferBytes gives the target difference between the total memory reserved for the cgroup
	// and the value of the cgroup's memory.high.
	//
	// In other words, memory.high + OOMBufferBytes will equal the total memory that the cgroup may
	// use (equal to system memory, minus whatever's taken out for the file cache).
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

// mib is a helper function to format a quantity of bytes as a string
func mib(bytes uint64) string {
	return fmt.Sprintf("%g MiB", float64(bytes)/float64(1<<20))
}

// setMemoryLimits updates the cgroup's value of memory.high and memory.max, according to the memory
// made available to the cgroup.
//
// This method MUST be called while holding s.updateMemLimitsLock.
func (s *CgroupState) setMemoryLimits(logger *zap.Logger, availableMemory uint64) error {
	newMemHigh := s.config.calculateMemoryHighValue(availableMemory)

	logger.Info("Setting cgroup memory.high",
		zap.String("availableMemory", mib(availableMemory)),
		zap.String("target", mib(newMemHigh)),
	)

	s.mgr.MemoryHighEvent.Consume()

	memLimits := memoryLimits{
		highBytes: newMemHigh,
		maxBytes:  availableMemory,
	}
	if err := s.mgr.SetMemLimits(memLimits); err != nil {
		return fmt.Errorf("Error setting cgroup %q memory limits: %w", s.mgr.name, err)
	}

	logger.Info("Successfully set cgroup memory limits")
	return nil
}

// handleCgroupSignals is an internal function that handles "memory high" signals from the cgroup
func (s *CgroupState) handleCgroupSignalsLoop(logger *zap.Logger, config CgroupConfig) {
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
			logger.Info("Received upscale event")
			s.mgr.MemoryHighEvent.Consume()

			// note: Don't reset the timers. We still want to be precise about our rate limit, if
			// upscale events are happening very frequently.

		case <-s.mgr.MemoryHighEvent.Recv():
			select {
			case <-waitToFreeze.C:
				var err error

				// Freeze the cgroup and request more memory (maybe duplicate - that'll be handled
				// internally so we're not spamming the agent)
				waitingOnUpscale, err = s.handleMemoryHighEvent(logger, config)
				if err != nil {
					panic(fmt.Errorf("Error handling memory high event: %w", err))
				}
				waitToFreeze.Reset(time.Duration(config.DoNotFreezeMoreOftenThanMillis) * time.Millisecond)
			default:
				if !waitingOnUpscale {
					logger.Info("Received memory.high event, but too soon to re-freeze. Requesting upscaling")

					// Too soon after the last freeze, but there's currently no unsatisfied
					// upscaling requests. We should send a new one:
					func() {
						s.updateMemLimitsLock.Lock()
						defer s.updateMemLimitsLock.Unlock()

						// Double-check we haven't already been upscaled (can happen if the agent
						// independently decides to upscale us again)
						select {
						case <-s.upscaleEventsRecvr.Recv():
							logger.Info("No need to request upscaling because we were already upscaled")
							return
						default:
							s.requestUpscale(logger)
						}
					}()
				} else {
					// Maybe increase memory.high to reduce throttling:
					select {
					case <-waitToIncreaseMemoryHigh.C:
						logger.Info("Received memory.high event, too soon to re-freeze, but increasing memory.high")

						func() {
							s.updateMemLimitsLock.Lock()
							defer s.updateMemLimitsLock.Unlock()

							// Double-check we haven't already been upscaled (can happen if the
							// agent independently decides to upscale us again)
							select {
							case <-s.upscaleEventsRecvr.Recv():
								logger.Info("No need to update memory.high because we were already upscaled")
								return
							default:
								s.requestUpscale(logger)
							}

							memHigh, err := s.mgr.FetchMemoryHighBytes()
							if err != nil {
								panic(fmt.Errorf("Error fetching memory.high: %w", err))
							} else if memHigh == nil {
								panic(fmt.Errorf("memory.high is unset (equal to 'max') but should have been set to a value already"))
							}

							newMemHigh := *memHigh + config.MemoryHighIncreaseByBytes
							logger.Info(
								"Updating memory.high",
								zap.String("current", mib(*memHigh)),
								zap.String("target", mib(newMemHigh)),
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
func (s *CgroupState) handleMemoryHighEvent(logger *zap.Logger, config CgroupConfig) (waitingOnUpscale bool, _ error) {
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
		logger.Info("Skipping memory.high event because there was an upscale event")
		return false, nil
	default:
	}

	logger.Info("Received memory high event. Freezing cgroup")

	// Immediately freeze the cgroup before doing anything else.
	if err := s.mgr.Freeze(); err != nil {
		return false, fmt.Errorf("Error freezing cgroup: %w", err)
	}

	startTime := time.Now()

	// Start a timer for the maximum time we'll leave the cgroup frozen for:
	maxWaitBeforeThaw := time.Millisecond * time.Duration(config.MaxUpscaleWaitMillis)
	mustThaw := time.After(maxWaitBeforeThaw)

	logger.Info(fmt.Sprintf("Sending request for immediate upscaling, waiting for at most %s", maxWaitBeforeThaw))

	s.requestUpscale(logger)

	// Unlock before waiting:
	locked = false
	s.updateMemLimitsLock.Unlock()

	var upscaled bool

	select {
	case <-s.upscaleEventsRecvr.Recv():
		totalWait := time.Since(startTime)
		logger.Info("Received notification that upscale occurred", zap.Duration("totalWait", totalWait))
		upscaled = true
	case <-mustThaw:
		totalWait := time.Since(startTime)
		logger.Info("Timed out waiting for upscale", zap.Duration("totalWait", totalWait))
	}

	logger.Info("Thawing cgroup")
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

*/
