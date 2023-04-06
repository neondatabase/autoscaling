package informant

// Assorted constants that aren't worth having a configuration file for

import (
	"time"
)

const (
	PrometheusPort uint16 = 9100

	CheckDeadlockDelay   time.Duration = 1 * time.Second
	CheckDeadlockTimeout time.Duration = 250 * time.Millisecond

	AgentBackgroundCheckDelay   time.Duration = 10 * time.Second
	AgentBackgroundCheckTimeout time.Duration = 250 * time.Millisecond

	AgentResumeTimeout  time.Duration = 100 * time.Millisecond
	AgentSuspendTimeout time.Duration = 5 * time.Second        // may take a while; it /suspend intentionally waits
	AgentUpscaleTimeout time.Duration = 400 * time.Millisecond // does not include waiting for /upscale response
)

var (
	// DefaultStateConfig is the default state passed to NewState
	DefaultStateConfig StateConfig = StateConfig{
		SysBufferBytes: 100 * (1 << 20), // 100 MiB
	}

	// DefaultCgroupConfig is the default CgroupConfig used for cgroup interaction logic
	DefaultCgroupConfig CgroupConfig = CgroupConfig{
		OOMBufferBytes:        200 * (1 << 20), // 200 MiB
		MemoryHighBufferBytes: 100 * (1 << 20), // 100 MiB
		MaxUpscaleWaitMillis:  20,              // 20ms
	}

	// DefaultFileCacheConfig is the default FileCacheConfig used for managing the file cache
	DefaultFileCacheConfig FileCacheConfig = FileCacheConfig{
		InMemory:               true,
		ResourceMultiplier:     0.75,            // 75%
		MinRemainingAfterCache: 640 * (1 << 20), // 640 MiB ; 640 = 512 + 128
		SpreadFactor:           0.1,             // ensure any increase in file cache size is split 90-10 with 10% to other memory
	}
)
