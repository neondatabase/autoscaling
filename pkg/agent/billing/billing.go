package billing

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/billing"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type Config struct {
	URL                  string `json:"url"`
	CPUMetricName        string `json:"cpuMetricName"`
	ActiveTimeMetricName string `json:"activeTimeMetricName"`
	CollectEverySeconds  uint   `json:"collectEverySeconds"`
	PushEverySeconds     uint   `json:"pushEverySeconds"`
	PushTimeoutSeconds   uint   `json:"pushTimeoutSeconds"`
}

type metricsState struct {
	historical      map[metricsKey]vmMetricsHistory
	present         map[metricsKey]vmMetricsInstant
	lastCollectTime *time.Time
	pushWindowStart time.Time
}

type metricsKey struct {
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

func (m *metricsTimeSlice) Duration() time.Duration { return m.endTime.Sub(m.startTime) }

type vmMetricsInstant struct {
	// cpu stores the cpu allocation at a particular instant.
	cpu vmapi.MilliCPU
}

// vmMetricsSeconds is like vmMetrics, but the values cover the allocation over time
type vmMetricsSeconds struct {
	// cpu stores the CPU seconds allocated to the VM, roughly equivalent to the integral of CPU
	// usage over time.
	cpu float64
	// activeTime stores the total time that the VM was active
	activeTime time.Duration
}

const (
	EndpointLabel string = "neon/endpoint-id"
)

func RunBillingMetricsCollector(
	backgroundCtx context.Context,
	conf *Config,
	store VMStoreForNode,
) {
	client := billing.NewClient(conf.URL, http.DefaultClient)

	collectTicker := time.NewTicker(time.Second * time.Duration(conf.CollectEverySeconds))
	defer collectTicker.Stop()
	// Offset by half a second, so it's a bit more deterministic.
	time.Sleep(500 * time.Millisecond)
	pushTicker := time.NewTicker(time.Second * time.Duration(conf.PushEverySeconds))
	defer pushTicker.Stop()

	state := metricsState{
		historical:      make(map[metricsKey]vmMetricsHistory),
		present:         make(map[metricsKey]vmMetricsInstant),
		lastCollectTime: nil,
		pushWindowStart: time.Now(),
	}

	state.collect(conf, store)
	batch := client.NewBatch()

	for {
		select {
		case <-collectTicker.C:
			klog.Infof("Collecting billing state")
			if store.Stopped() && backgroundCtx.Err() == nil {
				panic(errors.New("VM store stopped but background context is still live"))
			}
			state.collect(conf, store)
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

func (s *metricsState) collect(conf *Config, store VMStoreForNode) {
	now := time.Now()

	old := s.present
	s.present = make(map[metricsKey]vmMetricsInstant)
	vmsOnThisNode := store.ListIndexed(func(i *VMNodeIndex) []*vmapi.VirtualMachine {
		return i.List()
	})
	for _, vm := range vmsOnThisNode {
		endpointID, ok := vm.Labels[EndpointLabel]
		if !ok {
			// we're only reporting metrics for VMs with endpoint IDs, and this VM doesn't have one
			continue
		}

		if !vm.Status.Phase.IsAlive() {
			continue
		}

		key := metricsKey{
			uid:        vm.UID,
			endpointID: endpointID,
		}
		presentMetrics := vmMetricsInstant{
			cpu: *vm.Spec.Guest.CPUs.Use,
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
					total:     vmMetricsSeconds{cpu: 0, activeTime: time.Duration(0)},
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

	duration := h.lastSlice.Duration()
	if duration < 0 {
		panic("negative duration")
	}

	// TODO: This approach is imperfect. Floating-point math is probably *fine*, but really not
	// something we want to rely on. A "proper" solution is a lot of work, but long-term valuable.
	metricsSeconds := vmMetricsSeconds{
		cpu:        duration.Seconds() * h.lastSlice.metrics.cpu.AsFloat64(),
		activeTime: duration,
	}
	h.total.cpu += metricsSeconds.cpu
	h.total.activeTime += metricsSeconds.activeTime

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

func logAddedEvent(event billing.IncrementalEvent) billing.IncrementalEvent {
	klog.Infof("Adding event for EndpointID %q: MetricName=%q, Value=%d", event.EndpointID, event.MetricName, event.Value)
	return event
}

// drainAppendToBatch clears the current history, adding it as events to the batch
func (s *metricsState) drainAppendToBatch(conf *Config, batch *billing.Batch) {
	now := time.Now()

	for key, history := range s.historical {
		history.finalizeCurrentTimeSlice()

		batch.AddIncrementalEvent(logAddedEvent(billing.IncrementalEvent{
			MetricName:     conf.CPUMetricName,
			Type:           "", // set in batch method
			IdempotencyKey: "", // set in batch method
			EndpointID:     key.endpointID,
			// TODO: maybe we should store start/stop time in the vmMetricsHistory object itself?
			// That way we can be aligned to collection, rather than pushing.
			StartTime: s.pushWindowStart,
			StopTime:  now,
			Value:     int(math.Round(history.total.cpu)),
		}))
		batch.AddIncrementalEvent(logAddedEvent(billing.IncrementalEvent{
			MetricName:     conf.ActiveTimeMetricName,
			Type:           "", // set in batch method
			IdempotencyKey: "", // set in batch method
			EndpointID:     key.endpointID,
			StartTime:      s.pushWindowStart,
			StopTime:       now,
			Value:          int(math.Round(history.total.activeTime.Seconds())),
		}))
	}

	s.pushWindowStart = now
	s.historical = make(map[metricsKey]vmMetricsHistory)
}

func pushBillingEvents(conf *Config, batch *billing.Batch) error {
	ctx, cancel := context.WithTimeout(context.TODO(), time.Second*time.Duration(conf.PushTimeoutSeconds))
	defer cancel()

	return batch.Send(ctx)
}
