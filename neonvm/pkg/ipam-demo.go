package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap/zapcore"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/neondatabase/autoscaling/neonvm/pkg/ipam"
)

var (
	kconfig = flag.String("kube-config", "~/.kube/config", "Path to kuberenetes config. Only required if out-of-cluster.")
	nadName = flag.String("nad-name", "ipam-demo", "Network Attachment Definition name")
	nadNs   = flag.String("nad-namespace", "default", "Network Attachment Definition namespace")

	demoLoggerName = "ipam-demo"
	demoNamespace  = "default"
	demoCount      = 10
)

func main() {

	opts := zap.Options{
		Development:     true,
		StacktraceLevel: zapcore.Level(zapcore.PanicLevel),
		TimeEncoder:     zapcore.ISO8601TimeEncoder,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// define logger
	logger := zap.New(zap.UseFlagOptions(&opts)).WithName(demoLoggerName)

	// define klog settings (used in LeaderElector)
	klog.SetLogger(logger.V(2))

	// define context with logger
	ctx := log.IntoContext(context.Background(), logger)

	// resolve tilda in kubeconfig path
	kcfg := *kconfig
	if strings.HasPrefix(kcfg, "~/") {
		dirname, _ := os.UserHomeDir()
		kcfg = filepath.Join(dirname, kcfg[2:])
	}

	// Create IPAM object
	ipam, err := ipam.New(ctx, *nadName, *nadNs, kcfg)
	if err != nil {
		logger.Error(err, "failed to create IPAM")
		os.Exit(1)
	}
	defer ipam.Close()

	var wg sync.WaitGroup

	// acquire IPs in parallel
	for i := 1; i <= demoCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			startTime := time.Now()
			id := fmt.Sprintf("demo-ipam-%d", i)
			logger.Info("try to lease", "id", id)
			if ip, err := ipam.AcquireIP(ctx, id, demoNamespace); err != nil {
				logger.Error(err, "lease failed", "id", id)
			} else {
				logger.Info("acquired", "id", id, "ip", ip.String(), "acquired in", time.Now().Sub(startTime))
			}
		}(i)
		time.Sleep(time.Millisecond * 200)
	}
	wg.Wait()

	// release IPs in parallel
	for i := 1; i <= demoCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			startTime := time.Now()
			id := fmt.Sprintf("demo-ipam-%d", i)
			logger.Info("try to release", "id", id)
			if ip, err := ipam.ReleaseIP(ctx, id, demoNamespace); err != nil {
				logger.Error(err, "release failed", "id", id)
			} else {
				logger.Info("released", "id", id, "ip", ip.String(), "released in", time.Now().Sub(startTime))
			}
		}(i)
		time.Sleep(time.Millisecond * 200)
	}
	wg.Wait()

}
