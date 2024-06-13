//go:build nodelete

package buildtag

// NeverDeleteRunnerPods is enabled by the 'nodelete' build tag only in CI, and if enabled, causes
// the neonvm-controller to leave the original VM runner pods around when they otherwise would be
// deleted and recreated in order to (a) restart the VM or (b) finish a VM migration.
//
// Please note that this does not affect behavior when the VM/VMM object is deleted! When that
// happens, all pods owned by it will still be deleted!
//
// For more information and motivation for this flag, see <https://github.com/neondatabase/autoscaling/issues/634>.
const NeverDeleteRunnerPods = true
