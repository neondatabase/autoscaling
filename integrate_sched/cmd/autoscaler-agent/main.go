package main

import (
	"context"
	"fmt"

	"github.com/neondatabase/autoscaling/pkg/agent"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"
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

	kubeClient, err := makeKubeClientSet()
	if err != nil {
		klog.Fatalf("Failed to make K8S client: %s", err)
	}

	podArgs, err := agent.ArgsFromPod(ctx, kubeClient, envArgs)
	if err != nil {
		klog.Fatalf("Error getting args from pod: %s", err)
	}
	klog.Infof("Got pod args: %+v", podArgs)

	schedulerIP, err := getSchedulerIp(ctx, kubeClient, podArgs.SchedulerName)
	if err != nil {
		klog.Fatalf("Error getting self pod scheduler's IP: %s", err)
	}
	klog.Infof("Got scheduler IP address: %s", schedulerIP)

	args := agent.Args{EnvArgs: envArgs, PodArgs: podArgs}

	cloudHypervisorSockPath := "/var/run/virtink/ch.sock"
	runner, err := agent.NewRunner(args, schedulerIP, cloudHypervisorSockPath)
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

func getSchedulerIp(ctx context.Context, kubeClient *kubernetes.Clientset, schedulerName string) (string, error) {
	schedulerNamespace := "kube-system"

	klog.Infof("Searching for scheduler pod with same name as pod scheduler: %s", schedulerName)
	selector := fmt.Sprintf("name=%s", schedulerName)
	pods, err := kubeClient.CoreV1().Pods(schedulerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", fmt.Errorf(
			"Error getting scheduler pod candidates (label %q in namespace %s): %s",
			selector, schedulerNamespace, err,
		)
	} else if len(pods.Items) == 0 {
		return "", fmt.Errorf(
			"No pods matching scheduler selector (label %q in namespace %s)",
			selector, schedulerNamespace,
		)
	} else if len(pods.Items) > 1 {
		return "", fmt.Errorf(
			"Too many (> 1) pods matching scheduler name (label %q in namespace %s)",
			selector, schedulerNamespace,
		)
	}

	schedulerPod := &pods.Items[0]
	if schedulerPod.Status.PodIP == "" {
		return "", fmt.Errorf("Scheduler pod IP is not available (not yet allocated?)")
	}

	return schedulerPod.Status.PodIP, nil
}
