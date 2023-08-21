package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"
	"golang.org/x/exp/slices"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type dumpStateConfig struct {
	Port           uint16 `json:"port"`
	TimeoutSeconds uint   `json:"timeoutSeconds"`
}

func (c *dumpStateConfig) validate() (string, error) {
	if c.Port == 0 {
		return "port", errors.New("value must be > 0")
	} else if c.TimeoutSeconds == 0 {
		return "timeoutSeconds", errors.New("value must be > 0")
	}

	return "", nil
}

type stateDump struct {
	Stopped   bool            `json:"stopped"`
	BuildInfo util.BuildInfo  `json:"buildInfo"`
	State     pluginStateDump `json:"state"`
}

func (p *AutoscaleEnforcer) startDumpStateServer(shutdownCtx context.Context, logger *zap.Logger) error {
	// Manually start the TCP listener so we can minimize errors in the background thread.
	addr := net.TCPAddr{IP: net.IPv4zero, Port: int(p.state.conf.DumpState.Port)}
	listener, err := net.ListenTCP("tcp", &addr)
	if err != nil {
		return fmt.Errorf("Error binding to %v", addr)
	}

	go func() {
		mux := http.NewServeMux()
		util.AddHandler(logger, mux, "/", http.MethodGet, "<empty>", func(ctx context.Context, _ *zap.Logger, body *struct{}) (*stateDump, int, error) {
			timeout := time.Duration(p.state.conf.DumpState.TimeoutSeconds) * time.Second

			startTime := time.Now()
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			state, err := p.dumpState(ctx, shutdownCtx.Err() != nil)
			if err != nil {
				if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
					totalDuration := time.Since(startTime)
					return nil, 500, fmt.Errorf("timed out after %s while getting state", totalDuration)
				} else {
					// some other type of cancel; 400 is a little weird, but there isn't a great
					// option here.
					return nil, 400, fmt.Errorf("error while getting state: %w", err)
				}
			}

			return state, 200, nil
		})
		// note: we don't shut down this server. It should be possible to continue fetching the
		// internal state after shutdown has started.
		server := &http.Server{Handler: mux}
		if err := server.Serve(listener); err != nil {
			logger.Error("dump-state server exited", zap.Error(err))
		}
	}()

	return nil
}

func (p *AutoscaleEnforcer) dumpState(ctx context.Context, stopped bool) (*stateDump, error) {
	state, err := p.state.dump(ctx)
	if err != nil {
		return nil, err
	}

	return &stateDump{
		Stopped:   stopped,
		BuildInfo: util.GetBuildInfo(),
		State:     *state,
	}, nil
}

type keyed[K any, V any] struct {
	Key   K `json:"key"`
	Value V `json:"value"`
}

type pluginStateDump struct {
	OngoingMigrationDeletions []keyed[util.NamespacedName, int] `json:"ongoingMigrationDeletions"`

	Nodes []keyed[string, nodeStateDump] `json:"nodes"`

	VMPods    []podNameAndPointer `json:"vmPods"`
	OtherPods []podNameAndPointer `json:"otherPods"`

	MaxTotalReservableCPU      vmapi.MilliCPU `json:"maxTotalReservableCPU"`
	MaxTotalReservableMemSlots uint16         `json:"maxTotalReservableMemSlots"`

	Conf Config `json:"config"`
}

type podNameAndPointer struct {
	Obj     pointerString       `json:"obj"`
	PodName util.NamespacedName `json:"podName"`
}

type pointerString string

type nodeStateDump struct {
	Obj              pointerString                                   `json:"obj"`
	Name             string                                          `json:"name"`
	NodeGroup        string                                          `json:"nodeGroup"`
	AvailabilityZone string                                          `json:"availabilityZone"`
	VCPU             nodeResourceState[vmapi.MilliCPU]               `json:"vCPU"`
	MemSlots         nodeResourceState[uint16]                       `json:"memSlots"`
	Pods             []keyed[util.NamespacedName, podStateDump]      `json:"pods"`
	OtherPods        []keyed[util.NamespacedName, otherPodStateDump] `json:"otherPods"`
	Mq               []*podNameAndPointer                            `json:"mq"`
}

type podStateDump struct {
	Obj                      pointerString                    `json:"obj"`
	Name                     util.NamespacedName              `json:"name"`
	VMName                   util.NamespacedName              `json:"vmName"`
	Node                     pointerString                    `json:"node"`
	TestingOnlyAlwaysMigrate bool                             `json:"testingOnlyAlwaysMigrate"`
	VCPU                     podResourceState[vmapi.MilliCPU] `json:"vCPU"`
	MemSlots                 podResourceState[uint16]         `json:"memSlots"`
	MostRecentComputeUnit    *api.Resources                   `json:"mostRecentComputeUnit"`
	Metrics                  *api.Metrics                     `json:"metrics"`
	MqIndex                  int                              `json:"mqIndex"`
	MigrationState           *podMigrationStateDump           `json:"migrationState"`
}

type podMigrationStateDump struct {
	MigrationName util.NamespacedName `json:"migrationName"`
}

type otherPodStateDump struct {
	Obj       pointerString         `json:"obj"`
	Node      pointerString         `json:"node"`
	Resources podOtherResourceState `json:"resources"`
}

func makePointerString[T any](t *T) pointerString {
	return pointerString(fmt.Sprintf("%p", t))
}

func sortSliceByPodName[T any](slice []T, name func(T) util.NamespacedName) {
	slices.SortFunc(slice, func(a, b T) (less bool) {
		aName := name(a)
		bName := name(b)
		return aName.Namespace < bName.Namespace && aName.Name < bName.Name
	})
}

func (s *pluginState) dump(ctx context.Context) (*pluginStateDump, error) {
	if err := s.lock.TryLock(ctx); err != nil {
		return nil, err
	}
	defer s.lock.Unlock()

	vmPods := make([]podNameAndPointer, 0, len(s.podMap))
	for _, p := range s.podMap {
		vmPods = append(vmPods, podNameAndPointer{Obj: makePointerString(p), PodName: p.name})
	}
	sortSliceByPodName(vmPods, func(p podNameAndPointer) util.NamespacedName { return p.PodName })

	otherPods := make([]podNameAndPointer, 0, len(s.otherPods))
	for _, p := range s.otherPods {
		otherPods = append(otherPods, podNameAndPointer{Obj: makePointerString(p), PodName: p.name})
	}
	sortSliceByPodName(otherPods, func(p podNameAndPointer) util.NamespacedName { return p.PodName })

	nodes := make([]keyed[string, nodeStateDump], 0, len(s.nodeMap))
	for k, n := range s.nodeMap {
		nodes = append(nodes, keyed[string, nodeStateDump]{Key: k, Value: n.dump()})
	}
	slices.SortFunc(nodes, func(kvx, kvy keyed[string, nodeStateDump]) (less bool) {
		return kvx.Key < kvy.Key
	})

	ongoingMigrationDeletions := make([]keyed[util.NamespacedName, int], 0, len(s.ongoingMigrationDeletions))
	for k, count := range s.ongoingMigrationDeletions {
		ongoingMigrationDeletions = append(ongoingMigrationDeletions, keyed[util.NamespacedName, int]{Key: k, Value: count})
	}
	sortSliceByPodName(ongoingMigrationDeletions, func(kv keyed[util.NamespacedName, int]) util.NamespacedName { return kv.Key })

	return &pluginStateDump{
		OngoingMigrationDeletions:  ongoingMigrationDeletions,
		Nodes:                      nodes,
		VMPods:                     vmPods,
		OtherPods:                  otherPods,
		MaxTotalReservableCPU:      s.maxTotalReservableCPU,
		MaxTotalReservableMemSlots: s.maxTotalReservableMemSlots,
		Conf:                       *s.conf,
	}, nil
}

func (s *nodeState) dump() nodeStateDump {
	pods := make([]keyed[util.NamespacedName, podStateDump], 0, len(s.pods))
	for k, p := range s.pods {
		pods = append(pods, keyed[util.NamespacedName, podStateDump]{Key: k, Value: p.dump()})
	}
	sortSliceByPodName(pods, func(kv keyed[util.NamespacedName, podStateDump]) util.NamespacedName { return kv.Key })

	otherPods := make([]keyed[util.NamespacedName, otherPodStateDump], 0, len(s.otherPods))
	for k, p := range s.otherPods {
		otherPods = append(otherPods, keyed[util.NamespacedName, otherPodStateDump]{Key: k, Value: p.dump()})
	}
	sortSliceByPodName(otherPods, func(kv keyed[util.NamespacedName, otherPodStateDump]) util.NamespacedName { return kv.Key })

	mq := make([]*podNameAndPointer, 0, len(s.mq))
	for _, p := range s.mq {
		if p == nil {
			mq = append(mq, nil)
		} else {
			v := podNameAndPointer{Obj: makePointerString(p), PodName: p.name}
			mq = append(mq, &v)
		}
	}

	return nodeStateDump{
		Obj:              makePointerString(s),
		Name:             s.name,
		NodeGroup:        s.nodeGroup,
		AvailabilityZone: s.availabilityZone,
		VCPU:             s.vCPU,
		MemSlots:         s.memSlots,
		Pods:             pods,
		OtherPods:        otherPods,
		Mq:               mq,
	}
}

func (s *podState) dump() podStateDump {
	// Copy some of the "may be nil" pointer fields
	var mostRecentComputeUnit *api.Resources
	if s.mostRecentComputeUnit != nil {
		mrcu := *s.mostRecentComputeUnit
		mostRecentComputeUnit = &mrcu
	}
	var metrics *api.Metrics
	if s.metrics != nil {
		m := *s.metrics
		metrics = &m
	}
	var migrationState *podMigrationStateDump
	if s.migrationState != nil {
		migrationState = &podMigrationStateDump{
			MigrationName: s.migrationState.name,
		}
	}

	return podStateDump{
		Obj:                      makePointerString(s),
		Name:                     s.name,
		VMName:                   s.vmName,
		Node:                     makePointerString(s.node),
		TestingOnlyAlwaysMigrate: s.testingOnlyAlwaysMigrate,
		VCPU:                     s.vCPU,
		MemSlots:                 s.memSlots,
		MostRecentComputeUnit:    mostRecentComputeUnit,
		Metrics:                  metrics,
		MqIndex:                  s.mqIndex,
		MigrationState:           migrationState,
	}
}

func (s *otherPodState) dump() otherPodStateDump {
	return otherPodStateDump{
		Obj:       makePointerString(s),
		Node:      makePointerString(s.node),
		Resources: s.resources,
	}
}
