package agent

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	vmapi "github.com/neondatabase/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/billing"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type BillingConfig struct {
	URL                 string `json:"url"`
	CollectEverySeconds uint   `json:"collectEverySeconds"`
	PushEverySeconds    uint   `json:"pushEverySeconds"`
	PushTimeoutSeconds  uint   `json:"pushTimeoutSeconds"`
}

type billingMetricsState struct {
	historical      map[billingMetricsKey]vmMetricsHistory
	present         map[billingMetricsKey]vmMetricsInstant
	lastCollectTime *time.Time
	pushWindowStart time.Time
}

type billingMetricsKey struct {
	uid        types.UID
	endpointID string
}

type vmMetricsHistory struct {
	lastSlice *metricsTimeSlice
	total     vmMetricsSeconds
}

type metricsTimeSlice struct {
	metrics   vmMetricsInstant
	startTime time.Time
	endTime   time.Time
}

type vmMetricsInstant struct {
	// cpu stores the cpu allocation at a particular instant.
	cpu uint16
}

// vmMetricsSeconds is like vmMetrics, but the values cover the allocation over time
type vmMetricsSeconds struct {
	// cpu stores the cpu seconds allocated to the VM, roughly equivalent to the integral of CPU
	// usage over time.
	cpu uint32
}

const (
	MetricNameCPU string = "cpu"
	EndpointLabel string = "neon/endpoint-id"
)

func RunBillingMetricsColllector(backgroundCtx context.Context, conf *BillingConfig, store *util.WatchStore[vmapi.VirtualMachine]) {
	client := billing.NewClient(conf.URL, http.DefaultClient)

	collectTicker := time.NewTicker(time.Second * time.Duration(conf.CollectEverySeconds))
	defer collectTicker.Stop()
	pushTicker := time.NewTicker(time.Second * time.Duration(conf.PushEverySeconds))
	defer pushTicker.Stop()

	state := billingMetricsState{
		historical:      make(map[billingMetricsKey]vmMetricsHistory),
		present:         make(map[billingMetricsKey]vmMetricsInstant),
		lastCollectTime: nil,
		pushWindowStart: time.Now(),
	}

	state.collect(conf, store)

	for {
		select {
		case <-collectTicker.C:
			state.collect(conf, store)
		case <-pushTicker.C:
			batch := state.makeBatch(&client)
			if err := pushBillingEvents(conf, batch); err != nil {
				klog.Errorf("Error pushing billing events: %s", err)
				continue
			}
			// Sending was successful; reset state
			state.clear()
		case <-backgroundCtx.Done():
			// If we're being shut down, push the latests events we have before returning.
			batch := state.makeBatch(&client)
			if err := pushBillingEvents(conf, batch); err != nil {
				klog.Errorf("Error pushing billing events: %s", err)
			}
			return
		}
	}
}

func (s *billingMetricsState) collect(conf *BillingConfig, store *util.WatchStore[vmapi.VirtualMachine]) {
	now := time.Now()

	old := s.present
	s.present = make(map[billingMetricsKey]vmMetricsInstant)
	for _, vm := range store.Items() {
		endpointID, ok := vm.Labels[EndpointLabel]
		if !ok {
			// we're only reporting metrics for VMs with endpoint IDs, and this VM doesn't have one
			continue
		}

		key := billingMetricsKey{
			uid:        vm.UID,
			endpointID: endpointID,
		}
		presentMetrics := vmMetricsInstant{
			cpu: uint16(*vm.Spec.Guest.CPUs.Use),
		}
		if oldMetrics, ok := old[key]; ok {
			// The VM was present from s.lastTime to now. Add a time slice to its metrics history.
			timeSlice := metricsTimeSlice{
				metrics: vmMetricsInstant{
					// strategically under-bill by assigning the minimum to the entire time slice.
					cpu: util.Min(oldMetrics.cpu, presentMetrics.cpu),
				},
				// note: we know s.lastTime != nil because otherwise old would be empty.
				startTime: *s.lastCollectTime,
				endTime:   now,
			}

			vmHistory, ok := s.historical[key]
			if !ok {
				vmHistory = vmMetricsHistory{
					lastSlice: nil,
					total:     vmMetricsSeconds{cpu: 0},
				}
			}
			// append the slice, merging with the previous if the resource usage was the same
			vmHistory.appendSlice(timeSlice)
			s.historical[key] = vmHistory
		}

		s.present[key] = presentMetrics
	}

	s.lastCollectTime = &now
}

func (h *vmMetricsHistory) appendSlice(timeSlice metricsTimeSlice) {
	// Try to extend the existing period of continuous usage
	if h.lastSlice != nil && h.lastSlice.tryMerge(timeSlice) {
		return
	}

	// Something's new. Push previous time slice, start new one:
	h.finalizeCurrentTimeSlice()
	h.lastSlice = &timeSlice
}

// finalizeCurrentTimeSlice pushes the current time slice onto h.total
//
// This ends up rounding down the total time spent on a given time slice, so it's best to defer
// calling this function until it's actually needed.
func (h *vmMetricsHistory) finalizeCurrentTimeSlice() {
	if h.lastSlice == nil {
		return
	}

	duration := h.lastSlice.endTime.Sub(h.lastSlice.startTime).Nanoseconds()
	if duration < 0 {
		panic("negative duration")
	}

	// note: we could've used Duration.Seconds() above, but that gives us a float. We absolutely do
	// not want to be doing billing math with floats.
	//
	// TODO: This approach here explicitly rounds us down - otherwise rounding is actually quite
	// tricky, and it's safer to round down than up. In the future, we might internally store things
	// as milliseconds or something, and round down to the nearest "CPU second" when sending the
	// batch, in order to be more accurate. There's a variety of options.
	seconds := duration / time.Second.Nanoseconds()
	metricsSeconds := vmMetricsSeconds{
		cpu: uint32(h.lastSlice.metrics.cpu) * uint32(seconds),
	}
	h.total.cpu += metricsSeconds.cpu

	h.lastSlice = nil
}

// tryMerge attempts to merge s and next (assuming that next is after s), returning true only if
// that merging was successful.
//
// Merging may fail if s.endTime != next.startTime or s.metrics != next.metrics.
func (s *metricsTimeSlice) tryMerge(next metricsTimeSlice) bool {
	merged := s.endTime == next.startTime && s.metrics == next.metrics
	if merged {
		s.endTime = next.endTime
	}
	return merged
}

// clear updates the state to remove any previously un-pushed history
func (s *billingMetricsState) clear() {
	s.historical = make(map[billingMetricsKey]vmMetricsHistory)
}

// makeBatch creates a billing.Batch for the currently stored history, without modfiying any of it
func (s *billingMetricsState) makeBatch(client *billing.Client) *billing.Batch {
	batch := client.NewBatch()

	now := time.Now()

	for key, history := range s.historical {
		batch.AddIncrementalEvent(billing.IncrementalEvent{
			IdempotencyKey: uuid.NewString(),
			MetricName:     MetricNameCPU,
			Type:           "", // set on JSON marshal
			EndpointID:     key.endpointID,
			// TODO: maybe we should store start/stop time in the vmMetricsHistory object itself?
			// That way we can be aligned to collection, rather than pushing.
			StartTime: s.pushWindowStart,
			StopTime:  now,
			Value:     history.totalCPUSeconds(),
		})
	}

	s.pushWindowStart = now
	return batch
}

// totalCPUSeconds returns the integer CPU seconds corresponding to the allocation provided to the VM over
// the timespan recorded in this vmMetricsHistory.
func (h vmMetricsHistory) totalCPUSeconds() int {
	// note: finalizeCurrentTimeSlice does not modify data behind any pointers in h, so we can
	// safely call this without worrying about messing up our ongoing state.
	h.finalizeCurrentTimeSlice()
	return int(h.total.cpu)
}

func pushBillingEvents(conf *BillingConfig, batch *billing.Batch) error {
	ctx, cancel := context.WithTimeout(context.TODO(), time.Second*time.Duration(conf.PushTimeoutSeconds))
	defer cancel()

	return batch.Send(ctx)
}
