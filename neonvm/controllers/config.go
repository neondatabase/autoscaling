package controllers

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
}

func (c *ReconcilerConfig) criEndpointSocketPath() string {
	if c.IsK3s {
		return "/run/k3s/containerd/containerd.sock"
	} else {
		return "/run/containerd/containerd.sock"
	}
}
