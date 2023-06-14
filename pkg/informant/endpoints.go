package informant

// This file contains the high-level handlers for various HTTP endpoints

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// State is the global state of the informant
type State struct {
	config    StateConfig
	agents    *AgentSet
	cgroup    *CgroupState
	fileCache *FileCacheState

	// memReservedForFileCache stores the amount of memory that's currently reserved for the file
	// cache.
	//
	// This field is mostly used during initialization, where it allows us to pass state from the
	// file cache's startup hook to the cgroup's hook.
	//
	// There's definitely better ways of doing this, but the solution we have will work for now.
	memReservedForFileCache uint64
}

type StateConfig struct {
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
}

// NewStateOpts are individual options provided to NewState
type NewStateOpts struct {
	kind      newStateOptKind
	setFields func(*State)
	post      func(_ *zap.Logger, s *State, memTotal uint64) error
}

type newStateOptKind int

const (
	optCgroup newStateOptKind = iota
	optFileCache
)

// NewState instantiates a new State object, starting whatever background processes might be
// required
//
// Optional configuration may be provided by NewStateOpts - see WithCgroup and
// WithPostgresFileCache.
func NewState(logger *zap.Logger, agents *AgentSet, config StateConfig, opts ...NewStateOpts) (*State, error) {
	if config.SysBufferBytes == 0 {
		panic("invalid StateConfig: SysBufferBytes cannot be zero")
	}

	s := &State{
		config:                  config,
		agents:                  agents,
		cgroup:                  nil,
		fileCache:               nil,
		memReservedForFileCache: 0,
	}
	for _, opt := range opts {
		opt.setFields(s)
	}

	memInfo, err := getTotalSystemMemory()
	if err != nil {
		return nil, fmt.Errorf("Error getting system meminfo: %w", err)
	}

	// We need to process file cache initialization before cgroup initialization, so that the memory
	// allocated to the file cache is appropriately taken into account when we decide the cgroup's
	// memory limits.
	//
	// TODO: this should be made cleaner, but it's mostly ok when there's only two options.
	for _, kind := range []newStateOptKind{optFileCache, optCgroup} {
		for _, opt := range opts {
			if opt.kind == kind {
				if err := opt.post(logger, s, memInfo.Total); err != nil {
					return nil, err
				}
			}
		}
	}

	return s, nil
}

// WithCgroup creates a NewStateOpts that sets its CgroupHandler
//
// This function will panic if the provided CgroupConfig is invalid.
func WithCgroup(cgm *CgroupManager, config CgroupConfig) NewStateOpts {
	if config.OOMBufferBytes == 0 {
		panic("invalid CgroupConfig: OOMBufferBytes == 0")
	} else if config.MaxUpscaleWaitMillis == 0 {
		panic("invalid CgroupConfig: MaxUpscaleWaitMillis == 0")
	}

	return NewStateOpts{
		kind: optCgroup,
		setFields: func(s *State) {
			if s.cgroup != nil {
				panic("WithCgroupHandler option provided more than once")
			}

			upscaleEventsSendr, upscaleEventsRecvr := util.NewCondChannelPair()
			s.cgroup = &CgroupState{
				updateMemLimitsLock: sync.Mutex{},
				mgr:                 cgm,
				config:              config,
				upscaleEventsSendr:  upscaleEventsSendr,
				upscaleEventsRecvr:  upscaleEventsRecvr,
				requestUpscale:      func(l *zap.Logger) { s.agents.RequestUpscale(l) },
			}
		},
		post: func(logger *zap.Logger, s *State, memTotal uint64) error {
			logger = logger.With(zap.String("cgroup", s.cgroup.mgr.name))

			available := memTotal - s.memReservedForFileCache

			// FIXME: This is technically racy across restarts. The sequence would be:
			//  1. Respond "ok" to a downscale request
			//  2. Restart
			//  3. Read system memory
			//  4. Get downscaled (as approved earlier)
			// A potential way to fix this would be writing to a file to record approved downscale
			// operations.
			if err := s.cgroup.setMemoryLimits(logger, available); err != nil {
				return fmt.Errorf("Error setting initial cgroup memory limits: %w", err)
			}
			go s.cgroup.handleCgroupSignalsLoop(logger.Named("signal-handler"), config)
			return nil
		},
	}
}

// WithPostgresFileCache creates a NewStateOpts that enables connections to the postgres file cache
func WithPostgresFileCache(connStr string, config FileCacheConfig) NewStateOpts {
	if err := config.Validate(); err != nil {
		panic(fmt.Errorf("invalid FileCacheConfig: %w", err))
	}

	return NewStateOpts{
		kind: optFileCache,
		setFields: func(s *State) {
			if s.fileCache != nil {
				panic("WithPostgresFileCache option provided more than once")
			}

			s.fileCache = &FileCacheState{
				connStr: connStr,
				config:  config,
			}
		},
		post: func(logger *zap.Logger, s *State, memTotal uint64) error {
			if !config.InMemory {
				panic("file cache not in-memory unimplemented")
			}

			// FIXME: make the timeout configurable
			ctx, cancel := context.WithTimeout(context.TODO(), time.Second)
			defer cancel()

			// Check that we have permissions to set the file cache's size.
			size, err := s.fileCache.GetFileCacheSize(ctx)
			if err != nil {
				return fmt.Errorf("Error getting file cache size: %w", err)
			}

			newSize := s.fileCache.config.CalculateCacheSize(memTotal)
			logger.Info("Setting initial file cache size", zap.String("current", mib(size)), zap.String("target", mib(newSize)))

			// note: Even if newSize == size, we want to explicitly set it *anwyays*, just to verify
			// that we have the necessary permissions to do so.

			actualSize, err := s.fileCache.SetFileCacheSize(ctx, logger, newSize)
			if err != nil {
				return fmt.Errorf("Error setting file cache size: %w", err)
			}
			s.memReservedForFileCache = actualSize

			return nil
		},
	}
}

// RegisterAgent registers a new or updated autoscaler-agent
//
// Returns: body (if successful), status code, error (if unsuccessful)
func (s *State) RegisterAgent(ctx context.Context, logger *zap.Logger, info *api.AgentDesc) (*api.InformantDesc, int, error) {
	logger = logger.With(agentZapField(info.AgentID, info.ServerAddr))

	protoVersion, status, err := s.agents.RegisterNewAgent(logger, info)
	if err != nil {
		return nil, status, err
	}

	desc := api.InformantDesc{
		ProtoVersion: protoVersion,
		MetricsMethod: api.InformantMetricsMethod{
			Prometheus: &api.MetricsMethodPrometheus{Port: PrometheusPort},
		},
	}

	return &desc, 200, nil
}

// HealthCheck is a dummy endpoint that allows the autoscaler-agent to check that (a) the informant
// is up and running, and (b) the agent is still registered.
//
// Returns: body (if successful), status code, error (if unsuccessful)
func (s *State) HealthCheck(ctx context.Context, logger *zap.Logger, info *api.AgentIdentification) (*api.InformantHealthCheckResp, int, error) {
	agent, ok := s.agents.Get(info.AgentID)
	if !ok {
		return nil, 404, fmt.Errorf("No Agent with ID %s registered", info.AgentID)
	} else if !agent.protoVersion.AllowsHealthCheck() {
		return nil, 400, fmt.Errorf("health checks are not supported in protocol version %v", agent.protoVersion)
	}

	return &api.InformantHealthCheckResp{}, 200, nil
}

// TryDownscale tries to downscale the VM's current resource usage, returning whether the proposed
// amount is ok
//
// Returns: body (if successful), status code and error (if unsuccessful)
func (s *State) TryDownscale(ctx context.Context, logger *zap.Logger, target *api.AgentResourceMessage) (*api.DownscaleResult, int, error) {
	currentId := s.agents.CurrentIdStr()
	incomingId := target.Data.Id.AgentID.String()

	// First verify agent's authenticity before doing anything.
	// Note: if the current agent is nil, its id string will be "<nil>", which
	// does not match any valid UUID
	if incomingId != currentId {
		return nil, 400, fmt.Errorf("Agent ID %s is not the active Agent", incomingId)
	}

	// Helper functions for abbreviating returns.
	resultFromStatus := func(ok bool, status string) (*api.DownscaleResult, int, error) {
		return &api.DownscaleResult{Ok: ok, Status: status}, 200, nil
	}
	internalError := func(err error) (*api.DownscaleResult, int, error) {
		logger.Error("Internal error handling downscale request", zap.Error(err))
		return nil, 500, errors.New("Internal error")
	}

	// If we aren't interacting with something that should be adjusted, then we don't need to do anything.
	if s.cgroup == nil && s.fileCache == nil {
		logger.Info("No action needed for downscale (no cgroup or file cache enabled)")
		return resultFromStatus(true, "No action taken (no cgroup or file cache enabled)")
	}

	requestedMem := uint64(target.Data.Memory.Value())
	usableSystemMemory := util.SaturatingSub(requestedMem, s.config.SysBufferBytes)

	// Get the file cache's expected contribution to the memory usage
	var expectedFileCacheMemUsage uint64
	if s.fileCache != nil && s.fileCache.config.InMemory {
		expectedFileCacheMemUsage = s.fileCache.config.CalculateCacheSize(usableSystemMemory)
	}

	mib := float64(1 << 20) // 1 MiB = 2^20 bytes. We'll use this for pretty-printing.

	// Check whether this downscaling would be ok for the cgroup.
	//
	// Also, lock changing the cgroup between the initial calculations and later using them.
	var newCgroupMemHigh uint64
	if s.cgroup != nil {
		s.cgroup.updateMemLimitsLock.Lock()
		defer s.cgroup.updateMemLimitsLock.Unlock()

		newCgroupMemHigh = s.cgroup.config.calculateMemoryHighValue(usableSystemMemory - expectedFileCacheMemUsage)

		current, err := s.cgroup.getCurrentMemory()
		if err != nil {
			return internalError(fmt.Errorf("Error fetching getting cgroup memory: %w", err))
		}

		// For an explanation, refer to the documentation of CgroupConfig.MemoryHighBufferBytes
		//
		// TODO: this should be a method on (*CgroupConfig).
		if newCgroupMemHigh < current+s.cgroup.config.MemoryHighBufferBytes {
			verdict := "Calculated memory.high too low"
			status := fmt.Sprintf(
				"%s: %g MiB (new high) < %g MiB (current usage) + %g MiB (buffer)",
				verdict,
				float64(newCgroupMemHigh)/mib, float64(current)/mib,
				float64(s.cgroup.config.MemoryHighBufferBytes)/mib,
			)

			return resultFromStatus(false, status)
		}
	}

	var statusParts []string

	var fileCacheMemUsage uint64

	// The downscaling has been approved. Downscale the file cache, then the cgroup.
	if s.fileCache != nil && s.fileCache.config.InMemory {
		if !s.fileCache.config.InMemory {
			panic("file cache not in-memory unimplemented")
		}

		// FIXME: make the timeout configurablek
		dbCtx, cancel := context.WithTimeout(ctx, time.Second) // for talking to the DB
		defer cancel()

		actualUsage, err := s.fileCache.SetFileCacheSize(dbCtx, logger, expectedFileCacheMemUsage)
		if err != nil {
			return internalError(fmt.Errorf("Error setting file cache size: %w", err))
		}

		fileCacheMemUsage = actualUsage
		status := fmt.Sprintf("Set file cache size to %g MiB", float64(actualUsage)/mib)
		statusParts = append(statusParts, status)
	}

	if s.cgroup != nil {
		availableMemory := usableSystemMemory - fileCacheMemUsage

		if fileCacheMemUsage != expectedFileCacheMemUsage {
			newCgroupMemHigh = s.cgroup.config.calculateMemoryHighValue(availableMemory)
		}

		memLimits := memoryLimits{
			highBytes: newCgroupMemHigh,
			maxBytes:  availableMemory,
		}

		// TODO: see similar note above. We shouldn't call methods on s.cgroup.mgr from here.
		if err := s.cgroup.mgr.SetMemLimits(memLimits); err != nil {
			return internalError(fmt.Errorf("Error setting cgroup memory.high: %w", err))
		}

		status := fmt.Sprintf(
			"Set cgroup memory.high to %g MiB, of new max %g MiB",
			float64(newCgroupMemHigh)/mib, float64(availableMemory)/mib,
		)
		statusParts = append(statusParts, status)
	}

	return resultFromStatus(true, strings.Join(statusParts, "; "))
}

// NotifyUpscale signals that the VM's resource usage has been increased to the new amount
//
// Returns: body (if successful), status code and error (if unsuccessful)
func (s *State) NotifyUpscale(
	ctx context.Context,
	logger *zap.Logger,
	newResources *api.AgentResourceMessage,
) (*struct{}, int, error) {
	// FIXME: we shouldn't just trust what the agent says
	//
	// Because of race conditions like in <https://github.com/neondatabase/autoscaling/issues/23>,
	// it's possible for us to receive a notification on /upscale *before* NeonVM actually adds the
	// memory.
	//
	// So until the race condition described in #23 is fixed, we have to just trust that the agent
	// is telling the truth, *especially because it might not be*.

	currentId := s.agents.CurrentIdStr()
	incomingId := newResources.Data.Id.AgentID.String()

	// First verify agent's authenticity before doing anything.
	// Note: if the current agent is nil, its id string will be "<nil>", which
	// does not match any valid UUID
	if incomingId != currentId {
		return nil, 400, fmt.Errorf("Agent ID %s is not the active Agent", incomingId)
	}

	// Helper function for abbreviating returns.
	internalError := func(err error) (*struct{}, int, error) {
		logger.Error("Error handling upscale request", zap.Error(err))
		return nil, 500, errors.New("Internal error")
	}

	if s.cgroup == nil && s.fileCache == nil {
		logger.Info("No action needed for upscale (no cgroup or file cache enabled)")
		return &struct{}{}, 200, nil
	}

	newMem := uint64(newResources.Data.Memory.Value())
	usableSystemMemory := util.SaturatingSub(newMem, s.config.SysBufferBytes)

	if s.cgroup != nil {
		s.cgroup.updateMemLimitsLock.Lock()
		defer s.cgroup.updateMemLimitsLock.Unlock()
	}

	s.agents.ReceivedUpscale()

	// Get the file cache's expected contribution to the memory usage
	var fileCacheMemUsage uint64
	if s.fileCache != nil {
		logger := logger.With(zap.String("fileCacheConnstr", s.fileCache.connStr))

		if !s.fileCache.config.InMemory {
			panic("file cache not in-memory unimplemented")
		}

		// FIXME: make the timeout configurable
		dbCtx, cancel := context.WithTimeout(ctx, time.Second) // for talking to the DB
		defer cancel()

		// Update the size of the file cache
		expectedUsage := s.fileCache.config.CalculateCacheSize(usableSystemMemory)

		logger.Info("Updating file cache size", zap.String("target", mib(expectedUsage)), zap.String("totalMemory", mib(newMem)))

		actualUsage, err := s.fileCache.SetFileCacheSize(dbCtx, logger, expectedUsage)
		if err != nil {
			return internalError(fmt.Errorf("Error setting file cache size: %w", err))
		}

		if actualUsage != expectedUsage {
			logger.Warn(
				"File cache size was set to a different value than we wanted",
				zap.String("target", mib(expectedUsage)),
				zap.String("actual", mib(actualUsage)),
			)
		}

		fileCacheMemUsage = actualUsage
	}

	if s.cgroup != nil {
		logger := logger.With(zap.String("cgroup", s.cgroup.mgr.name))

		availableMemory := usableSystemMemory - fileCacheMemUsage

		newMemHigh := s.cgroup.config.calculateMemoryHighValue(availableMemory)
		logger.Info("Updating cgroup memory.high", zap.String("target", mib(newMemHigh)), zap.String("totalMemory", mib(newMem)))

		memLimits := memoryLimits{
			highBytes: newMemHigh,
			maxBytes:  availableMemory,
		}

		if err := s.cgroup.mgr.SetMemLimits(memLimits); err != nil {
			return internalError(fmt.Errorf("Error setting cgroup memory.high: %w", err))
		}

		s.cgroup.upscaleEventsSendr.Send()
	}

	return &struct{}{}, 200, nil
}

// UnregisterAgent unregisters the autoscaler-agent given by info, if it is currently registered
//
// If a different autoscaler-agent is currently registered, this method will do nothing.
//
// Returns: body (if successful), status code and error (if unsuccessful)
func (s *State) UnregisterAgent(ctx context.Context, logger *zap.Logger, info *api.AgentDesc) (*api.UnregisterAgent, int, error) {
	agent, ok := s.agents.Get(info.AgentID)
	if !ok {
		return nil, 404, fmt.Errorf("No agent with ID %q", info.AgentID)
	} else if agent.serverAddr != info.ServerAddr {
		// On our side, log the address we're expecting, but don't give that to the client
		logger.Warn(fmt.Sprintf(
			"Agent serverAddr is incorrect, got %q but expected %q",
			info.ServerAddr, agent.serverAddr,
		))
		return nil, 400, fmt.Errorf("Agent serverAddr is incorrect, got %q", info.ServerAddr)
	}

	wasActive := agent.EnsureUnregistered(logger)
	return &api.UnregisterAgent{WasActive: wasActive}, 200, nil
}
