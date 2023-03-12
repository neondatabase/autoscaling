package task

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"

	"github.com/sharnoff/chord"

	"k8s.io/klog/v2"
)

var ManagerKey managerKey

type managerKey struct{}

// Manager provides a per-goroutine task management interface
//
// It's expected that each goroutine will have its own Manager.
type Manager struct {
	ctx     context.Context
	signals *signalManager
	group   *chord.TaskGroup
	caller  *chord.StackTrace

	onError ErrorHandler
	onPanic PanicHandler

	pathInGroup string
	groupPath   string

	cleanupToken *struct{} // cleanup magic :P
}

// signalManager wraps a chord.SignalManager with a refcount, so we can call SignalManager.Stop()
// only when all goroutines spawned by the task are done
type signalManager struct {
	sm       *chord.SignalManager
	parent   *signalManager
	refcount atomic.Int64
}

func (s *signalManager) incr() {
	s.refcount.Add(1)
}

func (s *signalManager) decr() {
	if s.refcount.Add(-1) == 0 {
		s.sm.Stop()
		if s.parent != nil {
			s.parent.decr()
		}
	}
}

type TaskTree = chord.TaskTree

type sigShutdown struct{}

type ErrorHandler func(error) error
type PanicHandler func(fullTaskName string, err any, stackTrace chord.StackTrace)

func LogFatalError(format string) func(error) error {
	return func(err error) error {
		klog.Fatalf(format, err)
		return nil
	}
}

func LogPanic(taskName string, err any, stackTrace chord.StackTrace) {
	klog.Errorf("task %s panicked with %v:\n%s", taskName, err, stackTrace.String())
}

func LogPanicAndExit(taskName string, err any, stackTrace chord.StackTrace) {
	LogPanic(taskName, err, stackTrace)
	os.Exit(2)
}

func LogPanicAndShutdown(m Manager, makeCtx func() (context.Context, context.CancelFunc)) PanicHandler {
	return func(taskName string, err any, stackTrace chord.StackTrace) {
		LogPanic(taskName, err, stackTrace)
		klog.Warningf("Shutting down %v due to previous panic", m.FullName())
		ctx, cancel := makeCtx()
		defer cancel()
		if err := m.Shutdown(ctx); err != nil {
			klog.Errorf("Shutdown returned error: %w", err)
		}
	}
}

func WrapOnError(format string, f func(context.Context) error) func(context.Context) error {
	return func(c context.Context) error {
		if err := f(c); err != nil {
			return fmt.Errorf(format, err)
		}
		return nil
	}
}

func Infallible(f func()) func(context.Context) error {
	return func(context.Context) error {
		f()
		return nil
	}
}

// NewRootTaskManager creates a new base TaskManager, typically done at program startup or
func NewRootTaskManager(name string) Manager {
	signals := &signalManager{
		sm:       chord.NewSignalManager(),
		parent:   nil,
		refcount: atomic.Int64{},
	}
	signals.refcount.Add(1)

	cleanupToken := &struct{}{}
	runtime.SetFinalizer(cleanupToken, func(obj any) {
		signals.decr()
	})

	return Manager{
		ctx:          nil,
		signals:      signals,
		group:        chord.NewTaskGroup(name),
		caller:       nil,
		onPanic:      nil,
		pathInGroup:  "main",
		groupPath:    name,
		cleanupToken: cleanupToken,
	}
}

func (m Manager) FullName() string {
	return fmt.Sprintf("group{%s}-task{%s}", m.groupPath, m.pathInGroup)
}

func (m Manager) ShutdownOnSigterm(makeContext func() (context.Context, context.CancelFunc)) {
	onErr := m.onError
	handler := func(_ context.Context, err error) error {
		return onErr(err)
	}
	err := m.signals.sm.WithErrorHandler(handler).On(syscall.SIGTERM, context.Background(), func(context.Context) error {
		ctx, cancel := makeContext()
		defer cancel()
		return m.Shutdown(ctx)
	})
	if err != nil {
		panic(fmt.Errorf("unexpected error while setting SIGTERM hook: %w", err))
	}
}

func (m Manager) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	} else {
		return m.signals.sm.Context(sigShutdown{})
	}
}

func (m Manager) Caller() *chord.StackTrace {
	return m.caller
}

func (m Manager) WithCaller(trace *chord.StackTrace) Manager {
	m.caller = trace
	return m
}

func (m Manager) WithContext(ctx context.Context) Manager {
	m.ctx = ctx
	return m
}

func (m Manager) WithShutdownErrorHandler(onError ErrorHandler) Manager {
	if m.onError != nil && onError != nil {
		parent := m.onError
		m.onError = func(err error) error {
			err = onError(err)
			if err != nil {
				err = parent(err)
			}
			return err
		}
	} else {
		m.onError = onError
	}

	return m
}

func (m Manager) WithPanicHandler(onPanic PanicHandler) Manager {
	m.onPanic = onPanic
	return m
}

func (m Manager) MaybeRecover() {
	if err := recover(); err != nil {
		if m.onPanic != nil {
			// panicking should skip 2 -- one for this function, and one for the call to
			// runtime.panic itself
			trace := chord.GetStackTrace(m.caller, 2)
			m.onPanic(m.FullName(), err, trace)
		} else {
			// If there's no panic handler, propagate the error
			panic(err)
		}
	}
}

func (m Manager) NewForTask(name string) (_ Manager, done func()) {
	m.group.Add(name)
	m.signals.incr()

	return Manager{
			ctx:          m.ctx,
			signals:      m.signals,
			group:        m.group,
			caller:       m.caller,
			onPanic:      m.onPanic,
			pathInGroup:  fmt.Sprintf("%s/%s", m.pathInGroup, name),
			groupPath:    m.groupPath,
			cleanupToken: m.cleanupToken,
		}, func() {
			m.group.Done(name)
			m.signals.decr()
		}
}

func (m Manager) Spawn(name string, f func(Manager)) {
	caller := chord.GetStackTrace(m.caller, 1) // ignore this function in stack trace

	child, done := m.WithCaller(&caller).NewForTask(name)
	go func() {
		defer done()
		defer child.MaybeRecover()
		f(child)
	}()
}

func (m Manager) SpawnAsSubgroup(name string, f func(Manager)) SubgroupHandle {
	caller := chord.GetStackTrace(m.caller, 1) // ignore this function in stack trace

	signals := &signalManager{
		sm:       m.signals.sm.NewChild(),
		parent:   m.signals,
		refcount: atomic.Int64{},
	}
	signals.refcount.Add(1)
	signals.parent.incr()

	sub := Manager{
		ctx:          nil, // maybe set below
		group:        m.group.NewSubgroup(name),
		signals:      signals,
		caller:       &caller,
		onPanic:      m.onPanic,
		pathInGroup:  "main",
		groupPath:    fmt.Sprintf("%s/%s", m.groupPath, name),
		cleanupToken: m.cleanupToken,
	}

	// make sure that that this subgroup's shutdown is propagated into the contexty by registering a
	// shutdown hook to cancel the context
	if m.ctx != nil {
		var cancel context.CancelFunc
		sub.ctx, cancel = context.WithCancel(m.ctx)
		definitelyNotErr := sub.signals.sm.On(sigShutdown{}, context.Background(), Infallible(cancel))
		if definitelyNotErr != nil {
			panic(fmt.Errorf("unexpected error: %w", definitelyNotErr))
		}
	}

	sub.group.Add("main")

	go func() {
		defer sub.group.Done("main")
		defer sub.signals.decr()
		defer sub.MaybeRecover()

		f(sub)
	}()

	return SubgroupHandle{m: sub}
}

func (m Manager) Shutdown(ctx context.Context) error {
	return m.signals.sm.TriggerAndWait(sigShutdown{}, ctx)
}

func (m Manager) OnShutdown(ctx context.Context, callbacks ...func(context.Context) error) error {
	onErr := m.onError
	handler := func(_ context.Context, err error) error {
		return onErr(err)
	}
	return m.signals.sm.WithErrorHandler(handler).On(sigShutdown{}, ctx, callbacks...)
}

func (m Manager) IgnoreParentShutdown() {
	m.signals.sm.Ignore(sigShutdown{})
}

type SubgroupHandle struct {
	m Manager
}

func (h SubgroupHandle) Shutdown(ctx context.Context) error {
	return h.m.Shutdown(ctx)
}

func (h SubgroupHandle) Wait() <-chan struct{} {
	return h.m.group.Wait()
}

func (h SubgroupHandle) TryWait(ctx context.Context) error {
	return h.m.group.TryWait(ctx)
}

func (h SubgroupHandle) TaskTree() TaskTree {
	return h.m.group.TaskTree()
}
