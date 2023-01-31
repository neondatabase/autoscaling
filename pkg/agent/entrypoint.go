package agent

import (
	"context"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"
	"github.com/tychoish/fun/pubsub"

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

	broker := pubsub.NewBroker[watchEvent](ctx, pubsub.BrokerOptions{})
	schedulerStore, err := startSchedulerWatcher(ctx, RunnerLogger{"Scheduler Watcher: "}, r.KubeClient, broker, r.Config.Scheduler.SchedulerName)
	if err != nil {
		return fmt.Errorf("starting scheduler watch server: %w", err)
	}

	globalState := r.newAgentState(r.EnvArgs.K8sPodIP, broker, schedulerStore)

	for {
		select {
		case <-ctx.Done():
			watchStore.Stop()
			schedulerStore.Stop()
			broker.Stop()
			func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				broker.Wait(shutdownCtx)
			}()

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
