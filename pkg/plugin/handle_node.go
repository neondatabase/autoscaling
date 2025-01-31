package plugin

// Handling of Node events.

import (
	"fmt"
	"time"

	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/neondatabase/autoscaling/pkg/plugin/reconcile"
	"github.com/neondatabase/autoscaling/pkg/plugin/state"
)

func (s *PluginState) HandleNodeEvent(logger *zap.Logger, kind reconcile.EventKind, node *corev1.Node) error {
	expectExists := kind == reconcile.EventKindModified || kind == reconcile.EventKindDeleted

	switch kind {
	case reconcile.EventKindAdded, reconcile.EventKindModified:
		return s.updateNode(logger, node, expectExists)
	case reconcile.EventKindDeleted, reconcile.EventKindEphemeral:
		return s.deleteNode(logger, node, expectExists)
	default:
		panic("unreachable")
	}
}

func (s *PluginState) updateNode(logger *zap.Logger, node *corev1.Node, expectExists bool) error {
	newNode, err := state.NodeStateFromK8sObj(node, s.config.Watermark, s.metrics.Nodes.InheritedLabels)
	if err != nil {
		return fmt.Errorf("could not get state from Node object: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var updated *nodeState

	oldNS, ok := s.nodes[node.Name]
	if !ok {
		if expectExists {
			logger.Warn("Adding node that unexpectedly doesn't exist in local state")
		}

		s.maxNodeCPU = max(s.maxNodeCPU, newNode.CPU.Total)
		s.maxNodeMem = max(s.maxNodeMem, newNode.Mem.Total)

		entry := &nodeState{
			node:                newNode,
			requestedMigrations: make(map[types.UID]struct{}),
			podsVMPatchedAt:     make(map[types.UID]time.Time),
		}

		logger.Info("Adding base node state", zap.Object("Node", entry.node))
		s.nodes[node.Name] = entry
		updated = entry
	} else /* oldNode DOES exist, let's update it */ {
		if !expectExists {
			logger.Warn("Updating node that unexpectedly exists in local state")
		}

		// Use (*Node).Speculatively() so that we can log both states before committing, and provide
		// protection from panics if .Update() has issues.
		oldNS.node.Speculatively(func(n *state.Node) (commit bool) {
			changed := n.Update(newNode)
			if changed {
				logger.Warn("Updating base node state", zap.Object("OldNode", oldNS.node), zap.Object("Node", n))
			}
			return true // yes, apply the change
		})
		updated = oldNS
	}

	return s.reconcileNode(logger, updated)
}

func (s *PluginState) deleteNode(logger *zap.Logger, node *corev1.Node, expectExists bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, exists := s.nodes[node.Name]
	if exists && !expectExists {
		logger.Warn("Deleting node that unexpectedly exists in local state")
	} else if !exists && expectExists {
		logger.Warn("No-op deleting node that unexpectedly doesn't exist in local state")
	}

	if exists {
		s.cleanupNode(logger, n)
	}

	return nil
}

// reconcileNode makes any updates necessary given the current state of the node.
// In particular, this method:
//
// 1. Triggers live migration if reserved resources are above the watermark; and
// 2. Updates the prometheus metrics we expose about the node
//
// NOTE: this function expects that the caller has acquired s.mu.
func (s *PluginState) reconcileNode(logger *zap.Logger, ns *nodeState) error {
	defer s.metrics.Nodes.Update(ns.node)

	err := s.balanceNode(logger, ns)
	if err != nil {
		return fmt.Errorf("could not trigger live migrations: %w", err)
	}

	return nil
}

// updateNodeMetricsAndRequeue updates the node's metrics and puts it back in the reconcile queue.
//
// This is typically used to force nodes to stay up-to-date after we update a pod on the node, while
// helping with fairness between time spent reconciling the pod vs the node.
func (s *PluginState) updateNodeMetricsAndRequeue(logger *zap.Logger, ns *nodeState) {
	if err := s.requeueNode(ns.node.Name); err != nil {
		logger.Error("Failed to requeue Node", zap.Error(err))
	}
	s.metrics.Nodes.Update(ns.node)
}

func (s *PluginState) balanceNode(logger *zap.Logger, ns *nodeState) error {
	var err error
	// use Speculatively() to produce a temporary node that triggerMigrationsIfNecessary can use to
	// evaluate what the state *will* look like after the migrations are running.
	ns.node.Speculatively(func(tmpNode *state.Node) (commit bool) {
		originalNode := ns.node
		requestedMigrations := []types.UID{}
		for uid := range ns.requestedMigrations {
			requestedMigrations = append(requestedMigrations, uid)
		}
		err = triggerMigrationsIfNecessary(
			logger,
			originalNode,
			tmpNode,
			requestedMigrations,
			func(podUID types.UID) error {
				if err := s.requeuePod(podUID); err != nil {
					return err
				}
				ns.requestedMigrations[podUID] = struct{}{}
				return nil
			},
		)

		return false // Never actually commit; we're just using Speculatively() for a cheap copy.
	})
	return err
}

// NOTE: this function expects that the caller has acquired s.mu.
func (s *PluginState) cleanupNode(logger *zap.Logger, ns *nodeState) {
	// remove any tentatively scheduled pods that are on this node
	for uid, nodeName := range s.tentativelyScheduled {
		if nodeName == ns.node.Name {
			delete(s.tentativelyScheduled, uid)
		}
	}

	s.metrics.Nodes.Remove(ns.node)
	delete(s.nodes, ns.node.Name)

	logger.Info("Removed node", zap.Object("Node", ns.node))
}
