package taskgroup_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/neondatabase/autoscaling/pkg/util/taskgroup"
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

func TestContextDoneAfterWait(t *testing.T) {
	err1 := errors.New("error 1")
	err2 := errors.New("error 2")
	log := zap.NewNop()

	g := taskgroup.NewGroup(log)
	g.Go("task1", func(_ *zap.Logger) error {
		return err1
	})
	g.Go("task2", func(_ *zap.Logger) error {
		return err2
	})
	err := g.Wait()
	assert.ErrorIs(t, err, err1)
	assert.ErrorIs(t, err, err2)

	select {
	case <-g.Ctx().Done():
		break
	default:
		t.Fatal("context should be done")
	}
}

func TestPrematureFailure(t *testing.T) {
	err1 := errors.New("error 1")
	log := zap.NewNop()

	g := taskgroup.NewGroup(log)
	g.Go("fail", func(_ *zap.Logger) error {
		return err1
	})
	g.Go("long-term", func(_ *zap.Logger) error {
		select {
		case <-g.Ctx().Done():
			break
		case <-time.After(1 * time.Second):
			t.Fatal("context should be done")
		}
		return nil
	})
	err := g.Wait()
	assert.ErrorIs(t, err, err1)
}

func TestParentContext(t *testing.T) {
	parentCtx, cancel := context.WithCancel(context.Background())
	g := taskgroup.NewGroup(zap.NewNop(), taskgroup.WithParentContext(parentCtx))
	cancel()

	select {
	case <-g.Ctx().Done():
		break
	default:
		t.Fatal("context should be done")
	}
}

func setupLogsCapture() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.InfoLevel)
	return zap.New(core), logs
}

func TestPanic(t *testing.T) {
	var panicCnt int
	handler := func(_ any) {
		panicCnt++
	}

	logger, logs := setupLogsCapture()
	g := taskgroup.NewGroup(logger, taskgroup.WithPanicHandler(handler))
	g.Go("task1", func(_ *zap.Logger) error {
		panic("panic message")
	})
	err := g.Wait()
	assert.NotNil(t, err)
	assert.Equal(t, "task task1 failed: panic: panic message", err.Error())
	assert.Equal(t, 1, panicCnt)

	// We have two log lines: one specific for the panic, with additional
	// context, and one for any task failure.
	assert.Equal(t, 2, logs.Len())

	msg0 := logs.All()[0]
	assert.Equal(t, "Task panicked", msg0.Message)
	assert.Len(t, msg0.Context, 2)
	assert.Equal(t, "payload", msg0.Context[0].Key)
	assert.Equal(t, "panic message", msg0.Context[0].String)
	assert.Equal(t, "stack", msg0.Context[1].Key)
	stackTrace := msg0.Context[1].String
	// test that the stack trace begins with gopanic(...) so we always start
	// the backtrace at the same place
	assert.True(t, strings.HasPrefix(stackTrace, "runtime.gopanic(...)\n"))

	msg1 := logs.All()[1]
	// msg := task {name} failed: {error}; error := panic: {panicMessage}
	assert.Equal(t, "task task1 failed: panic: panic message", msg1.Message)
	assert.Len(t, msg1.Context, 0)
}
