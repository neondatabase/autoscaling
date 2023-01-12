package agent

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/util"
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

	buildInfo := util.GetBuildInfo()
	klog.Infof("buildInfo.GitInfo:   %s", buildInfo.GitInfo)
	klog.Infof("buildInfo.NeonVM:    %s", buildInfo.NeonVM)
	klog.Infof("buildInfo.GoVersion: %s", buildInfo.GoVersion)

	klog.Info("Starting pod watcher")

	watchStore, err := startPodWatcher(ctx, r.Config, r.KubeClient, r.EnvArgs.K8sNodeName, podEvents)
	if err != nil {
		return fmt.Errorf("Error starting pod watcher: %w", err)
	}
	klog.Info("Pod watcher started, entering main loop")

	logger := RunnerLogger{prefix: "MainRunner: "}
	schedulerWatch, err := watchSchedulerUpdates(ctx, logger, r.KubeClient, r.Config.Scheduler.SchedulerName)
	if err != nil {
		return fmt.Errorf("Error starting scheduler watcher: %w", err)
	}

	globalState := r.newAgentState(schedulerWatch, r.EnvArgs.K8sPodIP)
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
