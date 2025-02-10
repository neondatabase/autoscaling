package reporting

// public API for event reporting

import (
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type EventSink[E any] struct {
	queueWriters []eventQueuePusher[E]
	done         func()
}

func NewEventSink[E any](logger *zap.Logger, metrics *EventSinkMetrics, clients ...Client[E]) *EventSink[E] {
	var queueWriters []eventQueuePusher[E]
	signalDone := make(chan struct{})

	for _, c := range clients {
		qw, qr := newEventQueue[E](metrics.queueSizeCurrent.WithLabelValues(c.Name))
		queueWriters = append(queueWriters, qw)

		// Start the sender
		sender := eventSender[E]{
			client:           c,
			metrics:          metrics,
			queue:            qr,
			done:             signalDone,
			lastSendDuration: 0,
		}
		go sender.senderLoop(logger.Named(fmt.Sprintf("send-%s", c.Name)))
	}

	return &EventSink[E]{
		queueWriters: queueWriters,
		done:         sync.OnceFunc(func() { close(signalDone) }),
	}
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
