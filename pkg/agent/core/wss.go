package core

// Working set size estimation
// For more, see: https://www.notion.so/neondatabase/874ef1cc942a4e6592434dbe9e609350

import (
	"fmt"
)

type WssEstimatorConfig struct {
	// MaxAllowedIncreaseFactor is the maximum tolerable increase in slope between windows.
	// If the slope increases by more than this factor, we will cut off the working set size as the
	// border between the two windows.
	MaxAllowedIncreaseFactor float64
	// InitialOffset is the index of the minimum working set size we must consider.
	//
	// In practice, this is taken from the scaling config's LFCMinWaitBeforeDownscaleMinutes, with
	// the expectation that datapoints are all one minute apart, starting at 1m. So a value of 15m
	// translates to an InitialOffset of 14 (-1 because indexes start at zero, but the first
	// datapoint is 1m).
	InitialOffset int
	// WindowSize sets the offset for datapoints used in the calculation of the slope before & after
	// a point. For window size W, we calculate the slope at point P as value[P]-value[P-(W-1)].
	// This value must be >= 2.
	//
	// In practice, this value is taken from the scaling config's LFCWindowSizeMinutes, with the
	// expectation that datapoints are all one minute apart. So, a value of 5 minutes translates to
	// a WindowSize of 5.
	WindowSize int
}

// EstimateTrueWorkingSetSize returns an estimate of the "true" current working set size, given a
// series of datapoints for the observed working set size over increasing time intervals.
//
// In practice, the 'series' is e.g., values of 'neon.lfc_approximate_working_set_size_seconds(d)'
// for equidistant values of 'd' from 1 minute to 60 minutes.
//
// This function panics if:
// * cfg.WindowSize < 2
// * cfg.InitialOffset < cfg.WindowSize - 1
func EstimateTrueWorkingSetSize(
	series []float64,
	cfg WssEstimatorConfig,
) float64 {
	if cfg.WindowSize < 2 {
		panic(fmt.Errorf("cfg.WindowSize must be >= 2 (got %v)", cfg.WindowSize))
	} else if cfg.InitialOffset < cfg.WindowSize-1 {
		panic(fmt.Errorf("cfg.InitialOffset must be >= cfg.WindowSize - 1 (got %v < %v - 1)", cfg.InitialOffset, cfg.WindowSize))
	}

	// For a window size of e.g. 5 points, we're looking back from series[t] to series[t-4], because
	// series[t] is already included. (and similarly for looking forward to series[t+4]).
	// 'w' is a shorthand for that -1 to make the code in the loop below cleaner.
	w := cfg.WindowSize - 1

	for t := cfg.InitialOffset; t < len(series)-w; t += 1 {
		// In theory the HLL estimator will guarantee that - at any instant - increasing the
		// duration for the working set will not decrease the value.
		// However in practice, the individual values are not calculated at the same time, so we
		// must still account for the possibility that series[t] < series[t-w], or similarly for
		// series[t+w] and series[t].
		// Hence, max(0.0, ...)
		d0 := max(0.0, series[t]-series[t-w])
		d1 := max(0.0, series[t+w]-series[t])

		if d1 > d0*cfg.MaxAllowedIncreaseFactor {
			return series[t]
		}
	}

	return series[len(series)-1]
}

// ProjectNextHighest looks at the rate of change between points in 'series', returning the maximum
// value if any of these slopes were to continue for 'projectLen' additional datapoints.
//
// For example, given the series '0, 1, 3, 4, 5', projectLen of 3, and ceil equal to 6,
// ProjectNextHighest will return 9 (because 1 → 3 would reach 9 if it continued for another 3
// datapoints (→ 5 → 7 → 9).
//
// Internally, ProjectNextHighest is used to allow preemptive scale-up when we can see that the
// observed working set size is increasing, but we don't know how big it'll get.
// In short, this function helps answer: "How much should we scale-up to accommodate expected
// increases in demand?".
func ProjectNextHighest(series []float64, projectLen float64) float64 {
	if len(series) < 2 {
		panic(fmt.Errorf("cannot ProjectNextHighest with series of length %d (must be >= 2)", len(series)))
	}

	highest := series[0]
	for i := 1; i < len(series); i += 1 {
		x0 := series[i-1]
		x1 := max(x0, series[i]) // ignore decreases
		predicted := x1 + (x1-x0)*projectLen
		highest = max(highest, predicted)
	}

	return highest
}
