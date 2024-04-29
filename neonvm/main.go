/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tychoish/fun/srv"
	"go.uber.org/zap/zapcore"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/neonvm/controllers"
	"github.com/neondatabase/autoscaling/pkg/util"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(vmv1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func run(mgr manager.Manager) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx = srv.SetShutdownSignal(ctx)
	ctx = srv.SetBaseContext(ctx)
	ctx = srv.WithOrchestrator(ctx)
	orca := srv.GetOrchestrator(ctx)
	defer func() {
		if err := orca.Wait(); err != nil {
			setupLog.Error(err, "failed to shut down orchestrator")
		}

		setupLog.Info("main loop returned, exiting")
	}()

	if err := orca.Add(srv.HTTP("pprof", time.Second, util.MakePPROF("0.0.0.0:7777"))); err != nil {
		return fmt.Errorf("failed to add pprof service: %w", err)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("problem running manager: %w", err)
	}

	return nil
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var concurrencyLimit int
	var enableContainerMgr bool
	var qemuDiskCacheSettings string
	var runnerRequestTimeout time.Duration
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.IntVar(&concurrencyLimit, "concurrency-limit", 1, "Maximum number of concurrent reconcile operations")
	flag.BoolVar(&enableContainerMgr, "enable-container-mgr", false, "Enable crictl-based container-mgr alongside each VM")
	flag.StringVar(&qemuDiskCacheSettings, "qemu-disk-cache-settings", "cache=none", "Set neonvm-runner's QEMU disk cache settings")
	flag.DurationVar(&runnerRequestTimeout, "runner-request-timeout", time.Second, "Set timeout for HTTP requests to neonvm-runner")
	opts := zap.Options{ //nolint:exhaustruct // typical options struct; not all fields needed.
		Development:     true,
		StacktraceLevel: zapcore.Level(zapcore.PanicLevel),
		TimeEncoder:     zapcore.ISO8601TimeEncoder,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	// define klog settings (used in LeaderElector)
	klog.SetLogger(zap.New(zap.UseFlagOptions(&opts)).V(2))

	// tune k8s client for manager
	cfg := ctrl.GetConfigOrDie()
	cfg.QPS = 1000
	cfg.Burst = 2000

	// fetch node info to determine if we're running in k3s
	isK3s, err := checkIfRunningInK3sCluster(cfg)
	if err != nil {
		setupLog.Error(err, "unable to check if running in k3s")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "a3b22509.neon.tech",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	reconcilerMetrics := controllers.MakeReconcilerMetrics()

	rc := &controllers.ReconcilerConfig{
		IsK3s:                   isK3s,
		UseContainerMgr:         enableContainerMgr,
		MaxConcurrentReconciles: concurrencyLimit,
		QEMUDiskCacheSettings:   qemuDiskCacheSettings,
		RunnerRequestTimeout:    runnerRequestTimeout,
	}

	vmReconciler := &controllers.VirtualMachineReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("virtualmachine-controller"),
		Config:   rc,
		Metrics:  reconcilerMetrics,
	}
	vmReconcilerMetrics, err := vmReconciler.SetupWithManager(mgr)
	if err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VirtualMachine")
		os.Exit(1)
	}
	if err = (&vmv1.VirtualMachine{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "VirtualMachine")
		os.Exit(1)
	}

	migrationReconciler := &controllers.VirtualMachineMigrationReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("virtualmachinemigration-controller"),
		Config:   rc,
		Metrics:  reconcilerMetrics,
	}
	migrationReconcilerMetrics, err := migrationReconciler.SetupWithManager(mgr)
	if err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VirtualMachineMigration")
		os.Exit(1)
	}
	if err = (&vmv1.VirtualMachineMigration{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "VirtualMachineMigration")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	dbgSrv := debugServerFunc(vmReconcilerMetrics, migrationReconcilerMetrics)
	if err := mgr.Add(dbgSrv); err != nil {
		setupLog.Error(err, "unable to set up debug server")
		os.Exit(1)
	}

	if err := run(mgr); err != nil {
		setupLog.Error(err, "run manager error")
		os.Exit(1)
	}
}

func checkIfRunningInK3sCluster(cfg *rest.Config) (bool, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return false, fmt.Errorf("failed to create new k8s client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.TODO(), 10*time.Second)
	defer cancel()

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}

	for _, node := range nodes.Items {
		if node.Status.NodeInfo.OSImage == "K3s dev" {
			return true, nil
		}
	}
	return false, nil
}

func debugServerFunc(reconcilers ...controllers.ReconcilerWithMetrics) manager.RunnableFunc {
	return manager.RunnableFunc(func(ctx context.Context) error {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			defer r.Body.Close()

			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = w.Write([]byte(fmt.Sprintf("request method must be %s", http.MethodGet)))
				return
			}

			response := make([]controllers.ReconcileSnapshot, 0, len(reconcilers))
			for _, r := range reconcilers {
				response = append(response, r.Snapshot())
			}

			responseBody, err := json.Marshal(&response)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(fmt.Sprintf("failed to marshal JSON response: %s", err)))
				return
			}

			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(responseBody)
		})

		server := &http.Server{
			Addr:    "0.0.0.0:7778",
			Handler: mux,
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		go func() {
			<-ctx.Done()
			_ = server.Shutdown(context.TODO())
		}()

		return server.ListenAndServe()
	})
}
