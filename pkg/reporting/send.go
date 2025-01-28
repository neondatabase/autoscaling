package reporting

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type eventSender[E any] struct {
	client Client[E]

	metrics *EventSinkMetrics

	queue         *eventBatcher[E]
	batchComplete util.BroadcastReceiver

	// lastSendDuration tracks the "real" last full duration of (eventSender).sendAllCompletedBatches().
	//
	// It's separate from metrics.lastSendDuration because (a) we'd like to include the duration of
	// ongoing calls to sendAllCompletedBatches, but (b) we don't want the bias towards lower
	// durations that comes with that.
	//
	// Here's some more detail:
	//
	// To make sure that long-running sendAllCompletedBatches() loops show up in the metrics while
	// they're still running, we want to periodically update metrics.lastSendDuration before the
	// loop has finished. A side-effect of doing this naively is that the gauge will sometimes
	// return durations that are much shorter than the *actual* previous send loop duration.
	//
	// In order to fix this, we store that *actual* previous duration in this field, but only
	// update the metric when either (a) the loop is done, or (b) the duration so far is already
	// longer than the previous one.
	//
	// This means that we remove the bias towards shorter durations, at the expense of sometimes
	// returning higher durations for too long. IMO that's ok, and we'd rather have our metrics give
	// a pessimistic but more accurate view.
	lastSendDuration time.Duration
}

func (s eventSender[E]) senderLoop(ctx context.Context, logger *zap.Logger) {
	heartbeat := time.Second * time.Duration(s.client.BaseConfig.PushEverySeconds)

	timer := time.NewTimer(heartbeat)
	defer timer.Stop()

	for {
		final := false

		select {
		case <-ctx.Done():
			logger.Info("Received notification that events submission is done")
			final = true
			// finish up any in-progress batch, so that we can send it before we exit.
			s.queue.finishOngoing()
		case <-s.batchComplete.Wait():
			s.batchComplete.Awake() // consume this notification
		case <-timer.C:
			// Timer has expired without any notification of a completed batch. Let's explicitly ask
			// for in-progress events to be wrapped up into a batch so we ship them fast enough and
			// reset the timer.
			//
			// consume s.batchComplete on repeat if there were events; otherwise wait until the
			// timer expires again.
			s.queue.finishOngoing()
			timer.Reset(heartbeat)
			continue
		}

		// Make sure that if there are no more events within the next heartbeat duration, that we'll
		// push the events that have been accumulated so far.
		timer.Reset(heartbeat)

		s.sendAllCompletedBatches(logger)

		if final {
			logger.Info("Ending events sender loop")
			return
		}
	}
}

func (s eventSender[E]) sendAllCompletedBatches(logger *zap.Logger) {
	logger.Info("Pushing all available event batches")

	if len(s.queue.peekCompleted()) == 0 {
		logger.Info("No event batches to push")
		s.lastSendDuration = 0
		s.metrics.lastSendDuration.WithLabelValues(s.client.Name).Set(1e-6) // small value, to indicate that nothing happened
		return
	}

	totalEvents := 0
	totalBatches := 0
	startTime := time.Now()

	// while there's still batches of events in the queue, send them
	//
	// If batches are being added to the queue faster than we can send them, this loop will not
	// terminate. For the most part, that's ok: worst-case, we miss that the parent context has
	// expired, which isn't the end of the world (eventually the autoscaler-agent will just be
	// force-killed). Any long-running call to this function will be reported by
	// s.metrics.lastSendDuration as we go (provided the request timeout isn't too long), so we
	// should get observability for it either way.
	for {
		batches := s.queue.peekCompleted()

		if len(batches) != 0 {
			logger.Info("Current queue size is non-zero", zap.Int("batchCount", len(batches)))
		} else {
			totalTime := time.Since(startTime)
			s.lastSendDuration = totalTime
			s.metrics.lastSendDuration.WithLabelValues(s.client.Name).Set(totalTime.Seconds())

			logger.Info(
				"All available event batches have been sent",
				zap.Int("totalEvents", totalEvents),
				zap.Int("totalBatches", totalBatches),
				zap.Duration("totalTime", totalTime),
			)
			return
		}

		batch := batches[0]

		req := s.client.Base.NewRequest()

		logger.Info(
			"Pushing events batch",
			zap.Int("count", len(batch.events)),
			req.LogFields(),
		)

		reqStart := time.Now()
		err := func() SimplifiableError {
			reqCtx, cancel := context.WithTimeout(
				context.TODO(),
				time.Second*time.Duration(s.client.BaseConfig.PushRequestTimeoutSeconds),
			)
			defer cancel()

			payload, err := s.client.SerializeBatch(batch.events)
			if err != nil {
				return err
			}

			return req.Send(reqCtx, payload)
		}()
		reqDuration := time.Since(reqStart)

		if err != nil {
			// Something went wrong and we're going to abandon attempting to push any further
			// events.
			logger.Error(
				"Failed to push billing events",
				zap.Int("count", len(batch.events)),
				zap.Duration("after", reqDuration),
				req.LogFields(),
				zap.Int("totalEvents", totalEvents),
				zap.Int("totalBatches", totalBatches),
				zap.Duration("totalTime", time.Since(startTime)),
				zap.Error(err),
			)

			rootErr := err.Simplified()
			s.metrics.sendErrorsTotal.WithLabelValues(s.client.Name, rootErr).Inc()

			s.lastSendDuration = 0
			s.metrics.lastSendDuration.WithLabelValues(s.client.Name).Set(0.0) // use 0 as a flag that something went wrong; there's no valid time here.
			return
		}

		s.queue.dropCompleted(1) // mark this batch as complete
		totalEvents += len(batch.events)
		totalBatches += 1
		currentTotalTime := time.Since(startTime)

		logger.Info(
			"Successfully pushed some events",
			zap.Int("count", len(batch.events)),
			zap.Duration("after", reqDuration),
			req.LogFields(),
			zap.Int("totalEvents", totalEvents),
			zap.Int("totalBatches", totalBatches),
			zap.Duration("totalTime", currentTotalTime),
		)

		if currentTotalTime > s.lastSendDuration {
			s.lastSendDuration = currentTotalTime
			s.metrics.lastSendDuration.WithLabelValues(s.client.Name).Set(currentTotalTime.Seconds())
		}
	}
}
