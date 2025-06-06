package billing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"go.uber.org/zap"

	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/reporting"
	"github.com/neondatabase/autoscaling/pkg/util/taskgroup"
)

type Config struct {
	Clients                ClientsConfig `json:"clients"`
	CPUMetricName          string        `json:"cpuMetricName"`
	ActiveTimeMetricName   string        `json:"activeTimeMetricName"`
	CollectEverySeconds    uint          `json:"collectEverySeconds"`
	AccumulateEverySeconds uint          `json:"accumulateEverySeconds"`
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
	cpu vmv1.MilliCPU
}

// vmMetricsSeconds is like vmMetrics, but the values cover the allocation over time
type vmMetricsSeconds struct {
	// cpu stores the CPU seconds allocated to the VM, roughly equivalent to the integral of CPU
	// usage over time.
	cpu float64
	// activeTime stores the total time that the VM was active
	activeTime time.Duration
}

type MetricsCollector struct {
	conf    *Config
	sink    *reporting.EventSink[*IncrementalEvent]
	metrics PromMetrics
}

func NewMetricsCollector(
	ctx context.Context,
	parentLogger *zap.Logger,
	conf *Config,
	metrics PromMetrics,
) (*MetricsCollector, error) {
	logger := parentLogger.Named("billing")

	clients, err := createClients(ctx, logger, conf.Clients)
	if err != nil {
		return nil, err
	}

	sink := reporting.NewEventSink(logger, metrics.reporting, clients...)

	return &MetricsCollector{
		conf:    conf,
		sink:    sink,
		metrics: metrics,
	}, nil
}

func (mc *MetricsCollector) Run(
	ctx context.Context,
	logger *zap.Logger,
	store VMStoreForNode,
) error {
	tg := taskgroup.NewGroup(logger, taskgroup.WithParentContext(ctx))

	// note: sink has its own context, so that it is canceled only after runCollector finishes.
	sinkCtx, cancelSink := context.WithCancel(context.Background())
	defer cancelSink() // make sure resources are cleaned up

	tg.Go("collect", func(logger *zap.Logger) error {
		defer cancelSink() // cancel event sending *only when we're done collecting*
		return mc.runCollector(tg.Ctx(), logger, store)
	})

	tg.Go("sink-run", func(logger *zap.Logger) error {
		err := mc.sink.Run(sinkCtx) // note: NOT tg.Ctx(); see more above.
		if err != nil {
			return fmt.Errorf("billing events sink failed: %w", err)
		}
		return nil
	})

	return tg.Wait()
}

func (mc *MetricsCollector) runCollector(
	ctx context.Context,
	logger *zap.Logger,
	store VMStoreForNode,
) error {
	collectTicker := time.NewTicker(time.Second * time.Duration(mc.conf.CollectEverySeconds))
	defer collectTicker.Stop()
	// Offset by half a second, so it's a bit more deterministic.
	time.Sleep(500 * time.Millisecond)
	accumulateTicker := time.NewTicker(time.Second * time.Duration(mc.conf.AccumulateEverySeconds))
	defer accumulateTicker.Stop()

	state := metricsState{
		historical:      make(map[metricsKey]vmMetricsHistory),
		present:         make(map[metricsKey]vmMetricsInstant),
		lastCollectTime: nil,
		pushWindowStart: time.Now(),
	}

	state.collect(logger, store, mc.metrics)

	for {
		select {
		case <-collectTicker.C:
			logger.Info("Collecting billing state")
			if store.Stopped() && ctx.Err() == nil {
				err := errors.New("VM store stopped but background context is still live")
				logger.Panic("Validation check failed", zap.Error(err))
				return err
			}
			state.collect(logger, store, mc.metrics)
		case <-accumulateTicker.C:
			logger.Info("Creating billing batch")
			state.drainEnqueue(logger, mc.conf, GetHostname(), mc.sink)
		case <-ctx.Done():
			return nil
		}
	}
}

func (s *metricsState) collect(logger *zap.Logger, store VMStoreForNode, metrics PromMetrics) {
	now := time.Now()

	metricsBatch := metrics.forBatch()
	defer metricsBatch.finish() // This doesn't *really* need to be deferred, but it's up here so we don't forget

	old := s.present
	s.present = make(map[metricsKey]vmMetricsInstant)
	var vmsOnThisNode []*vmv1.VirtualMachine
	if store.Failing() {
		logger.Error("VM store is currently stopped. No events will be recorded")
	} else {
		vmsOnThisNode = store.ListIndexed(func(i *VMNodeIndex) []*vmv1.VirtualMachine {
			return i.List()
		})
	}
	for _, vm := range vmsOnThisNode {
		endpointID, isEndpoint := vm.Annotations[api.AnnotationBillingEndpointID]
		metricsBatch.inc(isEndpointFlag(isEndpoint), autoscalingEnabledFlag(api.HasAutoscalingEnabled(vm)), vm.Status.Phase)
		if !isEndpoint {
			// we're only reporting metrics for VMs with endpoint IDs, and this VM doesn't have one
			continue
		}

		if !vm.Status.Phase.IsAlive() || vm.Status.CPUs == nil {
			continue
		}

		key := metricsKey{
			uid:        vm.UID,
			endpointID: endpointID,
		}
		presentMetrics := vmMetricsInstant{
			cpu: *vm.Status.CPUs,
		}
		if oldMetrics, ok := old[key]; ok {
			// The VM was present from s.lastTime to now. Add a time slice to its metrics history.
			timeSlice := metricsTimeSlice{
				metrics: vmMetricsInstant{
					// strategically under-bill by assigning the minimum to the entire time slice.
					cpu: min(oldMetrics.cpu, presentMetrics.cpu),
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
// Merging may fail if m.endTime != next.startTime or m.metrics != next.metrics.
func (m *metricsTimeSlice) tryMerge(next metricsTimeSlice) bool {
	merged := m.endTime == next.startTime && m.metrics == next.metrics
	if merged {
		m.endTime = next.endTime
	}
	return merged
}

func logAddedEvent(logger *zap.Logger, event *IncrementalEvent) *IncrementalEvent {
	logger.Info(
		"Adding event to batch",
		zap.String("IdempotencyKey", event.IdempotencyKey),
		zap.String("EndpointID", event.EndpointID),
		zap.String("MetricName", event.MetricName),
		zap.Int("Value", event.Value),
	)
	return event
}

// drainEnqueue clears the current history, adding it as events to the queue
func (s *metricsState) drainEnqueue(
	logger *zap.Logger,
	conf *Config,
	hostname string,
	sink *reporting.EventSink[*IncrementalEvent],
) {
	now := time.Now()

	countInBatch := 0
	batchSize := 2 * len(s.historical)

	enqueue := sink.Enqueue

	for key, history := range s.historical {
		history.finalizeCurrentTimeSlice()

		countInBatch += 1
		enqueue(logAddedEvent(logger, enrichEvents(now, hostname, countInBatch, batchSize, &IncrementalEvent{
			MetricName:     conf.CPUMetricName,
			Type:           "", // set by enrichEvents
			IdempotencyKey: "", // set by enrichEvents
			EndpointID:     key.endpointID,
			// TODO: maybe we should store start/stop time in the vmMetricsHistory object itself?
			// That way we can be aligned to collection, rather than pushing.
			StartTime: s.pushWindowStart,
			StopTime:  now,
			Value:     int(math.Round(history.total.cpu)),
		})))
		countInBatch += 1
		enqueue(logAddedEvent(logger, enrichEvents(now, hostname, countInBatch, batchSize, &IncrementalEvent{
			MetricName:     conf.ActiveTimeMetricName,
			Type:           "", // set by enrichEvents
			IdempotencyKey: "", // set by enrichEvents
			EndpointID:     key.endpointID,
			StartTime:      s.pushWindowStart,
			StopTime:       now,
			Value:          int(math.Round(history.total.activeTime.Seconds())),
		})))
	}

	s.pushWindowStart = now
	s.historical = make(map[metricsKey]vmMetricsHistory)
}
