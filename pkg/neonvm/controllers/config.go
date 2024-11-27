package controllers

import (
	"time"

	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

// ReconcilerConfig stores shared configuration for VirtualMachineReconciler and
// VirtualMachineMigrationReconciler.
type ReconcilerConfig struct {
	// DisableRunnerCgroup, if true, disables running QEMU in a cgroup in new VM runner pods.
	// Fractional CPU scaling will continue to *pretend* to work, but it will not do anything in
	// practice.
	//
	// Under the hood, this results in passing -skip-cgroup-management and -enable-dummy-cpu-server
	// to neonvm-runner.
	DisableRunnerCgroup bool

	MaxConcurrentReconciles int

	// SkipUpdateValidationFor is the set of object names that we should ignore when doing webhook
	// update validation.
	SkipUpdateValidationFor map[types.NamespacedName]struct{}

	// QEMUDiskCacheSettings sets the values of the 'cache.*' settings used for QEMU disks.
	//
	// This field is passed to neonvm-runner as the `-qemu-disk-cache-settings` arg, and is directly
	// used in setting up the VM disks via QEMU's `-drive` flag.
	QEMUDiskCacheSettings string

	// DefaultMemoryProvider is the memory provider (dimm slots or virtio-mem) that will be used for
	// new VMs (or, when old ones restart) if nothing is explicitly set.
	DefaultMemoryProvider vmv1.MemoryProvider

	// MemhpAutoMovableRatio specifies the value that new neonvm-runners will set as the
	// kernel's 'memory_hotplug.auto_movable_ratio', iff the memory provider is virtio-mem.
	//
	// This value is passed directly to neonvm-runner as the '-memhp-auto-movable-ratio' flag.
	// We've confirmed sensible values are from 301 to 801 (i.e. 3.01:1 through 8.01:1).
	// The range of sensible values may extend further, but we have not tested that.
	MemhpAutoMovableRatio string

	// FailurePendingPeriod is the period for the propagation of
	// reconciliation failures to the observability instruments
	FailurePendingPeriod time.Duration

	// FailingRefreshInterval is the interval between consecutive
	// updates of metrics and logs, related to failing reconciliations
	FailingRefreshInterval time.Duration

	// AtMostOnePod is the flag that indicates whether we should only have one pod per VM.
	AtMostOnePod bool
	// DefaultCPUScalingMode is the default CPU scaling mode that will be used for VMs with empty spec.cpuScalingMode
	DefaultCPUScalingMode vmv1.CpuScalingMode
}
