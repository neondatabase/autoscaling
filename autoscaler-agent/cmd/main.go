package main

import (
	"context"
	"os/signal"
	"syscall"
	"time"

	"github.com/tychoish/fun/srv"
	"go.uber.org/zap"

	"k8s.io/client-go/kubernetes"
	scheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
	"github.com/neondatabase/autoscaling/pkg/agent"
	"github.com/neondatabase/autoscaling/pkg/util"
)

func main() {
	logConfig := zap.NewProductionConfig()
	logConfig.Sampling = nil                // Disable sampling, which the production config enables by default.
	logConfig.Level.SetLevel(zap.InfoLevel) // Only "info" level and above (i.e. not debug logs)
	logger := zap.Must(logConfig.Build()).Named("autoscaler-agent")
	defer logger.Sync() //nolint:errcheck // what are we gonna do, log something about it?

	envArgs, err := agent.ArgsFromEnv()
	if err != nil {
		logger.Panic("Failed to get args from environment", zap.Error(err))
	}
	logger.Info("Got environment args", zap.Any("args", envArgs))

	config, err := agent.ReadConfig(envArgs.ConfigPath)
	if err != nil {
		logger.Panic("Failed to read config", zap.Error(err))
	}
	logger.Info("Got config", zap.Any("config", config))

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		logger.Panic("Failed to get in-cluster K8s config", zap.Error(err))
	}
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		logger.Panic("Failed to make K8S client", zap.Error(err))
	}
	if err = vmv1.AddToScheme(scheme.Scheme); err != nil {
		logger.Panic("Failed to add NeonVM scheme", zap.Error(err))
	}

	vmClient, err := vmclient.NewForConfig(kubeConfig)
	if err != nil {
		logger.Panic("Failed to make VM client", zap.Error(err))
	}

	runner := agent.MainRunner{
		EnvArgs:    envArgs,
		Config:     config,
		KubeClient: kubeClient,
		VMClient:   vmClient,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	ctx = srv.SetShutdownSignal(ctx)
	ctx = srv.SetBaseContext(ctx)
	ctx = srv.WithOrchestrator(ctx)
	defer func() {
		cancel()
		if err := srv.GetOrchestrator(ctx).Wait(); err != nil {
			logger.Panic("Failed to shut down orchestrator", zap.Error(err))
		}

		logger.Info("Main loop returned without issue. Exiting.")
	}()

	if err := srv.GetOrchestrator(ctx).Add(srv.HTTP("agent-pprof", time.Second, util.MakePPROF("0.0.0.0:7777"))); err != nil {
		logger.Panic("Failed to add pprof service", zap.Error(err))
	}

	if err = runner.Run(logger, ctx); err != nil {
		logger.Panic("Main loop failed", zap.Error(err))
	}
}
