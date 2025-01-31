package plugin

// Decision-making for live migrations.

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/slices"

	"k8s.io/apimachinery/pkg/types"

	"github.com/neondatabase/autoscaling/pkg/plugin/state"
)

// triggerMigrationsIfNecessary uses the state of the temporary node to request any migrations that
// may be ncessary to reduce the reserved resources below the watermark.
func triggerMigrationsIfNecessary(
	logger *zap.Logger,
	originalNode *state.Node,
	tmpNode *state.Node,
	requestedMigrations []types.UID,
	requestMigrationAndRequeue func(podUID types.UID) error,
) error {
	// To get an accurate count of the amount that's migrating, mark all the pods in
	// requestedMigrations as if they're already migrating.
	// They technically might not be! So we do this via Speculatively() in order to use the
	// existing node methods without actually committing these changes.
	for _, uid := range requestedMigrations {
		p, ok := tmpNode.GetPod(uid)
		if !ok {
			logger.Warn(
				"Node state marked pod as migrating that doesn't exist locally",
				zap.Object("Pod", zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
					enc.AddString("UID", string(uid))
					return nil
				})),
			)
			continue
		}

		// Mark the pod as migrating and update it in the (speculative) node
		newPod := p
		newPod.Migrating = true
		tmpNode.UpdatePod(p, newPod)
	}

	cpuAbove := tmpNode.CPU.UnmigratedAboveWatermark()
	memAbove := tmpNode.Mem.UnmigratedAboveWatermark()
	// if we're below the watermark (or already migrating enough to be below the watermark),
	// there's nothing to do:
	if cpuAbove == 0 && memAbove == 0 {
		return nil
	}

	logger.Info(
		"Not enough resources are being migrated to reduce to the watermark. Finding migration targets",
		zap.Object("Node", originalNode),
		zap.String("CPUToMigrate", fmt.Sprint(cpuAbove)),
		zap.String("MemToMigrate", fmt.Sprint(memAbove)),
	)

	var candidates []state.Pod
	for _, pod := range tmpNode.MigratablePods() {
		// Maybe this pod is currently being migrated. If so, don't include it on the list of
		// new candidates:
		if pod.Migrating {
			continue
		}

		candidates = append(candidates, pod)
	}

	// Ok, we have some migration candidates. Let's sort them and keep triggering migrations
	// until it'll be enough to get below the watermark.
	slices.SortFunc(candidates, func(cx, cy state.Pod) bool {
		return cx.BetterMigrationTargetThan(cy)
	})
	for _, pod := range candidates {
		podLogger := logger.With(zap.Any("CandidatePod", pod))

		// If we find a pod that is singularly above the watermark, don't migrate it! We'll
		// likely just end up above the watermark on the new node.
		// NOTE that this is NOT true in heterogeneous clusters (i.e., where the nodes are
		// different sizes), but scheduling is much more complex there, and we'd have to be
		// careful to only migrate when there's candidate nodes *with room* where the VM could
		// fit without going over the watermark.
		//
		// That's all quite complicated -- hence why we're taking the easy way out.
		tooBig := pod.CPU.Reserved > tmpNode.CPU.Watermark || pod.Mem.Reserved > tmpNode.Mem.Watermark
		if tooBig {
			podLogger.Warn("Skipping potential migration of candidate Pod because it's too big")
			continue
		}

		// Trigger migration of this pod!
		podLogger.Info("Internally triggering migration for candidate Pod")
		if err := requestMigrationAndRequeue(pod.UID); err != nil {
			podLogger.Error("Failed to requeue reconciling of candidate Pod")
			return fmt.Errorf("could not requeue pod %v with UID %s: %w", pod.NamespacedName, pod.UID, err)
		}
		// update the pod state in the speculative node
		newPod := pod
		newPod.Migrating = true
		tmpNode.UpdatePod(pod, newPod)

		// ... and then check if we need to keep migrating more ...
		cpuAbove = tmpNode.CPU.UnmigratedAboveWatermark()
		memAbove = tmpNode.Mem.UnmigratedAboveWatermark()

		if cpuAbove <= 0 && memAbove <= 0 {
			// We've triggered enough migrations that it should get us below the watermark.
			// We're done for now.
			break
		}
	}

	if cpuAbove > 0 || memAbove > 0 {
		logger.Warn(
			"Could not trigger enough migrations to get below watermark",
			zap.Object("SpeculativeNode", tmpNode),
		)
	}

	return nil
}
