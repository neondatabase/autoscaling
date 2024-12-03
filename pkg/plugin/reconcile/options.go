package reconcile

import (
	"context"
	"time"
)

// QueueOption customizes the behavior of NewQueue.
type QueueOption struct {
	apply func(*queueSettings)
}

// queueSettings is the internal, temporary structure that we use to hold the results of applying
// the various QueueOptions
type queueSettings struct {
	baseContext    context.Context
	middleware     []Middleware
	waitCallback   QueueWaitDurationCallback
	resultCallback ResultCallback
	errorCallback  ErrorStatsCallback
	panicCallback  PanicCallback
}

func defaultQueueSettings() *queueSettings {
	return &queueSettings{
		baseContext:    context.Background(),
		middleware:     []Middleware{},
		waitCallback:   nil,
		resultCallback: nil,
		errorCallback:  nil,
		panicCallback:  nil,
	}
}

// WithBaseContext sets a context to use for the queue, equivalent to automatically calling
// (*Queue).Stop() when the context is canceled.
func WithBaseContext(ctx context.Context) QueueOption {
	return QueueOption{
		apply: func(s *queueSettings) {
			s.baseContext = ctx
		},
	}
}

// WithMiddleware appends the specified middleware callback for the Queue.
//
// Additional middleware is executed later -- i.e., the first middleware provided will be given a
// callback representing all remaining middleware plus the final handler.
func WithMiddleware(mw Middleware) QueueOption {
	return QueueOption{
		apply: func(s *queueSettings) {
			s.middleware = append(s.middleware, mw)
		},
	}
}

// QueueWaitDurationCallback represents the signature of the callback that may be provided to add
// observability for how long items are waiting in the queue before being reconciled.
type QueueWaitDurationCallback = func(time.Duration)

// WithQueueWaitDurationCallback sets the QueueWaitDurationCallback that will be called with the
// wait time from the desired reconcile time whenever a reconcile operation starts.
func WithQueueWaitDurationCallback(cb QueueWaitDurationCallback) QueueOption {
	return QueueOption{
		apply: func(s *queueSettings) {
			s.waitCallback = cb
		},
	}
}

// WithResultCallback sets the ResultCallback to provide to the LogMiddleware.
//
// It will be called after every reconcile operation completes with the relevant information about
// the operation and its execution.
func WithResultCallback(cb ResultCallback) QueueOption {
	return QueueOption{
		apply: func(s *queueSettings) {
			s.resultCallback = cb
		},
	}
}

// WithErrorStatsCallback sets the callback to provide to the ErrorBackoffMiddleware.
//
// It will be called whenever the error statistics change.
//
// Determining whether the reconcile operation failed is possible by checking
// ErrorStats.SuccessiveFailures -- if it's zero, the operation was successful.
func WithErrorStatsCallback(cb ErrorStatsCallback) QueueOption {
	return QueueOption{
		apply: func(s *queueSettings) {
			s.errorCallback = cb
		},
	}
}

// WithPanicCallback sets the callback to provide to the CatchPanicMiddleware.
//
// It will be called on each panic.
func WithPanicCallback(cb PanicCallback) QueueOption {
	return QueueOption{
		apply: func(s *queueSettings) {
			s.panicCallback = cb
		},
	}
}
