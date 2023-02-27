package testutil

import (
	"context"
	"sync"

	"github.com/tychoish/fun/erc"

	"github.com/neondatabase/autoscaling/pkg/util"
)

func Watch[T any, P util.WatchObject[T]](
	ctx context.Context,
	stream <-chan util.ProcessEventArgs[T, P],
	handlers util.WatchHandlerFuncs[P],
) (*util.WatchStore[T], func() error) {
	store := util.NewWatchStore[T]()

	wg := &sync.WaitGroup{}
	ec := &erc.Collector{}

	wg.Add(1)

	go func() {
		defer wg.Done()
		defer func() { erc.Recover(ec) }()
		for {
			select {
			case <-ctx.Done():
				return
			case args, ok := <-stream:
				if !ok || ctx.Err() != nil {
					return
				}
				ec.Add(util.ProcessEvent[T, P](store, handlers, args))
			}
		}
	}()

	return store, func() error { wg.Wait(); return ec.Resolve() }
}
