package agent

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"
)

type MainRunner struct {
	EnvArgs    EnvArgs
	Config     *Config
	KubeClient *kubernetes.Clientset
	VMClient   *vmclient.Clientset
}

func (r MainRunner) Run() error {
	ctx := context.Background()

	podEvents := make(chan podEvent)

	klog.Info("Starting pod watcher")
	watchStore, err := startPodWatcher(ctx, r.Config, r.KubeClient, r.EnvArgs.K8sNodeName, podEvents)
	if err != nil {
		return fmt.Errorf("Error starting pod watcher: %w", err)
	}
	klog.Info("Pod watcher started, entering main loop")

	globalState := r.newAgentState()

	for {
		select {
		case <-ctx.Done():
			watchStore.Stop()
			// Remove anything else from podEvents
		loop:
			for {
				select {
				case <-podEvents:
				default:
					break loop
				}
			}

			globalState.Stop()
			return nil
		case event := <-podEvents:
			globalState.handleEvent(event)
		}
	}
}
