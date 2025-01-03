package plugin

// Handling of VirtualMachineMigration events.

import (
	"fmt"

	"go.uber.org/zap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/plugin/reconcile"
	"github.com/neondatabase/autoscaling/pkg/plugin/state"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// LabelPluginCreatedMigration marks all VirtualMachineMigrations that are created automatically by
// the scheduler plugin.
const LabelPluginCreatedMigration = "autoscaling.neon.tech/created-by-scheduler"

func (s *PluginState) HandleMigrationEvent(
	logger *zap.Logger,
	kind reconcile.EventKind,
	vmm *vmv1.VirtualMachineMigration,
) error {
	logger = logger.With(zap.Object("VirtualMachine", util.NamespacedName{
		Name:      vmm.Spec.VmName,
		Namespace: vmm.Namespace,
	}))

	switch kind {
	case reconcile.EventKindDeleted, reconcile.EventKindEphemeral:
		// Migration was deleted. Nothing to do.
		return nil
	case reconcile.EventKindAdded, reconcile.EventKindModified:
		return s.deleteMigrationIfNeeded(logger, vmm)
	default:
		panic("unreachable")
	}
}

func metadataForNewMigration(pod state.Pod) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		// NOTE: We derive the name of the migration from the name of the *pod* so that
		// we don't accidentally believe that there's already a migration ongoing for a
		// pod when it's actually a different pod of the same VM.
		Name:      fmt.Sprintf("schedplugin-%s", pod.Name),
		Namespace: pod.Namespace,
		Labels: map[string]string{
			LabelPluginCreatedMigration: "true",
		},
	}
}

// deleteMigrationIfNeeded deletes the migration object if it was created by the scheduler plugin
// and has reached a terminal state (succeeded or failed).
//
// This is basically for automatic clenaup of migrations once they're finished, otherwise we'd leak
// migration objects.
func (s *PluginState) deleteMigrationIfNeeded(logger *zap.Logger, vmm *vmv1.VirtualMachineMigration) error {
	// Check that the migration is owned by the scheduler plugin:
	if _, ok := vmm.Labels[LabelPluginCreatedMigration]; !ok {
		return nil
	}

	// Check if the migration is in a terminal state:
	switch vmm.Status.Phase {
	case vmv1.VmmSucceeded, vmv1.VmmFailed:
		// terminal state! it should be cleaned up.
	default:
		// non-terminal state, do nothing.
		return nil
	}

	// Check if the migration is already going to be deleted
	if vmm.DeletionTimestamp != nil {
		return nil
	}

	// Ok: we own this migration, it's done, and not yet being deleted. Let's delete it.
	if vmm.Status.Phase == vmv1.VmmFailed {
		logger.Warn("Deleting failed VirtualMachineMigration", zap.Any("VirtualMachineMigration", vmm))
	} else {
		logger.Info("Deleting successful VirtualMachineMigration", zap.Any("VirtualMachineMigration", vmm))
	}

	if err := s.deleteMigration(logger, vmm); err != nil {
		return fmt.Errorf("could not delete migration: %w", err)
	}

	return nil
}
