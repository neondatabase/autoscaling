package main

import (
	"errors"
	"flag"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type cliFlags struct {
	metricsAddr             string
	probeAddr               string
	leaderElection          leaderElectionCliFlags
	k8sClient               k8sClientCliFlags
	concurrencyLimit        int
	skipUpdateValidationFor map[types.NamespacedName]struct{}
	disableRunnerCgroup     bool
	defaultCpuScalingMode   vmv1.CpuScalingMode
	qemuDiskCacheSettings   string
	memhpAutoMovableRatio   string
	failurePendingPeriod    time.Duration
	failingRefreshInterval  time.Duration
	atMostOnePod            bool
	useVirtioConsole        bool
}

type leaderElectionCliFlags struct {
	enable        bool
	leaseDuration time.Duration
	renewDeadline time.Duration
	retryPeriod   time.Duration
}

type k8sClientCliFlags struct {
	qps   int
	burst int
}

func getCli() cliFlags {
	metricsAddr := flag.String("metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	probeAddr := flag.String("health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")

	// leader election: Comments and defaults taken from the manager.Options fields in
	// controller-runtime.
	enableLeaderElection := flag.Bool("leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	leaderElectionLeaseDuration := flag.Duration("leader-election-lease-duration", 15*time.Second,
		"The duration that non-leaders will wait to force acquire leadership.")
	leaderElectionRenewDeadline := flag.Duration("leader-election-renew-deadline", 10*time.Second,
		"The duration that the leader will retry refreshing leadership before giving up.")
	leaderElectionRetryPeriod := flag.Duration("leader-election-retry-period", 2*time.Second,
		"The duration leader election clients should wait between action attempts.")

	// k8s client rate limits: Defaults taken from our hard-coded limits as of 2025-05-15.
	k8sClientQPS := flag.Int("k8s-client-qps", 1000, "Client-side rate limit for k8s operations per second")
	k8sClientBurst := flag.Int("k8s-client-burst", 2000, "Maximum k8s operations per second that can be bursted to")

	concurrencyLimit := flag.Int("concurrency-limit", 1, "Maximum number of concurrent reconcile operations")
	skipUpdateValidationFor := namespacedNameSetFlag(
		"skip-update-validation-for",
		"Comma-separated list of object names to skip webhook validation, like 'foo' or 'default/bar'",
	)
	var defaultCpuScalingMode vmv1.CpuScalingMode
	flag.Func("default-cpu-scaling-mode", "Set default cpu scaling mode to use for new VMs", defaultCpuScalingMode.FlagFunc)
	disableRunnerCgroup := flag.Bool("disable-runner-cgroup", false, "Disable creation of a cgroup in neonvm-runner for fractional CPU limiting")
	qemuDiskCacheSettings := flag.String("qemu-disk-cache-settings", "cache=none", "Set neonvm-runner's QEMU disk cache settings")
	memhpAutoMovableRatio := flag.String("memhp-auto-movable-ratio", "301", "For virtio-mem, set VM kernel's memory_hotplug.auto_movable_ratio")
	failurePendingPeriod := flag.Duration("failure-pending-period", 1*time.Minute,
		"the period for the propagation of reconciliation failures to the observability instruments")
	failingRefreshInterval := flag.Duration("failing-refresh-interval", 1*time.Minute,
		"the interval between consecutive updates of metrics and logs, related to failing reconciliations")
	atMostOnePod := flag.Bool("at-most-one-pod", false,
		"If true, the controller will ensure that at most one pod is running at a time. "+
			"Otherwise, the outdated pod might be left to terminate, while the new one is already running.")
	useVirtioConsole := flag.Bool("use-virtio-console", false,
		"If true, the controller will set up the runner to use virtio console instead of serial console.")

	flag.Parse()

	return cliFlags{
		metricsAddr: *metricsAddr,
		probeAddr:   *probeAddr,
		leaderElection: leaderElectionCliFlags{
			enable:        *enableLeaderElection,
			leaseDuration: *leaderElectionLeaseDuration,
			renewDeadline: *leaderElectionRenewDeadline,
			retryPeriod:   *leaderElectionRetryPeriod,
		},
		k8sClient: k8sClientCliFlags{
			qps:   *k8sClientQPS,
			burst: *k8sClientBurst,
		},
		concurrencyLimit:        *concurrencyLimit,
		skipUpdateValidationFor: *skipUpdateValidationFor,
		disableRunnerCgroup:     *disableRunnerCgroup,
		defaultCpuScalingMode:   defaultCpuScalingMode,
		qemuDiskCacheSettings:   *qemuDiskCacheSettings,
		memhpAutoMovableRatio:   *memhpAutoMovableRatio,
		failurePendingPeriod:    *failurePendingPeriod,
		failingRefreshInterval:  *failingRefreshInterval,
		atMostOnePod:            *atMostOnePod,
		useVirtioConsole:        *useVirtioConsole,
	}
}

func namespacedNameSetFlag(name string, usage string) *map[types.NamespacedName]struct{} {
	set := new(map[types.NamespacedName]struct{})
	flag.Func(name, usage, func(value string) error {
		var err error
		*set, err = parseNamespacedNameSet(value)
		return err
	})
	return set
}

func parseNamespacedNameSet(value string) (map[types.NamespacedName]struct{}, error) {
	objSet := make(map[types.NamespacedName]struct{})

	if value != "" {
		for _, name := range strings.Split(value, ",") {
			if name == "" {
				return nil, errors.New("name must not be empty")
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
	return objSet, nil
}
