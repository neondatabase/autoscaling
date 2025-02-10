package reporting

// public API for event reporting

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/taskgroup"
)

type EventSink[E any] struct {
	queueWriters []*eventBatcher[E]

	runSenders func(context.Context) error
}

// NewEventSink creates a new EventSink with the given clients to dispatch events into.
//
// You MUST call (*EventSink[E]).Run() if you wish for any enqueued events to actually be sent via
// the clients.
func NewEventSink[E any](logger *zap.Logger, metrics *EventSinkMetrics, clients ...Client[E]) *EventSink[E] {
	var queueWriters []*eventBatcher[E]
	var senders []eventSender[E]

	for _, c := range clients {
		broadcaster := util.NewBroadcaster()
		sizeGauge := metrics.queueSizeCurrent.WithLabelValues(c.Name)

		batcher := newEventBatcher[E](int(c.BaseConfig.MaxBatchSize), broadcaster.Broadcast, sizeGauge)
		queueWriters = append(queueWriters, batcher)

		// Create the sender -- we'll save starting it for the call to Run()
		senders = append(senders, eventSender[E]{
			client:           c,
			metrics:          metrics,
			queue:            batcher,
			batchComplete:    broadcaster.NewReceiver(),
			lastSendDuration: 0,
		})
	}

	var runSenders func(context.Context) error
	if len(senders) > 0 {
		runSenders = func(ctx context.Context) error {
			tg := taskgroup.NewGroup(logger, taskgroup.WithParentContext(ctx))

			for _, sender := range senders {
				taskName := fmt.Sprintf("send-%s", sender.client.Name)
				tg.Go(taskName, func(logger *zap.Logger) error {
					sender.senderLoop(tg.Ctx(), logger)
					return nil
				})
			}

			return tg.Wait()
		}
	} else {
		// Special case when there's no clients -- we want our run function to just wait until the
		// context is complete, matching what the behavior *would* be if there were actually sender
		// threads we were waiting on.
		runSenders = func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		}
	}

	return &EventSink[E]{
		queueWriters: queueWriters,
		runSenders:   runSenders,
	}
}

// Run executes the client threads responsible for actually pushing enqueued events to the
// appropriate places.
//
// The clients will periodically push events until the context expires, at which point they will
// push any remaining events. Run() only completes after these final events have been pushed.
//
// Calling Run() more than once is unsound.
func (s *EventSink[E]) Run(ctx context.Context) error {
	return s.runSenders(ctx)
}

// Enqueue submits the event to the internal client sending queues, returning without blocking.
func (s *EventSink[E]) Enqueue(event E) {
	for _, q := range s.queueWriters {
		q.enqueue(event)
	}
}

type EventSinkMetrics struct {
	queueSizeCurrent *prometheus.GaugeVec
	lastSendDuration *prometheus.GaugeVec
	sendErrorsTotal  *prometheus.CounterVec
}

func NewEventSinkMetrics(prefix string, reg prometheus.Registerer) *EventSinkMetrics {
	return &EventSinkMetrics{
		queueSizeCurrent: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: fmt.Sprintf("%s_queue_size", prefix),
				Help: "Size of the billing subsystem's queue of unsent events",
			},
			[]string{"client"},
		)),
		lastSendDuration: util.RegisterMetric(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: fmt.Sprintf("%s_last_send_duration_seconds", prefix),
				Help: "Duration, in seconds, that it took to send the latest set of billing events (or current time if ongoing)",
			},
			[]string{"client"},
		)),
		sendErrorsTotal: util.RegisterMetric(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: fmt.Sprintf("%s_send_errors_total", prefix),
				Help: "Total errors from attempting to send billing events",
			},
			[]string{"client", "cause"},
		)),
	}
}
