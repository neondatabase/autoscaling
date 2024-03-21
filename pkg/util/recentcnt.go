package util

import "time"

// RecentCounter is a struct that keeps track of recent timestamps within a given interval.
type RecentCounter struct {
	interval   time.Duration
	timestamps []time.Time
}

func NewRecentCounter(interval time.Duration) *RecentCounter {
	return &RecentCounter{
		interval:   interval,
		timestamps: make([]time.Time, 0),
	}
}

func (rc *RecentCounter) cleanup(t time.Time) {
	checkpoint := t.Add(-rc.interval)
	i := 0
	for ; i < len(rc.timestamps); i++ {
		if rc.timestamps[i].After(checkpoint) {
			break
		}
	}
	rc.timestamps = rc.timestamps[i:]
}

// Inc increments the counter and adds the current timestamp to the list of timestamps.
func (rc *RecentCounter) Inc() {
	now := time.Now()
	rc.cleanup(now)
	rc.timestamps = append(rc.timestamps, now)
}

// Get returns the number of recent timestamps stored in the RecentCounter.
func (rc *RecentCounter) Get() uint {
	rc.cleanup(time.Now())
	return uint(len(rc.timestamps))
}
