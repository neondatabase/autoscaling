package executor

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
)

func (c *ExecutorCore) DoSleeper(ctx context.Context, logger *zap.Logger) {
	updates := c.updates.NewReceiver()

	// preallocate the timer. We clear it at the top of the loop; the 0 duration is just because we
	// need *some* value, so it might as well be zero.
	timer := time.NewTimer(0)
	defer timer.Stop()

	last := c.getActions()
	for {
		// Ensure the timer is cleared at the top of the loop
		if !timer.Stop() {
			<-timer.C
		}

		// If NOT waiting for a particular duration:
		if last.actions.Wait == nil {
			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				updates.Awake()
				last = c.getActions()
			}
		}

		// If YES waiting for a particular duration
		if last.actions.Wait != nil {
			// NB: It's possible for last.calculatedAt to be somewhat out of date. It's *probably*
			// fine, because we'll be given a notification any time the state has changed, so we
			// should wake from a select soon enough to get here
			timer.Reset(last.actions.Wait.Duration)

			select {
			case <-ctx.Done():
				return
			case <-updates.Wait():
				updates.Awake()

				last = c.getActions()
			case <-timer.C:
				select {
				// If there's also an update, then let that take preference:
				case <-updates.Wait():
					updates.Awake()
					last = c.getActions()
				// Otherwise, trigger cache invalidation because we've waited for the requested
				// amount of time:
				default:
					c.update(func(*core.State) {})
					updates.Awake()
					last = c.getActions()
				}
			}
		}
	}
}