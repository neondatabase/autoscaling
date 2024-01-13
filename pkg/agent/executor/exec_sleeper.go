package executor

import (
	"context"
	"time"

	"go.uber.org/zap"
)

func (c *ExecutorCore) DoSleeper(ctx context.Context, logger *zap.Logger) {
	updates := c.updates.NewReceiver()

	// preallocate the timer. We clear it at the top of the loop; the 0 duration is just because we
	// need *some* value, so it might as well be zero.
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		// Ensure the timer is cleared at the top of the loop
		if !timer.Stop() {
			// Clear timer.C only if we haven't already read from it
			select {
			case <-timer.C:
			default:
			}
		}

		// Wait until the state's changed or we're done
		select {
		case <-ctx.Done():
			return
		case <-updates.Wait():
			updates.Awake()
		}

		last := c.getActions()
		if last.actions.Wait == nil {
			continue // nothing to do; wait until the state changes
		}

		// NB: It's possible for last.calculatedAt to be somewhat out of date. It's *probably*
		// fine, because we'll be given a notification any time the state has changed, so we
		// should wake from a select soon enough to get here
		timer.Reset(last.actions.Wait.Duration)

		select {
		case <-ctx.Done():
			return
		case <-updates.Wait():
			// Don't consume the event here. Rely on the event to remain at the top of the loop
			continue
		case <-timer.C:
			select {
			// If there's also an update, then let that take preference:
			case <-updates.Wait():
				// Same thing as above - don't consume the event here.
				continue
			// Otherwise, trigger cache invalidation because we've waited for the requested
			// amount of time:
			default:
				c.update(func(*State) {})
				updates.Awake()
				last = c.getActions()
			}
		}
	}
}
