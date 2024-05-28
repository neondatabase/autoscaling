// Originally taken from https://github.com/ptxmac/multierrgroup

// Package taskgroup provides a mix of multierr and errgroup
// See documentation for https://pkg.go.dev/go.uber.org/multierr and https://pkg.go.dev/golang.org/x/sync/errgroup
package taskgroup

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/multierr"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/util/stack"
)

// Group manages goroutines and collect all the errors.
// See https://pkg.go.dev/golang.org/x/sync/errgroup#group for more information
type Group interface {
	Ctx() context.Context
	WithPanicHandler(f func(any))
	Wait() error
	Go(name string, f func(logger *zap.Logger) error)
}

type group struct {
	cancel       context.CancelFunc
	ctx          context.Context
	logger       *zap.Logger
	panicHandler func(any)

	wg sync.WaitGroup

	errMutex sync.Mutex
	err      error
}

type GroupOption func(*group)

// WithParentContext sets the parent context for the group.
func WithParentContext(ctx context.Context) GroupOption {
	return func(g *group) {
		g.ctx, g.cancel = context.WithCancel(ctx)
	}
}

// NewGroup returns a new Group.
func NewGroup(logger *zap.Logger, opts ...GroupOption) Group {
	g := &group{
		cancel:       nil, // Set separately by Ctx
		ctx:          nil, // Set separately by Ctx
		panicHandler: nil, // Set separately by WithPanicHandler
		logger:       logger,
		wg:           sync.WaitGroup{},

		errMutex: sync.Mutex{},
		err:      nil,
	}

	for _, opt := range opts {
		opt(g)
	}
	if g.ctx == nil {
		// If parent context is not set, use background context
		WithParentContext(context.Background())(g)
	}

	return g
}

// Ctx returns a context that will be canceled when the group is Waited.
func (g *group) Ctx() context.Context {
	return g.ctx
}

// WithPanicHandler sets a panic handler for the group.
func (g *group) WithPanicHandler(f func(any)) {
	g.panicHandler = f
}

// Wait blocks until all goroutines have completed.
//
// All errors returned from the goroutines will be combined into one using multierr and returned from this method.
func (g *group) Wait() error {
	g.wg.Wait()
	if g.cancel != nil {
		g.cancel()
	}
	return g.err
}

func (g *group) call(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if g.panicHandler != nil {
				g.panicHandler(r)
			}
			st := stack.GetStackTrace(nil, 1).String()
			g.logger.Error("panic", zap.Any("panic", r), zap.String("stack", st))
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	err = f()
	return err
}

// Go calls the function in a new goroutine.
// If a non-nil errors is returned, the context is canceled and
// the error is collected using multierr and will be returned by Wait.
func (g *group) Go(name string, f func(logger *zap.Logger) error) {
	g.wg.Add(1)

	go func() {
		defer g.wg.Done()
		logger := g.logger.Named(name)
		cb := func() error {
			return f(logger)
		}
		if err := g.call(cb); err != nil {
			err = fmt.Errorf("task %s failed: %w", name, err)
			g.errMutex.Lock()
			g.err = multierr.Append(g.err, err)
			g.errMutex.Unlock()
			logger.Error(err.Error())
			if g.cancel != nil {
				g.cancel()
			}
		}
	}()
}
