package billing

// Logic responsible for sending billing events by repeatedly pulling from the eventQueue

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/billing"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type clientInfo struct {
	client billing.Client
	name   string
	config BaseClientConfig
}

type eventSender struct {
	clientInfo

	metrics           PromMetrics
	queue             eventQueuePuller[*billing.IncrementalEvent]
	collectorFinished util.CondChannelReceiver

	// lastSendDuration tracks the "real" last full duration of (eventSender).sendAllCurrentEvents().
	//
	// It's separate from metrics.lastSendDuration because (a) we'd like to include the duration of
	// ongoing calls to sendAllCurrentEvents, but (b) we don't want the bias towards lower durations
	// that comes with that.
	//
	// Here's some more detail:
	//
	// To make sure that long-running sendAllCurrentEvents() loops show up in the metrics while
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

func (s eventSender) senderLoop(logger *zap.Logger) {
	ticker := time.NewTicker(time.Second * time.Duration(s.config.PushEverySeconds))
	defer ticker.Stop()

	for {
		final := false

		select {
		case <-s.collectorFinished.Recv():
			logger.Info("Received notification that collector finished")
			final = true
		case <-ticker.C:
		}

		s.sendAllCurrentEvents(logger)

		if final {
			logger.Info("Ending events sender loop")
			return
		}
	}
}

func (s eventSender) sendAllCurrentEvents(logger *zap.Logger) {
	logger.Info("Pushing all available events")

	if s.queue.size() == 0 {
		logger.Info("No billing events to push")
		s.lastSendDuration = 0
		s.metrics.lastSendDuration.WithLabelValues(s.clientInfo.name).Set(1e-6) // small value, to indicate that nothing happened
		return
	}

	total := 0
	startTime := time.Now()

	// while there's still events in the queue, send them
	//
	// If events are being added to the queue faster than we can send them, this loop will not
	// terminate. For the most part, that's ok: worst-case, we miss the collectorFinished
	// notification, which isn't the end of the world. Any long-running call to this function will
	// be reported by s.metrics.lastSendDuration as we go (provided the request timeout isn't too
	// long).
	for {
		if size := s.queue.size(); size != 0 {
			logger.Info("Current queue size is non-zero", zap.Int("queueSize", size))
		}

		chunk := s.queue.get(int(s.config.MaxBatchSize))
		count := len(chunk)
		if count == 0 {
			totalTime := time.Since(startTime)
			s.lastSendDuration = totalTime
			s.metrics.lastSendDuration.WithLabelValues(s.clientInfo.name).Set(totalTime.Seconds())

			logger.Info(
				"All available events have been sent",
				zap.Int("total", total),
				zap.Duration("totalTime", totalTime),
			)
			return
		}

		traceID := billing.GenerateTraceID()

		logger.Info(
			"Pushing billing events",
			zap.Int("count", count),
			zap.String("traceID", string(traceID)),
			s.client.LogFields(),
		)

		reqStart := time.Now()
		err := func() error {
			reqCtx, cancel := context.WithTimeout(context.TODO(), time.Second*time.Duration(s.config.PushRequestTimeoutSeconds))
			defer cancel()

			return billing.Send(reqCtx, s.client, traceID, chunk)
		}()
		reqDuration := time.Since(reqStart)

		if err != nil {
			// Something went wrong and we're going to abandon attempting to push any further
			// events.
			logger.Error(
				"Failed to push billing events",
				zap.Int("count", count),
				zap.Duration("after", reqDuration),
				zap.String("traceID", string(traceID)),
				s.client.LogFields(),
				zap.Int("total", total),
				zap.Duration("totalTime", time.Since(startTime)),
				zap.Error(err),
			)

			var rootErr string
			//nolint:errorlint // The type switch (instead of errors.As) is ok; billing.Send() guarantees the error types.
			switch e := err.(type) {
			case billing.JSONError:
				rootErr = "JSON marshaling"
			case billing.UnexpectedStatusCodeError:
				rootErr = fmt.Sprintf("HTTP code %d", e.StatusCode)
			default:
				rootErr = util.RootError(err).Error()
			}
			s.metrics.sendErrorsTotal.WithLabelValues(s.clientInfo.name, rootErr).Inc()

			s.lastSendDuration = 0
			s.metrics.lastSendDuration.WithLabelValues(s.clientInfo.name).Set(0.0) // use 0 as a flag that something went wrong; there's no valid time here.
			return
		}

		s.queue.drop(count) // mark len(chunk) as successfully processed
		total += len(chunk)
		currentTotalTime := time.Since(startTime)

		logger.Info(
			"Successfully pushed some billing events",
			zap.Int("count", count),
			zap.Duration("after", reqDuration),
			zap.String("traceID", string(traceID)),
			s.client.LogFields(),
			zap.Int("total", total),
			zap.Duration("totalTime", currentTotalTime),
		)

		if currentTotalTime > s.lastSendDuration {
			s.lastSendDuration = currentTotalTime
			s.metrics.lastSendDuration.WithLabelValues(s.clientInfo.name).Set(currentTotalTime.Seconds())
		}
	}
}
