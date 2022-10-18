package main

import (
	"context"
	"fmt"

	agent "github.com/neondatabase/autoscaling/agent/src"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"
)

func main() {
	ctx := context.Background()
	args, err := agent.ArgsFromEnv()
	if err != nil {
		klog.Fatalf("Error getting args from environment: %s", err)
	}

	config, err := agent.ReadConfig(args.ConfigPath)
	if err != nil {
		klog.Fatalf("Error reading config: %s", err)
	}

	klog.V(1).Infof("Got environment args: %+v", args)
	thisPodName := PodName{Name: args.K8sPodName, Namespace: args.K8sPodNamespace}

	kubeClient, err := makeKubeClientSet()
	if err != nil {
		klog.Fatalf("Failed to make K8S client: %s", err)
	}

	schedulerIP, err := thisPodName.getSchedulerPodIP(kubeClient, ctx)
	if err != nil {
		klog.Fatalf("Error getting self pod scheduler's IP: %s", err)
	}
	klog.Infof("Got scheduler IP address as %s", schedulerIP)

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

type PodName struct {
	Name      string
	Namespace string
}

func makeKubeClientSet() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	return clientset, err
}

func (n *PodName) getPod(kubeClient *kubernetes.Clientset, ctx context.Context) (*corev1.Pod, error) {
	thisPod, err := kubeClient.CoreV1().Pods(n.Namespace).Get(ctx, n.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("Error getting pod %s:%s: %s", n.Namespace, n.Name, err)
	}
	return thisPod, nil
}

func (n *PodName) getSchedulerPodIP(kubeClient *kubernetes.Clientset, ctx context.Context) (string, error) {
	pod, err := n.getPod(kubeClient, ctx)
	if err != nil {
		return "", err
	}

	if pod.Spec.SchedulerName == "" || pod.Spec.SchedulerName == "default-scheduler" {
		// default-scheduler is the name of the scheduler used for pods not assigned to a particular
		// scheduler.
		return "", fmt.Errorf("Pod is not using a custom scheduler")
	}

	// Assume that the scheduler's pod name matches the scheduler profile name.
	//
	// This isn't guaranteed, but doing it another way is annoying and practically they should have
	// the same name anyways.
	schedulerName := &PodName{
		Name:      pod.Spec.SchedulerName,
		Namespace: "kube-system",
	}

	klog.Infof("Searching for scheduler pod with same name as pod scheduler: %s", schedulerName.Name)
	selector := fmt.Sprintf("name=%s", schedulerName.Name)
	// selector := "component=scheduler"
	pods, err := kubeClient.CoreV1().Pods(schedulerName.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", fmt.Errorf(
			"Error getting scheduler pod candidates (label %q in namespace %s): %s",
			selector, schedulerName.Namespace, err,
		)
	} else if len(pods.Items) == 0 {
		return "", fmt.Errorf(
			"No pods matching scheduler selector (label %q in namespace %s)",
			selector, schedulerName.Namespace,
		)
	} else if len(pods.Items) > 1 {
		return "", fmt.Errorf(
			"Too many (> 1) pods matching scheduler name (label %q in namespace %s)",
			selector, schedulerName.Namespace,
		)
	}

	schedulerPod := &pods.Items[0]
	if schedulerPod.Status.PodIP == "" {
		return "", fmt.Errorf("Scheduler pod IP is not available (not yet allocated?)")
	}

	return schedulerPod.Status.PodIP, nil
}
