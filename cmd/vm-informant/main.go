package main

import (
	"context"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/tychoish/fun/srv"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/informant"
	"github.com/neondatabase/autoscaling/pkg/util"
)

func main() {
	logConfig := zap.NewProductionConfig()
	logConfig.Sampling = nil // Disable sampling, which the production config enables by default.
	logger := zap.Must(logConfig.Build()).Named("vm-informant")
	defer logger.Sync() //nolint:errcheck // what are we gonna do, log something about it?

	logger.Info("", zap.Any("buildInfo", util.GetBuildInfo()))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	ctx = srv.SetShutdownSignal(ctx) // allows workers to cause a shutdown
	ctx = srv.WithOrchestrator(ctx)  // creates and starts an orchestrator
	ctx = srv.SetBaseContext(ctx)    // sets a context for starting async work in request scopes

	orca := srv.GetOrchestrator(ctx)

	defer func() {
		if err := orca.Service().Wait(); err != nil {
			logger.Panic("Failed to shut down service", zap.Error(err))
		}
	}()

	state, err := informant.NewState(logger)
	if err != nil {
		logger.Fatal("Error creating informant state", zap.Error(err))
	}
	logger.Info("Created informant state.", zap.Any("state", state))

	mux := http.NewServeMux()
	hl := logger.Named("handle")
	util.AddHandler(hl, mux, "/register", http.MethodPost, "AgentDesc", state.RegisterAgent)
	util.AddHandler(hl, mux, "/health-check", http.MethodPut, "AgentIdentification", state.HealthCheck)
	util.AddHandler(hl, mux, "/downscale", http.MethodPut, "AgentResourceMessage", state.TryDownscale)
	util.AddHandler(hl, mux, "/upscale", http.MethodPut, "AgentResourceMessage", state.NotifyUpscale)
	util.AddHandler(hl, mux, "/unregister", http.MethodDelete, "AgentDesc", state.UnregisterAgent)

	addr := "0.0.0.0:10301"
	hl.Info("Starting server", zap.String("addr", addr))

	// we create an http service and add it to the orchestrator,
	// which will start it and manage its lifecycle.
	if err := orca.Add(srv.HTTP("vm-informant-api", 5*time.Second, &http.Server{Addr: addr, Handler: mux})); err != nil {
		logger.Fatal("Failed to add API server", zap.Error(err))
	}

	// we drop to the defers now, which will block until the signal
	// handler is called.
}
