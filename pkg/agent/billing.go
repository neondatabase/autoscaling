package agent

import (
	"context"
	"math"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	vmapi "github.com/neondatabase/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/billing"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type BillingConfig struct {
	URL                 string `json:"url"`
	CPUMetricName       string `json:"cpuMetricName"`
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
	// cpu stores the CPU seconds allocated to the VM, roughly equivalent to the integral of CPU
	// usage over time.
	cpu uint32
}

const (
	EndpointLabel string = "neon/endpoint-id"
)

func RunBillingMetricsCollector(
	backgroundCtx context.Context,
	conf *BillingConfig,
	targetNode string,
	store *util.WatchStore[vmapi.VirtualMachine],
) {
	client := billing.NewClient(conf.URL, http.DefaultClient)

	collectTicker := time.NewTicker(time.Second * time.Duration(conf.CollectEverySeconds))
	defer collectTicker.Stop()
	// Offset by half a second, so it's a bit more deterministic.
	time.Sleep(500 * time.Millisecond)
	pushTicker := time.NewTicker(time.Second * time.Duration(conf.PushEverySeconds))
	defer pushTicker.Stop()

	state := billingMetricsState{
		historical:      make(map[billingMetricsKey]vmMetricsHistory),
		present:         make(map[billingMetricsKey]vmMetricsInstant),
		lastCollectTime: nil,
		pushWindowStart: time.Now(),
	}

	state.collect(conf, targetNode, store)
	batch := client.NewBatch()

	for {
		select {
		case <-collectTicker.C:
			klog.Infof("Collecting billing state")
			state.collect(conf, targetNode, store)
		case <-pushTicker.C:
			klog.Infof("Creating billing batch")
			state.drainAppendToBatch(conf, batch)
			klog.Infof("Pushing billing events (count = %d)", batch.Count())
			if err := pushBillingEvents(conf, batch); err != nil {
				klog.Errorf("Error pushing billing events: %s", err)
				continue
			}
			// Sending was successful; clear the batch.
			batch = client.NewBatch()
		case <-backgroundCtx.Done():
			// If we're being shut down, push the latests events we have before returning.
			klog.Infof("Creating final billing batch")
			state.drainAppendToBatch(conf, batch)
			klog.Infof("Pushing final billing events (count = %d)", batch.Count())
			if err := pushBillingEvents(conf, batch); err != nil {
				klog.Errorf("Error pushing billing events: %s", err)
			}
			return
		}
	}
}

func (s *billingMetricsState) collect(conf *BillingConfig, targetNode string, store *util.WatchStore[vmapi.VirtualMachine]) {
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

	duration := h.lastSlice.endTime.Sub(h.lastSlice.startTime)
	if duration < 0 {
		panic("negative duration")
	}

	seconds := duration.Seconds()
	// TODO: This approach is imperfect. Floating-point math is probably *fine*, but really not
	// something we want to rely on. A "proper" solution is a lot of work, but long-term valuable.
	metricsSeconds := vmMetricsSeconds{
		cpu: uint32(math.Round(float64(h.lastSlice.metrics.cpu) * seconds)),
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

// drainAppendToBatch clears the current history, adding it as events to the batch
func (s *billingMetricsState) drainAppendToBatch(conf *BillingConfig, batch *billing.Batch) {
	now := time.Now()

	for key, history := range s.historical {
		history.finalizeCurrentTimeSlice()

		batch.AddIncrementalEvent(billing.IncrementalEvent{
			MetricName: conf.CPUMetricName,
			Type:       "", // set on JSON marshal
			EndpointID: key.endpointID,
			// TODO: maybe we should store start/stop time in the vmMetricsHistory object itself?
			// That way we can be aligned to collection, rather than pushing.
			StartTime: s.pushWindowStart,
			StopTime:  now,
			Value:     int(history.total.cpu),
		})
	}

	s.pushWindowStart = now
	s.historical = make(map[billingMetricsKey]vmMetricsHistory)
}

func pushBillingEvents(conf *BillingConfig, batch *billing.Batch) error {
	ctx, cancel := context.WithTimeout(context.TODO(), time.Second*time.Duration(conf.PushTimeoutSeconds))
	defer cancel()

	return batch.Send(ctx)
}
