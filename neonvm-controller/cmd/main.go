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
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	certv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/go-logr/zapr"
	"github.com/tychoish/fun/srv"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/klog/v2"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/neonvm/controllers"
	"github.com/neondatabase/autoscaling/pkg/neonvm/ipam"
	"github.com/neondatabase/autoscaling/pkg/util"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(vmv1.AddToScheme(scheme))
	utilruntime.Must(certv1.AddToScheme(scheme))
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
	cli := getCli()

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
	cfg.QPS = float32(cli.k8sClient.qps)
	cfg.Burst = cli.k8sClient.burst
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: cli.metricsAddr,
		},
		HealthProbeBindAddress: cli.probeAddr,
		LeaderElection:         cli.leaderElection.enable,
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

		LeaseDuration: &cli.leaderElection.leaseDuration,
		RenewDeadline: &cli.leaderElection.renewDeadline,
		RetryPeriod:   &cli.leaderElection.retryPeriod,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		panic(err)
	}

	reconcilerMetrics := controllers.MakeReconcilerMetrics()

	rc := &controllers.ReconcilerConfig{
		DisableRunnerCgroup:     cli.disableRunnerCgroup,
		MaxConcurrentReconciles: cli.concurrencyLimit,
		SkipUpdateValidationFor: cli.skipUpdateValidationFor,
		QEMUDiskCacheSettings:   cli.qemuDiskCacheSettings,
		MemhpAutoMovableRatio:   cli.memhpAutoMovableRatio,
		FailurePendingPeriod:    cli.failurePendingPeriod,
		FailingRefreshInterval:  cli.failingRefreshInterval,
		AtMostOnePod:            cli.atMostOnePod,
		DefaultCPUScalingMode:   cli.defaultCpuScalingMode,
		NADConfig:               controllers.GetNADConfig(),
		UseVirtioConsole:        cli.useVirtioConsole,
	}

	ipam, err := ipam.New(ipam.IPAMParams{
		NadName:      rc.NADConfig.IPAMName,
		NadNamespace: rc.NADConfig.IPAMNamespace,

		// Let's not have more than a quarter of reconcilliation workers stuck
		// at IPAM mutex.
		ConcurrencyLimit: max(1, cli.concurrencyLimit/4),

		MetricsReg: metrics.Registry,
	})
	if err != nil {
		setupLog.Error(err, "unable to create ipam")
		panic(err)
	}
	defer ipam.Close()

	vmReconciler := &controllers.VMReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Config:  rc,
		Metrics: reconcilerMetrics,
		IPAM:    ipam,
	}
	vmReconcilerMetrics, err := vmReconciler.SetupWithManager(mgr)
	if err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VirtualMachine")
		panic(err)
	}
	vmWebhook := &controllers.VMWebhook{
		Recorder: mgr.GetEventRecorderFor("virtualmachine-webhook"),
		Config:   rc,
	}
	if err := vmWebhook.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "VirtualMachine")
		panic(err)
	}

	migrationReconciler := &controllers.VirtualMachineMigrationReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Config:  rc,
		Metrics: reconcilerMetrics,
	}
	migrationReconcilerMetrics, err := migrationReconciler.SetupWithManager(mgr)
	if err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VirtualMachineMigration")
		panic(err)
	}
	migrationWebhook := &controllers.VMMigrationWebhook{
		Recorder: mgr.GetEventRecorderFor("virtualmachinemigration-webhook"),
		Config:   rc,
	}
	if err := migrationWebhook.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "VirtualMachine")
		panic(err)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		panic(err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		panic(err)
	}

	dbgSrv := debugServerFunc(vmReconcilerMetrics, migrationReconcilerMetrics)
	if err := mgr.Add(dbgSrv); err != nil {
		setupLog.Error(err, "unable to set up debug server")
		panic(err)
	}

	if err := mgr.Add(vmReconcilerMetrics.FailingRefresher()); err != nil {
		setupLog.Error(err, "unable to set up failing refresher")
		panic(err)
	}

	// NOTE: THE CONTROLLER MUST IMMEDIATELY EXIT AFTER RUNNING THE MANAGER.
	if err := run(mgr); err != nil {
		setupLog.Error(err, "run manager error")
		panic(err)
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
