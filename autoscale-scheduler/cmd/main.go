package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/tychoish/fun/srv"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zapio"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/neondatabase/autoscaling/pkg/plugin"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// all of the juicy bits are defined in pkg/plugin/

func main() {
	logConfig := zap.NewProductionConfig()
	logConfig.Sampling = nil // Disable sampling, which the production config enables by default.
	logConfig.DisableStacktrace = true
	logger := zap.Must(logConfig.Build()).Named("autoscale-scheduler")

	if err := runProgram(logger); err != nil {
		log.Fatal(err)
	}
}

// runProgram is the "real" main, but returning an error means that
// the shutdown handling code doesn't have to call os.Exit, even indirectly.
func runProgram(logger *zap.Logger) (err error) {
	conf, err := plugin.ReadConfig(plugin.DefaultConfigPath)
	if err != nil {
		return fmt.Errorf("Error reading config at %q: %w", plugin.DefaultConfigPath, err)
	}

	// this: listens for sigterm, when we catch that signal, the
	// context gets canceled, a go routine waits for half a second, and
	// then closes the signal channel, which we block on in a
	// defer. because defers execute in LIFO errors, this just
	// pauses for a *very* short period of time before exiting.
	//
	// eventually, the constructed application will track it's
	// services and be able to more coherently wait for shutdown
	// without needing a sleep.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	ctx = srv.SetShutdownSignal(ctx)
	ctx = srv.WithOrchestrator(ctx)
	ctx = srv.SetBaseContext(ctx)
	orca := srv.GetOrchestrator(ctx)
	defer func() { err = orca.Service().Wait() }()

	if err := orca.Add(srv.HTTP("scheduler-pprof", time.Second, util.MakePPROF("0.0.0.0:7777"))); err != nil {
		return err
	}

	// The normal scheduler outputs to klog, and there isn't *really* a way to stop that. So to make
	// everything fit nicely, we'll redirect it to zap as well.
	redirectKlog(logger.Named("klog"))

	constructor := func(_ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
		return plugin.NewAutoscaleEnforcerPlugin(ctx, logger, h, conf)
	}

	command := app.NewSchedulerCommand(app.WithPlugin(plugin.PluginName, constructor))
	// Don't output the full usage whenever any error occurs (otherwise, startup errors get drowned
	// out by many pages of scheduler command flags)
	command.SilenceUsage = true

	if err := command.ExecuteContext(ctx); err != nil {
		return err
	}
	return
}

func redirectKlog(to *zap.Logger) {
	severityPairs := []struct {
		klogLevel string
		zapLevel  zapcore.Level
	}{
		{"info", zapcore.InfoLevel},
		{"warning", zapcore.WarnLevel},
		{"error", zapcore.ErrorLevel},
		{"fatal", zapcore.FatalLevel},
	}

	for _, pair := range severityPairs {
		klog.SetOutputBySeverity(pair.klogLevel, &zapio.Writer{
			Log:   to,
			Level: pair.zapLevel,
		})
	}

	// By default, we'll get LogToStderr(true), which completely bypasses any redirecting with
	// SetOutput or SetOutputBySeverity. So... we'd like to avoid that, which thankfully we can do.
	klog.LogToStderr(false)
}
