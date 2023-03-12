package main

import (
	"k8s.io/client-go/kubernetes"
	scheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"

	vmapi "github.com/neondatabase/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/agent"
	"github.com/neondatabase/autoscaling/pkg/task"
)

func main() {
	envArgs, err := agent.ArgsFromEnv()
	if err != nil {
		klog.Fatalf("Error getting args from environment: %s", err)
	}

	config, err := agent.ReadConfig(envArgs.ConfigPath)
	if err != nil {
		klog.Fatalf("Error reading config: %s", err)
	}
	klog.Infof("Got environment args: %+v", envArgs)

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Error getting in-cluster K8S config: %s", err)
	}
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		klog.Fatalf("Failed to make K8S client: %s", err)
	}
	if err = vmapi.AddToScheme(scheme.Scheme); err != nil {
		klog.Fatalf("Failed to add NeonVM scheme: %s", err)
	}

	vmClient, err := vmclient.NewForConfig(kubeConfig)
	if err != nil {
		klog.Fatalf("Failed to make VM client: %s", err)
	}

	runner := agent.MainRunner{
		EnvArgs:    envArgs,
		Config:     config,
		KubeClient: kubeClient,
		VMClient:   vmClient,
	}

	tm := task.NewRootTaskManager("autoscaler-agent")
	errHandler := task.LogFatalError("Error during shutdown: %w")

	tm = tm.WithShutdownErrorHandler(errHandler)
	tm = tm.WithPanicHandler(task.LogPanicAndShutdown(tm, agent.MakeShutdownContext))
	tm.ShutdownOnSigterm(agent.MakeShutdownContext)
	defer func() {
		ctx, cancel := agent.MakeShutdownContext()
		defer cancel()
		if err := tm.Shutdown(ctx); err != nil {
			errHandler(err)
		}
	}()

	if err = runner.Run(tm); err != nil {
		klog.Fatalf("Main loop failed: %s", err)
	}
	klog.Info("Main loop returned without issue. Exiting.")
}
