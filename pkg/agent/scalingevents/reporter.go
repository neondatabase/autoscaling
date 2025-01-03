package scalingevents

import (
	"context"
	"math"
	"time"

	"github.com/samber/lo"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/reporting"
)

type Config struct {
	// CUMultiplier sets the ratio between our internal compute unit and the one that should be
	// reported.
	//
	// This exists because Neon allows fractional compute units, while the autoscaler-agent acts on
	// integer multiples of a smaller compute unit.
	CUMultiplier float64 `json:"cuMultiplier"`

	// RereportThreshold sets the minimum amount of change in desired compute units required for us to
	// re-report the desired scaling.
	RereportThreshold float64 `json:"rereportThreshold"`

	// ClusterName is the name of the cluster that the reporting autoscaler-agent is in.
	ClusterName string `json:"clusterName"`
	// RegionName is the name of the region that the reporting autoscaler-agent is in.
	RegionName string `json:"regionName"`

	Clients ClientsConfig `json:"clients"`
}

type Reporter struct {
	conf    *Config
	sink    *reporting.EventSink[ScalingEvent]
	metrics PromMetrics
}

type ScalingEvent struct {
	Timestamp      time.Time         `json:"timestamp"`
	Region         string            `json:"region"`
	Cluster        string            `json:"cluster"`
	EndpointID     string            `json:"endpoint_id"`
	Type           scalingEventType  `json:"type"`
	CurrentMilliCU uint32            `json:"current_cu"`
	TargetMilliCU  uint32            `json:"target_cu"`
	GoalComponents *GoalCUComponents `json:"goalComponents,omitempty"`
}

type GoalCUComponents struct {
	CPU *float64 `json:"cpu,omitempty"`
	Mem *float64 `json:"mem,omitempty"`
	LFC *float64 `json:"lfc,omitempty"`
}

type scalingEventType string

const (
	scalingEventReal         = "real"
	scalingEventHypothetical = "hypothetical"
)

func NewReporter(
	ctx context.Context,
	parentLogger *zap.Logger,
	conf *Config,
	metrics PromMetrics,
) (*Reporter, error) {
	logger := parentLogger.Named("scalingevents")

	clients, err := createClients(ctx, logger, conf.Clients)
	if err != nil {
		return nil, err
	}

	sink := reporting.NewEventSink(logger, metrics.reporting, clients...)

	return &Reporter{
		conf:    conf,
		sink:    sink,
		metrics: metrics,
	}, nil
}

// Submit adds the ScalingEvent to the sender queue(s), returning without waiting for it to be sent.
func (r *Reporter) Submit(event ScalingEvent) {
	r.sink.Enqueue(event)
}

func convertToMilliCU(cu uint32, multiplier float64) uint32 {
	return uint32(math.Round(1000 * float64(cu) * multiplier))
}

// NewRealEvent is a helper function to create a ScalingEvent for actual scaling that has occurred.
//
// This method also handles compute unit translation.
func (r *Reporter) NewRealEvent(
	timestamp time.Time,
	endpointID string,
	currentCU uint32,
	targetCU uint32,
) ScalingEvent {
	return ScalingEvent{
		Timestamp:      timestamp,
		Region:         r.conf.RegionName,
		Cluster:        r.conf.ClusterName,
		EndpointID:     endpointID,
		Type:           scalingEventReal,
		CurrentMilliCU: convertToMilliCU(currentCU, r.conf.CUMultiplier),
		TargetMilliCU:  convertToMilliCU(targetCU, r.conf.CUMultiplier),
		GoalComponents: nil,
	}
}

func (r *Reporter) NewHypotheticalEvent(
	timestamp time.Time,
	endpointID string,
	currentCU uint32,
	targetCU uint32,
	goalCUs GoalCUComponents,
) ScalingEvent {
	convertFloat := func(cu *float64) *float64 {
		if cu != nil {
			return lo.ToPtr(*cu * r.conf.CUMultiplier)
		}
		return nil
	}

	return ScalingEvent{
		Timestamp:      timestamp,
		Region:         r.conf.RegionName,
		Cluster:        r.conf.ClusterName,
		EndpointID:     endpointID,
		Type:           scalingEventHypothetical,
		CurrentMilliCU: convertToMilliCU(currentCU, r.conf.CUMultiplier),
		TargetMilliCU:  convertToMilliCU(targetCU, r.conf.CUMultiplier),
		GoalComponents: &GoalCUComponents{
			CPU: convertFloat(goalCUs.CPU),
			Mem: convertFloat(goalCUs.Mem),
			LFC: convertFloat(goalCUs.LFC),
		},
	}
}
