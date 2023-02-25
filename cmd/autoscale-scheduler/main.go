package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/tychoish/fun/srv"

	"k8s.io/kubernetes/cmd/kube-scheduler/app"

	"github.com/neondatabase/autoscaling/pkg/plugin"
)

// all of the juicy bits are defined in pkg/plugin/

func main() {
	if err := runProgram(); err != nil {
		log.Fatal(err)
	}
}

// runProgram is the "real" main, but returning an error means that
// the shutdown handling code doesn't have to call os.Exit, even indirectly.
func runProgram() (err error) {
	// this: listens for sigterm, when we catch that signal, the
	// context gets canceled, a go routine waits for half a second, and
	// then closes the signal channel, which we block on in a
	// defer. because defers execute in LIFO errors, this just
	// pauses for a *very* short period of time before exiting.
	//
	// eventually, the constructed application will track it's
	// services and be able to more coherently wait for shutdown
	// without needing a sleep.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	ctx = srv.SetShutdown(ctx)
	ctx = srv.WithOrchestrator(ctx)
	ctx = srv.SetBaseContext(ctx)
	orca := srv.GetOrchestrator(ctx)
	defer func() { err = orca.Service().Wait() }()

	command := app.NewSchedulerCommand(app.WithPlugin(plugin.Name, plugin.NewAutoscaleEnforcerPlugin(ctx)))
	if err := command.ExecuteContext(ctx); err != nil {
		return err
	}
	return
}
