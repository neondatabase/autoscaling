// Package multierrgroup provides a mix of multierr and errgroup
// See documentation for https://pkg.go.dev/go.uber.org/multierr and https://pkg.go.dev/golang.org/x/sync/errgroup
package taskgroup

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/multierr"
	"go.uber.org/zap"
)

// Group manages goroutines and collect all the errors.
// See https://pkg.go.dev/golang.org/x/sync/errgroup#group for more information
type Group interface {
	WithContext(ctx context.Context) context.Context
	Wait() error
	Go(name string, f func(logger *zap.Logger) error)
}

type group struct {
	cancel context.CancelFunc
	logger *zap.Logger

	wg sync.WaitGroup

	errMutex sync.Mutex
	err      error
}

// NewGroup returns a new Group.
func NewGroup(logger *zap.Logger) Group {
	return &group{logger: logger}
}

// WithContext returns a new Group with a associated Context.
// The context will be canceled if any goroutine returns an error.
// See https://pkg.go.dev/golang.org/x/sync/errgroup#WithContext
func (g *group) WithContext(ctx context.Context) context.Context {
	ctx, g.cancel = context.WithCancel(ctx)
	return ctx
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

// Go calls the function in a new goroutine.
// If a non-nil errors is returned, the context is canceled and
// the error is collected using multierr and will be returned by Wait.
func (g *group) Go(name string, f func(logger *zap.Logger) error) {
	g.wg.Add(1)

	go func() {
		defer g.wg.Done()
		logger := g.logger.Named(name)
		if err := f(logger); err != nil {
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
