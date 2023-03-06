package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"k8s.io/klog/v2"

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

func (p *AutoscaleEnforcer) startDumpStateServer(shutdownCtx context.Context) error {
	// Manually start the TCP listener so we can minimize errors in the background thread.
	addr := net.TCPAddr{IP: net.IPv4zero, Port: int(p.state.conf.DumpState.Port)}
	listener, err := net.ListenTCP("tcp", &addr)
	if err != nil {
		return fmt.Errorf("Error binding to %v", addr)
	}

	go func() {
		mux := http.NewServeMux()
		util.AddHandler("dump-state: ", mux, "/", http.MethodGet, "<empty>", func(ctx context.Context, body *struct{}) (*stateDump, int, error) {
			timeout := time.Duration(p.state.conf.DumpState.TimeoutSeconds) * time.Second

			startTime := time.Now()
			ctx, cancel := context.WithDeadline(ctx, startTime.Add(timeout))
			defer cancel()

			state, err := p.dumpState(ctx, shutdownCtx.Err() != nil)
			if err != nil {
				totalDuration := time.Since(startTime)
				if totalDuration >= timeout {
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
			klog.Errorf("dump-state server exited: %w", err)
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
	Nodes []keyed[string, nodeStateDump] `json:"nodes"`

	VMPods    []podNameAndPointer `json:"vmPods"`
	OtherPods []podNameAndPointer `json:"otherPods"`

	MaxTotalReservableCPU      uint16 `json:"maxTotalReservableCPU"`
	MaxTotalReservableMemSlots uint16 `json:"maxTotalReservableMemSlots"`

	Conf config `json:"config"`
}

type podNameAndPointer struct {
	Obj     pointerString `json:"obj"`
	PodName api.PodName   `json:"podName"`
}

type pointerString string

type nodeStateDump struct {
	Obj       pointerString                           `json:"obj"`
	Name      string                                  `json:"name"`
	VCPU      nodeResourceState[uint16]               `json:"vCPU"`
	MemSlots  nodeResourceState[uint16]               `json:"memSlots"`
	Pods      []keyed[api.PodName, podStateDump]      `json:"pods"`
	OtherPods []keyed[api.PodName, otherPodStateDump] `json:"otherPods"`
	Mq        []*podNameAndPointer                    `json:"mq"`
}

type podStateDump struct {
	Obj                      pointerString            `json:"obj"`
	Name                     api.PodName              `json:"name"`
	VMName                   string                   `json:"vmName"`
	Node                     pointerString            `json:"node"`
	TestingOnlyAlwaysMigrate bool                     `json:"testingOnlyAlwaysMigrate"`
	VCPU                     podResourceState[uint16] `json:"vCPU"`
	MemSlots                 podResourceState[uint16] `json:"memSlots"`
	MostRecentComputeUnit    *api.Resources           `json:"mostRecentComputeUnit"`
	Metrics                  *api.Metrics             `json:"metrics"`
	MqIndex                  int                      `json:"mqIndex"`
	MigrationState           *podMigrationStateDump   `json:"migrationState"`
}

type podMigrationStateDump struct{}

type otherPodStateDump struct {
	Obj       pointerString         `json:"obj"`
	Node      pointerString         `json:"node"`
	Resources podOtherResourceState `json:"resources"`
}

func makePointerString[T any](t *T) pointerString {
	return pointerString(fmt.Sprintf("%p", t))
}

func (s *pluginState) dump(ctx context.Context) (*pluginStateDump, error) {
	if err := s.lock.TryLock(ctx); err != nil {
		return nil, err
	}
	defer s.lock.Unlock()

	var vmPods []podNameAndPointer
	for _, p := range s.podMap {
		vmPods = append(vmPods, podNameAndPointer{Obj: makePointerString(p), PodName: p.name})
	}

	var otherPods []podNameAndPointer
	for _, p := range s.otherPods {
		otherPods = append(otherPods, podNameAndPointer{Obj: makePointerString(p), PodName: p.name})
	}

	var nodes []keyed[string, nodeStateDump]
	for k, n := range s.nodeMap {
		nodes = append(nodes, keyed[string, nodeStateDump]{Key: k, Value: n.dump()})
	}

	return &pluginStateDump{
		Nodes:                      nodes,
		VMPods:                     vmPods,
		OtherPods:                  otherPods,
		MaxTotalReservableCPU:      s.maxTotalReservableCPU,
		MaxTotalReservableMemSlots: s.maxTotalReservableMemSlots,
		Conf:                       *s.conf,
	}, nil
}

func (s *nodeState) dump() nodeStateDump {

	var pods []keyed[api.PodName, podStateDump]
	for k, p := range s.pods {
		pods = append(pods, keyed[api.PodName, podStateDump]{Key: k, Value: p.dump()})
	}

	var otherPods []keyed[api.PodName, otherPodStateDump]
	for k, p := range s.otherPods {
		otherPods = append(otherPods, keyed[api.PodName, otherPodStateDump]{Key: k, Value: p.dump()})
	}

	var mq []*podNameAndPointer
	for _, p := range s.mq {
		if p == nil {
			mq = append(mq, nil)
		} else {
			v := podNameAndPointer{Obj: makePointerString(p), PodName: p.name}
			mq = append(mq, &v)
		}
	}

	return nodeStateDump{
		Obj:       makePointerString(s),
		Name:      s.name,
		VCPU:      s.vCPU,
		MemSlots:  s.memSlots,
		Pods:      pods,
		OtherPods: otherPods,
		Mq:        mq,
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
		migrationState = &podMigrationStateDump{}
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
