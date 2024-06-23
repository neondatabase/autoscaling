package controllers

import (
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

// ReconcilerConfig stores shared configuration for VirtualMachineReconciler and
// VirtualMachineMigrationReconciler.
type ReconcilerConfig struct {
	// IsK3s is true iff the cluster is running k3s nodes.
	//
	// This is required because - unlike the other most common kubernetes distributions - k3s
	// changes the location of the containerd socket.
	// There unfortunately does not appear to be a way to disable this behavior.
	IsK3s bool

	// UseContainerMgr, if true, enables using container-mgr for new VM runner pods.
	//
	// This is defined as a config option so we can do a gradual rollout of this change.
	UseContainerMgr bool

	MaxConcurrentReconciles int

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
}

func (c *ReconcilerConfig) criEndpointSocketPath() string {
	if c.IsK3s {
		return "/run/k3s/containerd/containerd.sock"
	} else {
		return "/run/containerd/containerd.sock"
	}
}
