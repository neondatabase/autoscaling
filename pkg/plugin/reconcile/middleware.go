package reconcile

import (
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/neondatabase/autoscaling/pkg/util/stack"
)

// Middleware wraps a reconcile operation to insert its own logic
type Middleware interface {
	Call(*zap.Logger, MiddlewareParams, MiddlewareHandlerFunc) (Result, error)
}

// MiddlewareHandlerFunc is an enriched version of HandlerFunc that accepts more parameters, so that
// middleware functions don't need to know whether they're calling the base HandlerFunc or another
// piece of middleware.
type MiddlewareHandlerFunc = func(*zap.Logger, MiddlewareParams) (Result, error)

// MiddlewareParams stores the miscellaneous parameters that are made available to all middleware
type MiddlewareParams struct {
	GVK       schema.GroupVersionKind
	UID       types.UID
	Name      string
	Namespace string
	EventKind EventKind
	Obj       Object
}

// Key returns the fields uniquely identifying the associated object
func (p MiddlewareParams) Key() Key {
	return Key{GVK: p.GVK, UID: p.UID}
}

func applyMiddleware(middleware []Middleware, handler HandlerFunc) HandlerFunc {
	f := func(l *zap.Logger, p MiddlewareParams) (Result, error) {
		return handler(l, p.EventKind, p.Obj)
	}
	// Iterate backwards, so that the
	for i := len(middleware) - 1; i >= 0; i-- {
		// copy to avoid loop var escaping (maybe not needed after Go 1.22? it's hard to be certain)
		m := middleware[i]
		// copy 'f', so that we don't recurse -- otherwise, the function will be referenced by name.
		// See https://go.dev/play/p/8f4EgbL4Rm2 for an example of the difference.
		oldF := f
		f = func(l *zap.Logger, p MiddlewareParams) (Result, error) {
			return m.Call(l, p, oldF)
		}
	}

	return func(logger *zap.Logger, k EventKind, obj Object) (Result, error) {
		// Extract the common parameters:
		params := MiddlewareParams{
			GVK:       obj.GetObjectKind().GroupVersionKind(),
			UID:       obj.GetUID(),
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
			EventKind: k,
			Obj:       obj,
		}
		return f(logger, params)
	}
}

func defaultMiddleware(
	types []schema.GroupVersionKind,
	resultCallback ResultCallback,
	errorCallback ErrorStatsCallback,
	panicCallback PanicCallback,
) []Middleware {
	return []Middleware{
		NewLogMiddleware(resultCallback),
		NewErrorBackoffMiddleware(types, errorCallback),
		NewCatchPanicMiddleware(panicCallback),
	}
}

// ResultCallback represents the signature of the optional callback that may be registered with the
// LogMiddleware to update metrics or similar based on the result of each reconcile operation.
type ResultCallback = func(params MiddlewareParams, duration time.Duration, err error)

// LogMiddleware is middleware for the reconcile queue that augments the logger with fields
// describing the object being reconciled, as well as logging the results of each reconcile
// operation.
//
// This middleware is always included. It's public to provide additional documentation.
type LogMiddleware struct {
	resultCallback ResultCallback
}

func NewLogMiddleware(callback ResultCallback) *LogMiddleware {
	return &LogMiddleware{
		resultCallback: callback,
	}
}

// ObjectMetaLogField returns a zap.Field for an object, in the same format that the default logging
// middleware uses.
//
// The returned zap.Field has the given key, and is a zap.Object with the namespace, name, and UID
// of the kubernetes object.
func ObjectMetaLogField(key string, obj Object) zap.Field {
	return objLogFieldFromParams(key, obj.GetNamespace(), obj.GetName(), obj.GetUID())
}

func objLogFieldFromParams(key string, namespace string, name string, uid types.UID) zap.Field {
	return zap.Object(key, zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		if namespace != "" {
			enc.AddString("Namespace", namespace)
		}
		enc.AddString("Name", name)
		enc.AddString("UID", string(uid))
		return nil
	}))
}

// Call implements Middleware
func (m *LogMiddleware) Call(
	logger *zap.Logger,
	params MiddlewareParams,
	handler MiddlewareHandlerFunc,
) (Result, error) {
	logger = logger.With(objLogFieldFromParams(params.GVK.Kind, params.Namespace, params.Name, params.UID))

	started := time.Now()
	result, err := handler(logger, params)
	duration := time.Since(started)
	if m.resultCallback != nil {
		m.resultCallback(params, duration, err)
	}
	if err != nil {
		logger.Error(
			fmt.Sprintf("Failed to reconcile %s %s", params.EventKind, params.GVK.Kind),
			zap.Duration("duration", duration),
			zap.Error(err),
		)
	} else {
		logger.Info(
			fmt.Sprintf("Reconciled %s %s", params.EventKind, params.GVK.Kind),
			zap.Duration("duration", duration),
		)
	}
	return result, err
}

// PanicCallback represents the signature of the optional callback that may be registered with the
// CatchPanicMiddleware.
type PanicCallback = func(MiddlewareParams)

// CatchPanicMiddleware is middleware for the reconcile queue that turns panics into errors.
//
// It can optionally be provided a PanicCallback to exfiltrate information about the panics that
// occur.
//
// This middleware is always included. It's public to provide additional documentation.
type CatchPanicMiddleware struct {
	callback PanicCallback
}

// NewCatchPanicMiddleware returns middleware to turn panics into errors.
//
// If not nil, the callback will be called whenever there is a panic.
//
// Because CatchPanicMiddleware is automatically included in calls to NewQueue, providing the
// callback is best done with the WithPanicCallback QueueOption.
func NewCatchPanicMiddleware(callback PanicCallback) *CatchPanicMiddleware {
	return &CatchPanicMiddleware{
		callback: callback,
	}
}

// Call implements Middleware
func (m *CatchPanicMiddleware) Call(
	logger *zap.Logger,
	params MiddlewareParams,
	handler MiddlewareHandlerFunc,
) (_ Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			st := stack.GetStackTrace(nil, 0).String()
			logger.Error("Reconcile panicked", zap.Any("payload", r), zap.String("stack", st))
			if m.callback != nil {
				m.callback(params)
			}
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	return handler(logger, params)
}

// ErrorStatsCallback represents the signature of the optional callback that may be registered with
// the ErrorBackoffMiddleware.
type ErrorStatsCallback = func(MiddlewareParams, ErrorStats)

// ErrorStats are the values provided to the callback for ErrorBackoffMiddleware.
type ErrorStats struct {
	// GlobalCount is the total number of objects currently failing to be reconciled.
	GlobalCount int
	// TypedCount is the number of objects of this type that are failing to be reconciled.
	TypedCount int
	// SuccessiveFailures gives the number of times in a row that this object has failed to be
	// reconciled.
	// On success, this value is equal to zero.
	SuccessiveFailures int
}

type ErrorBackoffMiddleware struct {
	globalCounterMu sync.Mutex
	globalFailing   int

	byType map[schema.GroupVersionKind]*typedTimingSet

	callback ErrorStatsCallback
}

type typedTimingSet struct {
	mu    sync.Mutex
	byUID map[types.UID]backoff
}

type backoff struct {
	successiveFailures int
	waitDuration       time.Duration
}

const (
	initialErrorWait = 100 * time.Millisecond
	backoffFactor    = 2.03 // factor of 2.03 results in 0.1s -> 60s after 10 failures.
	maxErrorWait     = time.Minute
)

func NewErrorBackoffMiddleware(typs []schema.GroupVersionKind, callback ErrorStatsCallback) *ErrorBackoffMiddleware {
	byType := make(map[schema.GroupVersionKind]*typedTimingSet)

	for _, gvk := range typs {
		byType[gvk] = &typedTimingSet{
			mu:    sync.Mutex{},
			byUID: make(map[types.UID]backoff),
		}
	}

	return &ErrorBackoffMiddleware{
		globalCounterMu: sync.Mutex{},
		globalFailing:   0,
		byType:          byType,
		callback:        callback,
	}
}

func (m *ErrorBackoffMiddleware) Call(
	logger *zap.Logger,
	params MiddlewareParams,
	handler MiddlewareHandlerFunc,
) (Result, error) {
	typed, ok := m.byType[params.GVK]
	if !ok {
		panic(fmt.Sprintf("received reconcile for unknown type %s", fmtGVK(params.GVK)))
	}

	result, err := handler(logger, params)

	typed.mu.Lock()
	defer typed.mu.Unlock()

	failed := err != nil
	b, wasFailing := typed.byUID[params.UID]

	var change int

	if failed {
		b.successiveFailures += 1
		if wasFailing {
			b.waitDuration = min(maxErrorWait, time.Duration(float64(b.waitDuration)*backoffFactor))
		} else {
			b.waitDuration = initialErrorWait
		}

		if result.RetryAfter != 0 {
			// Cap the current wait duration with the requested retry, IF the handler left an
			// explicit amount it wanted to wait.
			// This is to avoid long retries on a spurious failure after the situation has resolved.
			b.waitDuration = min(result.RetryAfter, b.waitDuration)
		}
		// use max(..) so that the backoff MUST be respected, but waits longer than it are allowed.
		result.RetryAfter = max(result.RetryAfter, b.waitDuration)

		typed.byUID[params.UID] = b
		if !wasFailing {
			change = 1 // +1 item failing
		}
	} else /* !failed */ {
		// reset the counters, for below
		b = backoff{waitDuration: 0, successiveFailures: 0}

		// remove the tracking for this value, if it was present - it's not failing
		delete(typed.byUID, params.UID)
		if wasFailing {
			change = -1 // -1 item failing
		}
	}

	if change != 0 {
		m.globalCounterMu.Lock()
		defer m.globalCounterMu.Unlock()

		m.globalFailing += change

		if m.callback != nil {
			m.callback(params, ErrorStats{
				GlobalCount:        m.globalFailing,
				TypedCount:         len(typed.byUID),
				SuccessiveFailures: b.successiveFailures,
			})
		}
	}

	return result, err
}
