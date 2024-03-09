package watch

import (
	"fmt"
	"sync"
)

// relistInfo encapsulates the logic backing (*Store[T]).Relist()
type relistInfo struct {
	mu sync.Mutex

	incRequestedMetric func()

	// triggerRelist has capacity=1 and *if* the channel contains an item, then relisting has been
	// requested by some call to (*relistInfo).Relist().
	triggerRelist chan struct{}
	// relisted is replaced whenever a new list request *starts* and closed once one completes
	// successfully.
	// Each time listing fails, we store the old channel in the relistExecutor, closing all the
	// channels at once only when listing is successful.
	//
	// Refer to the usage in (*relistExecutor).CollectTriggeredRequests() for more.
	relisted chan struct{}

	// relistCount is the number of instances of relisted that have been closed.
	//
	// Externally, we only guarantee that this is monotonically increasing; internally, it's
	// associated each time with a single instance of `relisted`, so that RelistIfHaventSince can
	// hook into ongoing execution.
	relistCount RelistCount

	// executor stores a reference to the current relistExecutor, if there is one active.
	executor *relistExecutor
}

// RelistCount is a monotonically increasing value to track (approximately) the number of relists
// that have happened
//
// RelistCounts are returned by (*Store[T]).RelistCount{Upper,Lower}Bound() and typically used in
// (*Store[T]).RelistIfHaventSince(). Refer to those functions for more info.
type RelistCount int64

func newRelistInfo(incRequestedMetric func()) *relistInfo {
	return &relistInfo{
		mu:                 sync.Mutex{},
		incRequestedMetric: incRequestedMetric,
		triggerRelist:      make(chan struct{}, 1), // ensure sends are non-blocking
		relisted:           make(chan struct{}),
		relistCount:        0,
		executor:           nil,
	}
}

// Triggered returns a channel that contains a value when relisting has been requested
func (r *relistInfo) Triggered() <-chan struct{} {
	return r.triggerRelist
}

func (r *relistInfo) Relist() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lockedRelist()
}

func (r *relistInfo) lockedRelist() <-chan struct{} {
	// Because triggerRelist has capacity=1, failing to immediately send to the channel means that
	// there's already a signal to request relisting that has not yet been processed.
	select {
	case r.triggerRelist <- struct{}{}:
		// Only increment the metric if it's a "fresh" request (i.e. deduplicate before the metric).
		// TODO: this is only for compatibility with the original definition of the metric; we
		// should do this on every call to Relist().
		r.incRequestedMetric()
	default:
	}

	// note: r.relisted is replaced immediately before every attempt at the API call for relisting,
	// so that there's a strict happens-before relation that guarantees that *when* r.relisted is
	// closed, the relevant List call *must* have happened after any attempted send on
	// r.triggerRelist.
	return r.relisted
}

var closedChan = makeClosedChan()

func makeClosedChan() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func (r *relistInfo) RelistIfHaventSince(c RelistCount) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()

	if c < r.relistCount {
		return closedChan
	}

	upperBound := r.lockedRelistCountUpperBound()
	// we know c >= r.relistCount, so if c < upperBound we know that the higher upper bound comes
	// from the executor.
	if c < upperBound {
		// doesn't matter which channel we return (they'll all get closed at once)
		// So it's easiest just to return the first.
		return r.executor.signalComplete[0]
	}

	// Otherwise, the relist hasn't already happened, and there isn't one in progress that will
	// satisfy what we're looking for, so we need to start a fresh one.
	return r.lockedRelist()
}

func (r *relistInfo) RelistCountUpperBound() RelistCount {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lockedRelistCountUpperBound()
}

func (r *relistInfo) lockedRelistCountUpperBound() RelistCount {
	c := r.relistCount
	if r.executor != nil {
		c += RelistCount(len(r.executor.signalComplete))
	}

	return c
}

type relistExecutor struct {
	r *relistInfo

	// Every time we make a new request, we create a channel for it. That's because we need
	// to make sure that any user's call to WatchStore.Relist() that happens *while* we're
	// actually making the request to K8s won't get overwritten by that request. Basically,
	// we need to make sure that relisting is only marked as complete if there was a request
	// that occurred *after* the call to Relist() returned.
	//
	// There's probably other ways we could do this - it's an area for possible improvement.
	//
	// Note: if we didn't do this at all, the alternative would be to ignore additional
	// relist requests, having them handled naturally as we get around to watching again.
	// This can amplify request failures - particularly if the K8s API server is overloaded.
	signalComplete []chan struct{}

	// firstIteration is true on the first call to
	firstIteration bool
}

// Executor returns a helper to handle the process of actually performing the relisting
//
// The reason this is its own type is because we need to take care to handle new requests properly,
// and part of that is storing any individual request channels that get collected as part of
// retrying on failure.
func (r *relistInfo) Executor() *relistExecutor {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.executor != nil {
		panic(fmt.Errorf("(*relistInfo).Executor() called without previous executor Complete()'d"))
	}

	e := &relistExecutor{
		r:              r,
		signalComplete: make([]chan struct{}, 0, 1),
		firstIteration: true,
	}

	r.executor = e
	return e
}

// CollectTriggeredRequests SHOULD be called before each attempt at relisting, until one is
// successful.
func (e *relistExecutor) CollectTriggeredRequests() {
	e.r.mu.Lock()
	defer e.r.mu.Unlock()

	newRelistTriggered := false

	// Consume any additional relist request.
	// All usage of triggerRelist from within (*Store[T]).Relist() is asynchronous,
	// because triggerRelist has capacity=1 and has an item in it iff relisting has
	// been requested, so if Relist() *would* block on sending, the signal has
	// already been given.
	// That's all to say: Receiving only once from triggerRelist is sufficient.
	select {
	case <-e.r.Triggered():
		newRelistTriggered = true
	default:
	}

	if e.firstIteration || newRelistTriggered {
		e.signalComplete = append(e.signalComplete, e.r.relisted)
		e.r.relisted = make(chan struct{})
	}

	e.firstIteration = false
}

// Complete closes any and all channels that were previously returned by (*relistInfo).relist() and
// collected as part of this executor.
func (e *relistExecutor) Complete() {
	e.r.mu.Lock()
	defer e.r.mu.Unlock()

	for _, ch := range e.signalComplete {
		close(ch)
	}
	e.r.relistCount += RelistCount(len(e.signalComplete))

	// decouple this executor
	e.r.executor = nil
	e.r = nil
}
