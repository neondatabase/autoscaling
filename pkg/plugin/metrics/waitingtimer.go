package metrics

// Helper for reconcile metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// WaitingTimer records the amount of time spent waiting, given occasional state transitions between
// "waiting" / "not waiting".
//
// This is useful to measure the saturation of our reconcile workers because it helps isolate the
// impact in aggregate metrics from groups of operations that are all waiting at the same time.
//
// (In essense, it's like the difference between load average and Linux PSI metrics.)
type WaitingTimer struct {
	mu sync.Mutex

	waiting        bool
	lastTransition time.Time
	totalTime      time.Duration

	gauge prometheus.GaugeFunc
}

func NewWaitingTimer(opts prometheus.Opts) *WaitingTimer {
	t := &WaitingTimer{
		mu:             sync.Mutex{},
		waiting:        false,
		lastTransition: time.Now(),
		totalTime:      0,
		gauge:          nil, // set below
	}

	t.gauge = prometheus.NewGaugeFunc(prometheus.GaugeOpts(opts), func() float64 {
		return t.getTotal().Seconds()
	})

	return t
}

func (t *WaitingTimer) SetWaiting(waiting bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.waiting == waiting {
		return
	}

	now := time.Now()
	// If it was previously marked as waiting (but now no longer), then add the time since we
	// started waiting to the running total.
	if t.waiting {
		t.totalTime += now.Sub(t.lastTransition)
	}
	// Otherwise, we'll mark ourselves as waiting (or not), and set the last change to now so that
	// the next switch records the time since now.
	t.waiting = waiting
	t.lastTransition = now
}

func (t *WaitingTimer) getTotal() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	total := t.totalTime
	if t.waiting {
		total += time.Since(t.lastTransition)
	}
	return total
}

// Describe implements prometheus.Collector
func (t *WaitingTimer) Describe(ch chan<- *prometheus.Desc) {
	t.gauge.Describe(ch)
}

func (t *WaitingTimer) Collect(ch chan<- prometheus.Metric) {
	t.gauge.Collect(ch)
}
