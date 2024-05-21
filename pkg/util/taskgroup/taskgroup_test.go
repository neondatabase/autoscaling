package taskgroup_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.uber.org/multierr"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/util/taskgroup"
	"github.com/stretchr/testify/assert"
)

func ExampleGroup() {
	g := taskgroup.NewGroup(zap.NewNop())
	g.Go("task1", func(_ *zap.Logger) error {
		return errors.New("error 1")
	})
	g.Go("task2", func(_ *zap.Logger) error {
		return errors.New("error 2")
	})
	err := g.Wait()
	// Using golang.org/x/sync/errgroup would return a return error depending on which goroutine was scheduled first

	errs := multierr.Errors(err)
	fmt.Println("Got", len(errs), "errors")
	// Output: Got 2 errors
}

func TestWithContext(t *testing.T) {
	err1 := errors.New("error 1")
	err2 := errors.New("error 2")
	log := zap.NewNop()

	g := taskgroup.NewGroup(log)
	ctx := g.WithContext(context.Background())
	g.Go("task1", func(_ *zap.Logger) error {
		return err1
	})
	g.Go("task2", func(_ *zap.Logger) error {
		return err2
	})
	err := g.Wait()
	if !errors.Is(err, err1) {
		t.Errorf("error: %s should be: %s", err, err1)
	}
	if !errors.Is(err, err2) {
		t.Errorf("error: %s should be: %s", err, err2)
	}
	canceled := false
	select {
	case <-ctx.Done():
		canceled = true
	default:

	}
	if !canceled {
		t.Errorf("context should have been canceled!")
	}
}

func TestPanic(t *testing.T) {
	log := zap.NewNop()
	g := taskgroup.NewGroup(log)
	g.Go("task1", func(_ *zap.Logger) error {
		panic("panic message")
	})
	err := g.Wait()
	assert.NotNil(t, err)
	assert.Equal(t, err.Error(), "task task1 failed: panic: panic message")
}
