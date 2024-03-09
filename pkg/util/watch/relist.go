package watch

import (
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
}

func newRelistInfo(incRequestedMetric func()) *relistInfo {
	return &relistInfo{
		mu:                 sync.Mutex{},
		incRequestedMetric: incRequestedMetric,
		triggerRelist:      make(chan struct{}, 1), // ensure sends are non-blocking
		relisted:           make(chan struct{}),
	}
}

// Triggered returns a channel that contains a value when relisting has been requested
func (r *relistInfo) Triggered() <-chan struct{} {
	return r.triggerRelist
}

func (r *relistInfo) Relist() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()

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
	return &relistExecutor{
		r:              r,
		signalComplete: make([]chan struct{}, 0, 1),
		firstIteration: true,
	}
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
func (r *relistExecutor) Complete() {
	for _, ch := range r.signalComplete {
		close(ch)
	}
	r.signalComplete = nil
}
