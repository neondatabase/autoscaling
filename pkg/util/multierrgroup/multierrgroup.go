// Package multierrgroup provides a mix of multierr and errgroup
// See documentation for https://pkg.go.dev/go.uber.org/multierr and https://pkg.go.dev/golang.org/x/sync/errgroup
package multierrgroup

import (
	"context"
	"sync"

	"go.uber.org/multierr"
)

// Group manages goroutines and collect all the errors.
// See https://pkg.go.dev/golang.org/x/sync/errgroup#Group for more information
type Group struct {
	cancel context.CancelFunc

	wg sync.WaitGroup

	errMutex sync.Mutex
	err      error
}

// WithContext returns a new Group with a associated Context.
// The context will be canceled if any goroutine returns an error.
// See https://pkg.go.dev/golang.org/x/sync/errgroup#WithContext
func WithContext(ctx context.Context) (*Group, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &Group{cancel: cancel}, ctx
}

// Wait blocks until all goroutines have completed.
//
// All errors returned from the goroutines will be combined into one using multierr and returned from this method.
func (g *Group) Wait() error {
	g.wg.Wait()
	if g.cancel != nil {
		g.cancel()
	}
	return g.err
}

// Go calls the function in a new goroutine.
// If a non-nil errors is returned, the context is canceled and
// the error is collected using multierr and will be returned by Wait.
func (g *Group) Go(f func() error) {
	g.wg.Add(1)

	go func() {
		defer g.wg.Done()
		if err := f(); err != nil {
			g.errMutex.Lock()
			g.err = multierr.Append(g.err, err)
			g.errMutex.Unlock()
			if g.cancel != nil {
				g.cancel()
			}
		}
	}()
}
