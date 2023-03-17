package task

// Definition of task.Manager, alongside a handful of helper types.

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

// ManagerKey is a unique value that can be used as a key for context.Value to set/get a Manager.
var ManagerKey managerKey

type managerKey struct{}

// Manager provides a per-goroutine task management interface.
//
// It's expected that each goroutine will have its own Manager, which should then be *exclusively*
// used to spawn new goroutines. In other words: there should be no bare calls to 'go f()' outside
// this package; you should use Manager.Spawn or Manager.SpawnAsSubgroup instead.
//
// Broadly, the primary task-running methods are Manager.Spawn (run a single function) and
// Manager.SpawnAsSubgroup (run a function as the start of a "subgroup" - see below).
// There's also a handful of methods for modifying the Manager in various ways - usually starting
// with "With", which return a modified Manager.
//
// Shutdown hooks are registered with Manager.OnShutdown and called in reverse order by
// Manager.Shutdown (refer to OnShutdown for more information). See also Manager.ShutdownOnSigterm
// and Manager.IgnoreParentShutdown
//
// And finally, a handful of methods are provided to retrieve bits of per-task or per-group
// information - see Manager.Context and Manager.Caller.
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

// LogFatalError is an ErrorHandler that, on any error, calls klog.Fatalf(format, err). Typical
// usage might look like:
//
//	tm.WithErrorHandler(LogFatalError("Server shutdown failed: %w")).
//	 	OnShutdown(context.TODO(), httpServer.Shutdown)
func LogFatalError(format string) ErrorHandler {
	return func(err error) error {
		klog.Fatalf(format, err)
		return nil
	}
}

// LogPanic is a PanicHandler that emits an error log describing the panic, with a format roughly
// equivalent to the default display representation for unhandled panics.
func LogPanic(taskName string, err any, stackTrace chord.StackTrace) {
	klog.Errorf("task %s panicked with %v:\n%s", taskName, err, stackTrace.String())
}

// LogPanicAndExit is a PanicHandler that logs the panic (via LogPanic) and calls os.Exit.
//
// FIXME: This might run us into issues? Depending on the implementation of klog, we might end up
// exiting without actually flushing to stdout/stderr.
func LogPanicAndExit(taskName string, err any, stackTrace chord.StackTrace) {
	LogPanic(taskName, err, stackTrace)
	os.Exit(2)
}

// LogPanicAndShutdown is a PanicHandler that logs the panic (via LogPanic) and calls m.Shutdown
// with a context provided by makeCtx.
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

// WrapOnError returns a function wrapping f that will return fmt.Errorf(format, err) if f returns a
// non-nil error, and nil otherwise.
func WrapOnError(format string, f func(context.Context) error) func(context.Context) error {
	return func(c context.Context) error {
		if err := f(c); err != nil {
			return fmt.Errorf(format, err)
		}
		return nil
	}
}

// Infallible converts a function that does not return error into one that returns nil (and takes an
// unused Context).
//
// This is typically used when registering hooks with Manager.OnShutdown. For example:
//
//	func CloseSignalChannelOnShutdown(m Manager, sig chan struct{}) {
//	 	_ = m.OnShutdown(context.TODO(), Infallible(func() { close(sig) }))
//	}
func Infallible(f func()) func(context.Context) error {
	return func(context.Context) error {
		f()
		return nil
	}
}

// NewRootManager creates a new base Manager, typically done at program startup. Even for tests,
// it's preferable to have a single root Manager, using subgroups for each test.
func NewRootManager(name string) Manager {
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
		onError:      nil,
		onPanic:      nil,
		pathInGroup:  "main",
		groupPath:    name,
		cleanupToken: cleanupToken,
	}
}

// FullName returns the full name of the task. For specifics, refer to the function implementation
// (it's quite short).
func (m Manager) FullName() string {
	return fmt.Sprintf("group{%s}-task{%s}", m.groupPath, m.pathInGroup)
}

// ShutdownOnSigterm sets the Manager to call Manager.Shutdown when SIGTERM occurs.
func (m Manager) ShutdownOnSigterm(makeContext func() (context.Context, context.CancelFunc)) {
	onErr := m.onError
	handler := func(_ context.Context, err error) error {
		if onErr != nil {
			err = onErr(err)
		}
		return err
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

// Context returns a Context that is canceled once Shutdown has been triggered. If
// Manager.WithContext was previously called, the returned Context will be derived from that Context
// (but it still has the guarantees about shutdown).
func (m Manager) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	} else {
		return m.signals.sm.Context(sigShutdown{})
	}
}

// Caller returns the stack trace at the point this Manager's goroutine was spawned, if there was
// one. The returned chord.StackTrace may be nil.
func (m Manager) Caller() *chord.StackTrace {
	return m.caller
}

// WithCaller returns a Manager with the calling goroutine overriden, typically used for managing
// stack traces.
func (m Manager) WithCaller(trace *chord.StackTrace) Manager {
	m.caller = trace
	return m
}

func (m Manager) WithContext(ctx context.Context) Manager {
	m.ctx = ctx
	return m
}

// WithShutdownErrorHandler sets an ErrorHandler for shutdown hooks (see Manager.OnShutdown for
// more). If onError is nil, then all error handlers will be removed. If there is already an error
// handler, then it will be called after onError if onError returns a non-nil error.
//
// In general, if all ErrorHandlers, when called in sequence, all return a non-nil error, then that
// will be returned by Shutdown.
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

// MaybeRecover implements the "defer, run panic handler" logic used internally. This method is only
// really made public so that it can be used alongside Manager.NewForTask - for example:
//
//	func run(tm Manager, f func()) {
//		tm, done := tm.NewForTask("run")
//		defer done()
//		defer tm.MaybeRecover()
//
//		f()
//	}
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

// NewForTask creates a new Manager with a given task name, returning a cleanup function alongside
// it.
//
// done *must* be called when the task is finished.
//
// The returned Manager will not have its caller set. See also: Manager.WithCaller.
func (m Manager) NewForTask(name string) (_ Manager, done func()) {
	m.group.Add(name)
	m.signals.incr()

	if m.pathInGroup == "main" {
		m.pathInGroup = name
	} else {
		m.pathInGroup = fmt.Sprintf("%s/%s", m.pathInGroup, name)
	}
	klog.Infof("Starting task %q", m.FullName())

	return m, func() {
		m.group.Done(name)
		m.signals.decr()
		klog.Infof("Task %q ended", m.FullName())
	}
}

// Spawn runs the function in a new goroutine, with the name provided. The Manager passed to f will
// be derived from m, and specific to its goroutine.
func (m Manager) Spawn(name string, f func(Manager)) {
	caller := chord.GetStackTrace(m.caller, 1) // ignore this function in stack trace

	child, done := m.WithCaller(&caller).NewForTask(name)
	go func() {
		defer done()
		defer child.MaybeRecover()
		f(child)
	}()
}

// SpawnAsSubgroup runs the function in a new goroutine, creating a new task "subgroup" to hold it.
// The returned SubgroupHandle provides a handful of methods for interacting with the tasks spawned
// in that group - refer to its documentation for more information.
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
		onError:      m.onError,
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
	klog.Infof("Starting task %q", sub.FullName())

	go func() {
		defer sub.group.Done("main")
		defer sub.signals.decr()
		defer sub.MaybeRecover()
		klog.Infof("Task %q ended", sub.FullName())

		f(sub)
	}()

	return SubgroupHandle{m: sub}
}

// Shutdown runs all hooks registered with Manger.OnShutdown, for this task group and all subgroups.
//
// The context will be passed to all hooks. For more information, see Manager.OnShutdown.
//
// If Shutdown is called while currently in progress, it will block until shutting down is complete.
// The new context will be completely ignored. Only the original caller will receive any error.
func (m Manager) Shutdown(ctx context.Context) error {
	return m.signals.sm.TriggerAndWait(sigShutdown{}, ctx)
}

// OnShutdown registers a hook to be run on shutdown - whether that's triggered by Manager.Shutdown,
// SubgroupHandle.Shutdown, or via some signal.
//
// The callbacks are run in reverse order (including across calls to OnShutdown), and any unhandled
// error (i.e. all ErrorHandlers still return a non-nil error) will be returned, aborting the
// shutdown sequence.
//
// If the Manager's shutdown has already been triggered, then each callback will be immediately
// called with the provided Context, with any error returned. No error will be returned, and the
// context will be unused, if shutdown has not yet been triggered.
//
// For subgroups, the ordering of calling hooks is determined by when the subgroup was created. All
// hooks in a subgroup run together.
func (m Manager) OnShutdown(ctx context.Context, callbacks ...func(context.Context) error) error {
	onErr := m.onError
	handler := func(_ context.Context, err error) error {
		if onErr != nil {
			err = onErr(err)
		}
		return err
	}
	return m.signals.sm.WithErrorHandler(handler).On(sigShutdown{}, ctx, callbacks...)
}

// IgnoreParentShutdown modifies this Manager so that any call to Shutdown on a parent task group
// (via SpawnAsSubgroup)
//
// Broadly, IgnoreParentShutdown will do "the right thing" when the parent's shutdown has already
// been triggered: it will be un-triggered if this Manager's shutdown has not yet been observed, but
// it'll remain as-is if it has been.
func (m Manager) IgnoreParentShutdown() {
	m.signals.sm.Ignore(sigShutdown{})
}

// SubgroupHandle provides some ways to interact with a task subgroup, created by
// Manager.SpawnAsSubgroup.
//
// Refer to the provided methods for more.
type SubgroupHandle struct {
	m Manager
}

// Shutdown runs all shutdown hooks in the subgroup, passing the context and returning any error.
//
// If Shutdown is called while currently in progress, it will block until shutting down is complete.
// The new context will be completely ignored. Only the original caller will receive any error.
func (h SubgroupHandle) Shutdown(ctx context.Context) error {
	return h.m.Shutdown(ctx)
}

// Wait returns a channel that will be closed once all tasks in the subgroup have ended.
func (h SubgroupHandle) Wait() <-chan struct{} {
	return h.m.group.Wait()
}

// TryWait waits for all tasks in the subgroup to finish, or the context to be canceled. Once the
// context is canceled, its error will be returned.
func (h SubgroupHandle) TryWait(ctx context.Context) error {
	return h.m.group.TryWait(ctx)
}

// TaskTree returns a representation of the structure of currently running tasks in this subgroup.
// This is primarily intended for debugging only, used in various "dump state" endpoints.
func (h SubgroupHandle) TaskTree() TaskTree {
	return h.m.group.TaskTree()
}
