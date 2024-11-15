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
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/zapr"
	"github.com/tychoish/fun/srv"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/klog/v2"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/neonvm/controllers"
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
	var skipUpdateValidationFor map[types.NamespacedName]struct{}
	var disableRunnerCgroup bool
	var defaultCpuScalingMode vmv1.CpuScalingMode
	var qemuDiskCacheSettings string
	var defaultMemoryProvider vmv1.MemoryProvider
	var memhpAutoMovableRatio string
	var failurePendingPeriod time.Duration
	var failingRefreshInterval time.Duration
	var atMostOnePod bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.IntVar(&concurrencyLimit, "concurrency-limit", 1, "Maximum number of concurrent reconcile operations")
	flag.Func(
		"skip-update-validation-for",
		"Comma-separated list of object names to skip webhook validation, like 'foo' or 'default/bar'",
		func(value string) error {
			objSet := make(map[types.NamespacedName]struct{})

			if value != "" {
				for _, name := range strings.Split(value, ",") {
					if name == "" {
						return errors.New("name must not be empty")
					}

					var namespacedName types.NamespacedName
					splitBySlash := strings.SplitN(name, "/", 1)
					if len(splitBySlash) == 1 {
						namespacedName = types.NamespacedName{Namespace: "default", Name: splitBySlash[0]}
					} else {
						namespacedName = types.NamespacedName{Namespace: splitBySlash[0], Name: splitBySlash[1]}
					}
					objSet[namespacedName] = struct{}{}
				}
			}
			skipUpdateValidationFor = objSet
			return nil
		},
	)
	flag.Func("default-cpu-scaling-mode", "Set default cpu scaling mode to use for new VMs", defaultCpuScalingMode.FlagFunc)
	flag.BoolVar(&disableRunnerCgroup, "disable-runner-cgroup", false, "Disable creation of a cgroup in neonvm-runner for fractional CPU limiting")
	flag.StringVar(&qemuDiskCacheSettings, "qemu-disk-cache-settings", "cache=none", "Set neonvm-runner's QEMU disk cache settings")
	flag.Func("default-memory-provider", "Set default memory provider to use for new VMs", defaultMemoryProvider.FlagFunc)
	flag.StringVar(&memhpAutoMovableRatio, "memhp-auto-movable-ratio", "301", "For virtio-mem, set VM kernel's memory_hotplug.auto_movable_ratio")
	flag.DurationVar(&failurePendingPeriod, "failure-pending-period", 1*time.Minute,
		"the period for the propagation of reconciliation failures to the observability instruments")
	flag.DurationVar(&failingRefreshInterval, "failing-refresh-interval", 1*time.Minute,
		"the interval between consecutive updates of metrics and logs, related to failing reconciliations")
	flag.BoolVar(&atMostOnePod, "at-most-one-pod", false,
		"If true, the controller will ensure that at most one pod is running at a time. "+
			"Otherwise, the outdated pod might be left to terminate, while the new one is already running.")
	flag.Parse()

	if defaultMemoryProvider == "" {
		fmt.Fprintln(os.Stderr, "missing required flag '-default-memory-provider'")
		os.Exit(1)
	}

	logConfig := zap.NewProductionConfig()
	logConfig.Sampling = nil // Disabling sampling; it's enabled by default for zap's production configs.
	logConfig.Level.SetLevel(zap.InfoLevel)
	logConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logger := zapr.NewLogger(zap.Must(logConfig.Build(zap.AddStacktrace(zapcore.PanicLevel))))

	ctrl.SetLogger(logger)
	// define klog settings (used in LeaderElector)
	klog.SetLogger(logger.V(2))

	// tune k8s client for manager
	cfg := ctrl.GetConfigOrDie()
	cfg.QPS = 1000
	cfg.Burst = 2000
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "a3b22509.neon.tech",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// This option is only safe as long as the program immediately exits after the manager
		// stops.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	reconcilerMetrics := controllers.MakeReconcilerMetrics()

	rc := &controllers.ReconcilerConfig{
		DisableRunnerCgroup:     disableRunnerCgroup,
		MaxConcurrentReconciles: concurrencyLimit,
		SkipUpdateValidationFor: skipUpdateValidationFor,
		QEMUDiskCacheSettings:   qemuDiskCacheSettings,
		DefaultMemoryProvider:   defaultMemoryProvider,
		MemhpAutoMovableRatio:   memhpAutoMovableRatio,
		FailurePendingPeriod:    failurePendingPeriod,
		FailingRefreshInterval:  failingRefreshInterval,
		AtMostOnePod:            atMostOnePod,
		DefaultCPUScalingMode:   defaultCpuScalingMode,
	}

	vmReconciler := &controllers.VMReconciler{
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
	vmWebhook := &controllers.VMWebhook{
		Recorder: mgr.GetEventRecorderFor("virtualmachine-webhook"),
		Config:   rc,
	}
	if err := vmWebhook.SetupWithManager(mgr); err != nil {
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
	migrationWebhook := &controllers.VMMigrationWebhook{
		Recorder: mgr.GetEventRecorderFor("virtualmachinemigration-webhook"),
		Config:   rc,
	}
	if err := migrationWebhook.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "VirtualMachine")
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

	if err := mgr.Add(vmReconcilerMetrics.FailingRefresher()); err != nil {
		setupLog.Error(err, "unable to set up failing refresher")
		os.Exit(1)
	}

	// NOTE: THE CONTROLLER MUST IMMEDIATELY EXIT AFTER RUNNING THE MANAGER.
	if err := run(mgr); err != nil {
		setupLog.Error(err, "run manager error")
		os.Exit(1)
	}
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
