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
