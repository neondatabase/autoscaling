package agent

// Utilities for dumping internal state

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type StateDump struct {
	Stopped   bool           `json:"stopped"`
	BuildInfo util.BuildInfo `json:"buildInfo"`
	Pods      []podStateDump `json:"pods"`
}

func (s *agentState) StartDumpStateServer(shutdownCtx context.Context, config *DumpStateConfig) error {
	// Manually start the TCP listener so we can minimize errors in the background thread.
	addr := net.TCPAddr{IP: net.IPv4zero, Port: int(config.Port)}
	listener, err := net.ListenTCP("tcp", &addr)
	if err != nil {
		return fmt.Errorf("Error binding to %v", addr)
	}

	go func() {
		mux := http.NewServeMux()
		util.AddHandler("dump-state: ", mux, "/", http.MethodGet, "<empty>", func(ctx context.Context, body *struct{}) (*StateDump, int, error) {
			timeout := time.Duration(config.TimeoutSeconds) * time.Second

			startTime := time.Now()
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			state, err := s.DumpState(ctx, shutdownCtx.Err() != nil)
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
			klog.Errorf("dump-state server exited: %w", err)
		}
	}()

	return nil
}

func (s *agentState) DumpState(ctx context.Context, stopped bool) (*StateDump, error) {
	// Copy the high-level state, then process it
	podList, err := func() ([]*podState, error) {
		if err := s.lock.TryLock(ctx); err != nil {
			return nil, err
		}
		defer s.lock.Unlock()

		var list []*podState
		for name := range s.pods {
			list = append(list, s.pods[name])
		}
		return list, nil
	}()
	if err != nil {
		return nil, err
	}

	results := make(chan podStateDump)

	// TODO: We should have some concurrency limit on the number of active goroutines while fetching
	// state.
	for _, pod := range podList {
		// pass pod through explicitly, so we avoid variable reuse issues
		go func(p *podState) {
			results <- p.Dump(ctx)
		}(pod)
	}

	state := StateDump{
		Stopped:   stopped,
		BuildInfo: util.GetBuildInfo(),
		Pods:      nil,
	}

	cleanupCourtesy := time.NewTimer(0)
	cleanupCourtesy.Stop()

	for i := 0; i < len(podList); i += 1 {
		select {
		case <-ctx.Done():
			cleanupCourtesy.Reset(100 * time.Millisecond)
		case <-cleanupCourtesy.C:
			break
		case pod := <-results:
			state.Pods = append(state.Pods, pod)
		}
	}

	return &state, nil
}
