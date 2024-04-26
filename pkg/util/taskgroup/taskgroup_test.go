package taskgroup_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/neondatabase/autoscaling/pkg/util/taskgroup"
	"go.uber.org/multierr"
)

func ExampleGroup() {
	g := taskgroup.Group{}
	g.Go(func() error {
		return errors.New("error 1")
	})
	g.Go(func() error {
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

	g, ctx := taskgroup.WithContext(context.Background())
	g.Go(func() error {
		return err1
	})
	g.Go(func() error {
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
