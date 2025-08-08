package agent

// Utilities for dumping internal state

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type StateDump struct {
	Stopped bool           `json:"stopped"`
	Pods    []podStateDump `json:"pods"`
}

func (s *agentState) StartDumpStateServer(shutdownCtx context.Context, logger *zap.Logger, config *DumpStateConfig) error {
	// Manually start the TCP listener so we can minimize errors in the background thread.
	addr := net.TCPAddr{IP: net.IPv4zero, Port: int(config.Port)}
	listener, err := net.ListenTCP("tcp", &addr)
	if err != nil {
		return fmt.Errorf("error binding to %v", addr)
	}

	go func() {
		mux := http.NewServeMux()
		util.AddHandler(logger, mux, "/", http.MethodGet, "<empty>", func(ctx context.Context, logger *zap.Logger, body *struct{}) (*StateDump, int, error) {
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
			logger.Error("dump-state server exited", zap.Error(err))
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

		list := make([]*podState, 0, len(s.pods))
		for name := range s.pods {
			list = append(list, s.pods[name])
		}
		return list, nil
	}()
	if err != nil {
		return nil, err
	}

	state := StateDump{
		Stopped: stopped,
		Pods:    make([]podStateDump, len(podList)),
	}

	wg := sync.WaitGroup{}
	wg.Add(len(podList))
	concurrencyLimit := runtime.NumCPU()
	sema := make(chan struct{}, concurrencyLimit) // semaphore

	for i, pod := range podList {
		sema <- struct{}{} // enforce only 'concurrencyLimit' threads running at a time
		go func() {
			defer func() {
				<-sema
				wg.Done()
			}()

			state.Pods[i] = pod.dump(ctx)
		}()
	}

	// note: pod.Dump() respects the context, even with locking. When the context expires before we
	// acquire a lock, there's still valuable information to return - it's worthwhile to wait for
	// that to make it back to state.Pods when the context expires, instead of proactively aborting
	// in *this* thread.
	wg.Wait()

	// Sort the pods by name, so that we produce a deterministic ordering
	slices.SortFunc(state.Pods, func(a, b podStateDump) int {
		if n := strings.Compare(a.PodName.Namespace, b.PodName.Namespace); n != 0 {
			return n
		}

		return strings.Compare(a.PodName.Name, b.PodName.Name)
	})

	return &state, nil
}
