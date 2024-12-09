package reconcile

import (
	"context"
)

// QueueOption customizes the behavior of NewQueue.
type QueueOption struct {
	apply func(*queueSettings)
}

// queueSettings is the internal, temporary structure that we use to hold the results of applying
// the various QueueOptions
type queueSettings struct {
	baseContext   context.Context
	middleware    []Middleware
	errorCallback ErrorCallback
	panicCallback PanicCallback
}

func defaultQueueSettings() *queueSettings {
	return &queueSettings{
		baseContext:   context.Background(),
		middleware:    []Middleware{},
		errorCallback: nil,
		panicCallback: nil,
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

// WithErrorCallback sets the callback to provide to the ErrorBackoffMiddleware.
//
// It will be called both on successful and failed reconcile operations, with the relevant error
// statistics.
//
// Determining whether the reconcile operation failed is possible by checking
// ErrorStats.SuccessiveFailures -- if it's zero, the operation was successful.
func WithErrorCallback(cb ErrorCallback) QueueOption {
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
