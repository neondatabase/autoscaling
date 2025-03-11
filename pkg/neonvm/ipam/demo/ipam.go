package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/neonvm/ipam"
)

var (
	nadName = flag.String("nad-name", "ipam-demo", "Network Attachment Definition name")
	nadNs   = flag.String("nad-namespace", "default", "Network Attachment Definition namespace")

	demoLoggerName = "ipam-demo"
	demoNamespace  = "default"
	demoCount      = 10
)

func main() {
	opts := zap.Options{ //nolint:exhaustruct // typical options struct; not all fields expected to be filled.
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

	// Create IPAM object
	ipam, err := ipam.New(ipam.IPAMParams{
		NadName:          *nadName,
		NadNamespace:     *nadNs,
		ConcurrencyLimit: 1,
		MetricsReg:       prometheus.NewRegistry(),
	})
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
			if ip, err := ipam.AcquireIP(ctx, types.NamespacedName{Name: id, Namespace: demoNamespace}); err != nil {
				logger.Error(err, "lease failed", "id", id)
			} else {
				logger.Info("acquired", "id", id, "ip", ip.String(), "acquired in", time.Since(startTime))
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
			if ip, err := ipam.ReleaseIP(ctx, types.NamespacedName{Name: id, Namespace: demoNamespace}); err != nil {
				logger.Error(err, "release failed", "id", id)
			} else {
				logger.Info("released", "id", id, "ip", ip.String(), "released in", time.Since(startTime))
			}
		}(i)
		time.Sleep(time.Millisecond * 200)
	}
	wg.Wait()
}
