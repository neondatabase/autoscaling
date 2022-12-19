package main

import (
	"context"

	"github.com/neondatabase/autoscaling/pkg/agent"

	"k8s.io/client-go/kubernetes"
	scheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"

	vmapi "github.com/neondatabase/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"
)

func main() {
	ctx := context.Background()
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
	vmapi.AddToScheme(scheme.Scheme)
	vmClient, err := vmclient.NewForConfig(kubeConfig)
	if err != nil {
		klog.Fatalf("Failed to make VM client: %s", err)
	}

	podArgs, err := agent.ArgsFromPod(ctx, kubeClient, envArgs)
	if err != nil {
		klog.Fatalf("Error getting args from pod: %s", err)
	}
	klog.Infof("Got pod args: %+v", podArgs)

	vmInfo, err := agent.ArgsFromVM(ctx, vmClient, envArgs, podArgs)
	if err != nil {
		klog.Fatalf("Error getting VM info: %s", err)
	}

	args := agent.Args{EnvArgs: envArgs, PodArgs: podArgs, VmInfo: vmInfo}

	runner, err := agent.NewRunner(args, kubeClient, vmClient)
	if err != nil {
		klog.Fatalf("Error while creating main loop: %s", err)
	}

	err = runner.MainLoop(config, ctx)
	if err != nil {
		klog.Fatalf("Main loop failed: %s", err)
	}
	klog.Info("Main loop returned without issue. Exiting.")
}

func makeKubeClientSet() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	return clientset, err
}
