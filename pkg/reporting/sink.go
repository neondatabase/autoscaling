package reporting

// public API for event reporting

import (
	"context"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/util/taskgroup"
)

type EventSink[E any] struct {
	queueWriters []eventQueuePusher[E]
	done         func()
}

func NewEventSink[E any](
	ctx context.Context,
	logger *zap.Logger,
	tg taskgroup.Group,
	metrics *EventSinkMetrics,
	clients ...Client[E],
) *EventSink[E] {
	var queueWriters []eventQueuePusher[E]

	senderCtx, cancelSenders := context.WithCancel(ctx)

	for _, c := range clients {
		qw, qr := newEventQueue[E](metrics.queueSizeCurrent.WithLabelValues(c.Name))
		queueWriters = append(queueWriters, qw)

		// Start the sender
		sender := eventSender[E]{
			client:           c,
			metrics:          metrics,
			queue:            qr,
			lastSendDuration: 0,
		}
		taskName := fmt.Sprintf("send-%s", c.Name)
		tg.Go(taskName, func(_ *zap.Logger) error {
			sender.senderLoop(senderCtx, logger.Named(taskName))
			return nil
		})
	}

	return &EventSink[E]{
		queueWriters: queueWriters,
		done:         sync.OnceFunc(cancelSenders),
	}
}

// Enqueue submits the event to the internal client sending queues, returning without blocking.
func (s *EventSink[E]) Enqueue(event E) {
	for _, q := range s.queueWriters {
		q.enqueue(event)
	}
}

// Finish signals that the last events have been Enqueue'd, and so they should be sent off before
// shutting down.
func (s *EventSink[E]) Finish() {
	s.done()
}

type EventSinkMetrics struct {
	queueSizeCurrent *prometheus.GaugeVec
	lastSendDuration *prometheus.GaugeVec
	sendErrorsTotal  *prometheus.CounterVec
}

func NewEventSinkMetrics(prefix string) *EventSinkMetrics {
	return &EventSinkMetrics{
		queueSizeCurrent: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: fmt.Sprintf("%s_queue_size", prefix),
				Help: "Size of the billing subsystem's queue of unsent events",
			},
			[]string{"client"},
		),
		lastSendDuration: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: fmt.Sprintf("%s_last_send_duration_seconds", prefix),
				Help: "Duration, in seconds, that it took to send the latest set of billing events (or current time if ongoing)",
			},
			[]string{"client"},
		),
		sendErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: fmt.Sprintf("%s_send_errors_total", prefix),
				Help: "Total errors from attempting to send billing events",
			},
			[]string{"client", "cause"},
		),
	}
}

func (m *EventSinkMetrics) MustRegister(reg *prometheus.Registry) {
	reg.MustRegister(m.queueSizeCurrent)
	reg.MustRegister(m.lastSendDuration)
	reg.MustRegister(m.sendErrorsTotal)
}
