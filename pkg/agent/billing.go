package agent

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"k8s.io/apimachinery/pkg/types"

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
	historical map[billingMetricsKey]vmMetricsHistory
	present    map[billingMetricsKey]vmMetrics
	lastTime   *time.Time
}

type billingMetricsKey struct {
	uid        types.UID
	endpointID string
}

type vmMetricsHistory struct {
	slices []metricsTimeSlice
}

type metricsTimeSlice struct {
	metrics   vmMetrics
	startTime time.Time
	endTime   time.Time
}

type vmMetrics struct {
	cpu uint16
}

const (
	MetricNameCPU string = "cpu"
	EndpointLabel string = "neon/endpoint-id"
)

func RunBillingMetricsColllector(backgroundCtx context.Context, conf *BillingConfig, store *util.WatchStore[vmapi.VirtualMachine]) error {
	client := billing.NewClient(conf.URL, http.DefaultClient)

	collectTicker := time.NewTicker(time.Second * time.Duration(conf.CollectEverySeconds))
	defer collectTicker.Stop()
	pushTicker := time.NewTicker(time.Second * time.Duration(conf.PushEverySeconds))
	defer pushTicker.Stop()

	state := billingMetricsState{
		historical: make(map[billingMetricsKey]vmMetricsHistory),
		present:    make(map[billingMetricsKey]vmMetrics),
		lastTime:   nil,
	}

	state.collect(conf, store)

	for {
		select {
		case <-collectTicker.C:
			state.collect(conf, store)
		case <-pushTicker.C:
			batch := state.drainIntoBatch(&client)
			if err := pushBillingEvents(conf, batch); err != nil {
				return fmt.Errorf("Error pushing billing events: %w", err)
			}
		case <-backgroundCtx.Done():
			// If we're being shut down, push the latests events we have before returning.
			batch := state.drainIntoBatch(&client)
			if err := pushBillingEvents(conf, batch); err != nil {
				return fmt.Errorf("Error pushing billing events: %w", err)
			}
			return nil
		}
	}
}

func (s *billingMetricsState) collect(conf *BillingConfig, store *util.WatchStore[vmapi.VirtualMachine]) {
	now := time.Now()

	old := s.present
	s.present = make(map[billingMetricsKey]vmMetrics)
	for _, vm := range store.Items() {
		endpointID, ok := vm.Labels[EndpointLabel]
		if !ok {
			continue
		}

		key := billingMetricsKey{
			uid:        vm.UID,
			endpointID: endpointID,
		}
		presentMetrics := vmMetrics{
			cpu: uint16(*vm.Spec.Guest.CPUs.Use),
		}
		if oldMetrics, ok := old[key]; ok {
			// The VM was present from s.lastTime to now. Add a time slice to its metrics history.
			timeSlice := metricsTimeSlice{
				metrics: vmMetrics{
					// strategically under-bill by assigning the minimum to the entire time slice.
					cpu: util.Min(oldMetrics.cpu, presentMetrics.cpu),
				},
				// note: we know s.lastTime != nil because otherwise old would be empty.
				startTime: *s.lastTime,
				endTime:   now,
			}

			vmHistory, ok := s.historical[key]
			if !ok {
				vmHistory = vmMetricsHistory{slices: nil}
			}
			vmHistory.slices = append(vmHistory.slices, timeSlice)
			s.historical[key] = vmHistory
		}

		s.present[key] = presentMetrics
	}

	s.lastTime = &now
}

func (s *billingMetricsState) drainIntoBatch(client *billing.Client) *billing.Batch {
	batch := client.NewBatch()

	for key, history := range s.historical {
		history.forEachMergedSlice(func(slice metricsTimeSlice) {
			batch.AddIncrementalEvent(billing.IncrementalEvent{
				IdempotencyKey: uuid.NewString(),
				MetricName:     MetricNameCPU,
				Type:           "", // set on JSON marshal
				EndpointID:     key.endpointID,
				StartTime:      slice.startTime,
				StopTime:       slice.endTime,
				Value:          int(slice.metrics.cpu),
			})
		})
	}

	s.historical = make(map[billingMetricsKey]vmMetricsHistory)
	return batch
}

func (h vmMetricsHistory) forEachMergedSlice(process func(metricsTimeSlice)) {
	if len(h.slices) == 0 {
		return
	}

	last := h.slices[0]
	for _, slice := range h.slices[1:] {
		if last.endTime == slice.startTime && last.metrics == slice.metrics {
			last.endTime = slice.endTime
		} else {
			process(last)
			last = slice
		}

	}

	process(last)
}

func pushBillingEvents(conf *BillingConfig, batch *billing.Batch) error {
	ctx, cancel := context.WithTimeout(context.TODO(), time.Second*time.Duration(conf.PushTimeoutSeconds))
	defer cancel()

	return batch.Send(ctx)
}
