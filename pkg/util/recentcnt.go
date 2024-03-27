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

// cleanup removes all timestamps that are beyond the interval from the current time.
func (rc *RecentCounter) cleanup(now time.Time) {
	checkpoint := now.Add(-rc.interval)
	i := 0
	for ; i < len(rc.timestamps); i++ {
		if rc.timestamps[i].After(checkpoint) {
			break
		}
	}
	rc.timestamps = rc.timestamps[i:]
}

// inc is seperated from its exported version to provide more flexibity around testing.
func (rc *RecentCounter) inc(now time.Time) {
	rc.cleanup(now)
	rc.timestamps = append(rc.timestamps, now)
}

// get is seperated from its exported version to provide more flexibity around testing.
func (rc *RecentCounter) get(now time.Time) uint {
	rc.cleanup(now)
	return uint(len(rc.timestamps))
}

// Inc increments the counter and adds the current timestamp to the list of timestamps.
func (rc *RecentCounter) Inc() {
	rc.inc(time.Now())
}

// Get returns the number of recent timestamps stored in the RecentCounter.
func (rc *RecentCounter) Get() uint {
	return rc.get(time.Now())
}
