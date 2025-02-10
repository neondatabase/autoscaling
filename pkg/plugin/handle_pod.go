package plugin

// Handling of Pod events.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/samber/lo"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/plugin/reconcile"
	"github.com/neondatabase/autoscaling/pkg/plugin/state"
	"github.com/neondatabase/autoscaling/pkg/util/patch"
)

func (s *PluginState) HandlePodEvent(
	logger *zap.Logger,
	kind reconcile.EventKind,
	pod *corev1.Pod,
) (*reconcile.Result, error) {
	if s.config.ignoredNamespace(pod.Namespace) {
		// We intentionally don't include ignored pods in the namespace.
		return nil, nil
	}

	expectExists := kind == reconcile.EventKindModified || kind == reconcile.EventKindDeleted

	switch kind {
	case reconcile.EventKindAdded, reconcile.EventKindModified:
		updateResult, err := s.updatePod(logger, pod, expectExists)

		var reconcileResult *reconcile.Result
		if updateResult != nil && updateResult.retryAfter != nil {
			reconcileResult = &reconcile.Result{RetryAfter: *updateResult.retryAfter}
		}

		if err != nil {
			return reconcileResult, err
		}

		var retryAfter time.Duration
		if updateResult != nil {
			if updateResult.afterUnlock != nil {
				if err := updateResult.afterUnlock(); err != nil {
					return reconcileResult, err
				}
			}

			if updateResult.needsMoreResources {
				// mark this as failing; don't try again sooner than 5 seconds later.
				return &reconcile.Result{RetryAfter: 5 * time.Second}, errors.New("not enough resources to grant request for pod")
			}

			retryAfter = lo.FromPtr(updateResult.retryAfter)
		}
		return &reconcile.Result{RetryAfter: retryAfter}, nil

	case reconcile.EventKindDeleted, reconcile.EventKindEphemeral:
		err := s.deletePod(logger, pod, expectExists)
		return nil, err
	default:
		panic("unreachable")
	}
}

type podUpdateResult struct {
	needsMoreResources bool
	afterUnlock        func() error
	retryAfter         *time.Duration
}

func (s *PluginState) updatePod(
	logger *zap.Logger,
	pod *corev1.Pod,
	expectExists bool,
) (*podUpdateResult, error) {
	newPod, err := state.PodStateFromK8sObj(pod)
	if err != nil {
		return nil, fmt.Errorf("could not get state from Pod object: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var ns *nodeState // pre-declare this so we can update metrics in a defer
	defer func() {
		if ns != nil {
			s.updateNodeMetricsAndRequeue(logger, ns)
		}
	}()

	tentativeNode, scheduled := s.tentativelyScheduled[pod.UID]
	if scheduled {
		if pod.Spec.NodeName == tentativeNode {
			// oh hey, this pod has been properly scheduled now! Let's remove it from the
			// "tentatively scheduled" set.
			delete(s.tentativelyScheduled, pod.UID)
			logger.Info("Pod was scheduled as expected")
		} else if pod.Spec.NodeName != "" {
			logger.Panic(
				"Pod was scheduled onto a different Node than tentatively recorded",
				zap.String("OriginalNodeName", tentativeNode),
				zap.String("NewNodeName", pod.Spec.NodeName),
			)
		}
	}

	if !scheduled && pod.Spec.NodeName == "" {
		// still hasn't been scheduled, nothing to do yet.
		logger.Info("Skipping event for Pod that has not yet been scheduled")
		return nil, nil
	}

	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		nodeName = tentativeNode
	}

	logger = logger.With(logFieldForNodeName(nodeName))

	var ok bool
	ns, ok = s.nodes[nodeName]
	if !ok {
		return nil, fmt.Errorf("pod's node %q is not present in local state", nodeName)
	}

	// make the changes in Speculatively() so that we can log both states before committing, and
	// provide protection from panics.
	ns.node.Speculatively(func(n *state.Node) (commit bool) {
		oldPod, exists := ns.node.GetPod(newPod.UID)
		// note: only warn if the pod unexpectedly *does* exist; the normal path is that pods are
		// modified to be assigned their node, so we can't reliably say when a pod should not have
		// *previously* been present.
		if exists && !expectExists {
			logger.Warn("Updating Pod that unexpectedly exists in local state")
		}

		if exists {
			podChanged := n.UpdatePod(oldPod, newPod)
			if podChanged {
				logger.Info(
					"Updated Pod in local state",
					zap.Object("OldPod", oldPod),
					zap.Object("Pod", newPod),
					zap.Object("OldNode", ns.node),
					zap.Object("Node", n),
				)
			}
		} else {
			n.AddPod(newPod)
			logger.Info(
				"Added Pod to local state",
				zap.Object("Pod", newPod),
				zap.Object("OldNode", ns.node),
				zap.Object("Node", n),
			)
		}

		// Commit the changes so far, then keep going.
		return true
	})

	// At this point, our local state has been updated according to the Pod object from k8s.
	//
	// All that's left is to handle VMs that are the responsibility of *this* scheduler.
	if lo.IsEmpty(newPod.VirtualMachine) || pod.Spec.SchedulerName != s.config.SchedulerName {
		return nil, nil
	}

	if _, ok := ns.requestedMigrations[newPod.UID]; ok {
		// If the pod is already migrating, remove it from requestedMigrations.
		if newPod.Migrating {
			delete(ns.requestedMigrations, newPod.UID)
		} else if !newPod.Migratable {
			logger.Warn("Canceling previously wanted migration because Pod is not migratable")
			delete(ns.requestedMigrations, newPod.UID)
		} else {
			// Otherwise: the pod is not migrating, but *is* migratable. Let's trigger migration.
			logger.Info("Creating migration for Pod")
			return &podUpdateResult{
				needsMoreResources: false,
				// we need to release the lock to trigger the migration, otherwise we may slow down
				// processing due to API delays.
				afterUnlock: func() error {
					if err := s.createMigrationForPod(logger, newPod); err != nil {
						return fmt.Errorf("could not create migration for Pod: %w", err)
					}
					return nil
				},
				// All done for now; retry in 5s if the pod is not migrating yet.
				retryAfter: lo.ToPtr(5 * time.Second),
			}, nil
		}
	}

	if !newPod.Migrating {
		return s.reconcilePodResources(logger, ns, pod, newPod), nil
	}

	return nil, nil
}

func (s *PluginState) createMigrationForPod(logger *zap.Logger, pod state.Pod) error {
	vmm := &vmv1.VirtualMachineMigration{
		ObjectMeta: metadataForNewMigration(pod),
		Spec: vmv1.VirtualMachineMigrationSpec{
			VmName: pod.VirtualMachine.Name,

			// FIXME: NeonVM's VirtualMachineMigrationSpec has a bunch of boolean fields that aren't
			// pointers, which means we need to explicitly set them when using the Go API.
			PreventMigrationToSameHost: true,
			CompletionTimeout:          3600,
			Incremental:                true,
			AutoConverge:               true,
			MaxBandwidth:               resource.MustParse("1Gi"),
			AllowPostCopy:              false,
		},
	}

	return s.createMigration(logger, vmm)
}

func (s *PluginState) reconcilePodResources(
	logger *zap.Logger,
	ns *nodeState,
	oldPodObj *corev1.Pod,
	oldPod state.Pod,
) *podUpdateResult {
	// Quick check: Does this pod have autoscaling enabled? if no, then we shouldn't set our
	// annotations on it -- particularly because we may end up with stale approved resources when
	// the VM scales, and that can cause issues if autoscaling is enabled later.
	if !api.HasAutoscalingEnabled(oldPodObj) {
		return nil
	}

	var needsMoreResources bool

	desiredPod := oldPod
	ns.node.Speculatively(func(n *state.Node) (commit bool) {
		// Do a pass of reconciling this pod, in case there's resources it's requested that we can
		// now grant.
		done := n.ReconcilePodReserved(&desiredPod)
		needsMoreResources = !done
		// Don't accept these changes -- more on that below.
		return false
	})

	_, hasApprovedAnnotation := oldPodObj.Annotations[api.InternalAnnotationResourcesApproved]

	// At this point, desiredPod has the updated state of the pod that *would* be the case if we
	// fully reconcile it.
	//
	// If there are changes, we have are a few things to consider.
	//
	// 1. If we haven't finished handling the initial state, we cannot accept the changes (maybe not
	//    all pods are present). We should requeue this Pod once we're acting on more complete
	//    information.
	//
	// 2. If we're changing the reserved resources, we'll need to update the annotation for approved
	//    resources on the VM object to communicate that. We need to release the lock on the state
	//    *before* doing that, because otherwise we'll add significant processing delays.
	//
	// 3. If we fail to update the VM object, we need to *not* have decreased the resources reserved
	//    for the Pod -- we may not know if the change actually took effect, and if it didn't, we
	//    should still admit the possibility that the resources previously reserved will go back to
	//    being used.
	//
	// So, putting all that together:
	//
	// - Don't do anything if we haven't completed startup.
	// - Set the pod state to the maximum reserved between oldPod and newPod, and *then* patch
	//   the VM object.

	if !s.startupDone {
		s.requeueAfterStartup[oldPod.UID] = struct{}{}
		// don't report anything, even if needsMoreResources. We're waiting for startup to finish!
		return nil
	}
	if oldPod == desiredPod && hasApprovedAnnotation {
		// no changes, nothing to do. Although, if we *do* need more resources, log something about
		// it so we're not failing silently.
		if needsMoreResources {
			logger.Warn(
				"Unable to satisfy requested resources for Pod",
				zap.Object("Pod", oldPod),
				zap.Object("Node", ns.node),
			)
		}
		return &podUpdateResult{
			needsMoreResources: needsMoreResources,
			afterUnlock:        nil,
			retryAfter:         nil,
		}
	}

	// Startup done. Either we have changes or the pod is missing the approved resources annotation.
	//
	// If it hasn't been too soon since the last patch:
	// Update the local state if necessary; release the lock; patch the VM.
	//
	// Otherwise, mark retryAfter with the wait time necessary.
	now := time.Now()
	lastPatch, previouslyPatched := ns.podsVMPatchedAt[oldPod.UID]

	canRetryAt := now
	if previouslyPatched {
		canRetryAt = lastPatch.Add(time.Second * time.Duration(s.config.PatchRetryWaitSeconds))
	}

	if now.Before(canRetryAt) {
		retryAfter := canRetryAt.Sub(now)
		logger.Warn(
			"Want to patch VirtualMachine for reserved resources, but too soon to re-patch. Waiting.",
			zap.Duration("retryAfter", retryAfter),
		)
		return &podUpdateResult{
			needsMoreResources: needsMoreResources,
			afterUnlock:        nil,
			retryAfter:         &retryAfter,
		}
	}

	newPod := desiredPod
	newPod.CPU.Reserved = max(desiredPod.CPU.Reserved, oldPod.CPU.Reserved)
	newPod.Mem.Reserved = max(desiredPod.Mem.Reserved, oldPod.Mem.Reserved)

	if newPod == oldPod {
		if oldPod != desiredPod {
			logger.Info(
				"Reserved resources can be updated for Pod, patching VirtualMachine without updating local state",
				zap.Object("Pod", oldPod),
				zap.Object("DesiredPod", desiredPod),
				zap.Object("Node", ns.node),
			)
		} else /* implies !hasApprovedAnnotation */ {
			logger.Info(
				"Pod is missing approved resources annotation, patching VirtualMachine",
				zap.Object("Pod", oldPod),
			)
		}
	} else {
		ns.node.Speculatively(func(newNode *state.Node) (commit bool) {
			newNode.UpdatePod(oldPod, newPod)
			logger.Info(
				"Reserved resources updated for Pod, patching VirtualMachine",
				zap.Object("OldPod", oldPod),
				zap.Object("DesiredPod", desiredPod),
				zap.Object("Pod", newPod),
				zap.Object("OldNode", ns.node),
				zap.Object("Node", newNode),
			)

			return true // yes, commit the changes to use newPod
		})
	}

	ns.podsVMPatchedAt[oldPod.UID] = now

	return &podUpdateResult{
		needsMoreResources: needsMoreResources,
		afterUnlock: func() error {
			return s.patchReservedResourcesForPod(logger, oldPodObj, desiredPod)
		},
		retryAfter: nil,
	}
}

func (s *PluginState) patchReservedResourcesForPod(
	logger *zap.Logger,
	oldPodObj *corev1.Pod,
	newPod state.Pod,
) error {
	// Broadly, the idea with the patch is that we only want to update the reserved resources if the
	// resources that were requested are still current.
	//
	// So, because JSON Patch allows "tests" to check equality, we'll use those here to check
	// against the requested resources.

	marshalJSON := func(value any) string {
		bs, err := json.Marshal(value)
		if err != nil {
			panic(fmt.Sprintf("failed to marshal value: %s", err))
		}
		return string(bs)
	}

	var patches []patch.Operation

	// Check that the scaling unit and requested resources are the same:
	if scalingUnitJSON, ok := oldPodObj.Annotations[api.AnnotationAutoscalingUnit]; ok {
		patches = append(patches, patch.Operation{
			Op: patch.OpTest,
			Path: fmt.Sprintf(
				"/metadata/annotations/%s",
				patch.PathEscape(api.AnnotationAutoscalingUnit),
			),
			Value: scalingUnitJSON,
		})
	}
	if requestedJSON, ok := oldPodObj.Annotations[api.InternalAnnotationResourcesRequested]; ok {
		patches = append(patches, patch.Operation{
			Op: patch.OpTest,
			Path: fmt.Sprintf(
				"/metadata/annotations/%s",
				patch.PathEscape(api.InternalAnnotationResourcesRequested),
			),
			Value: requestedJSON,
		})
	}

	// ... and then if so, set the approved resources appropriately:
	reservedJSON := marshalJSON(api.Resources{
		VCPU: newPod.CPU.Reserved,
		Mem:  newPod.Mem.Reserved,
	})
	patches = append(patches, patch.Operation{
		Op: patch.OpReplace,
		Path: fmt.Sprintf(
			"/metadata/annotations/%s",
			patch.PathEscape(api.InternalAnnotationResourcesApproved),
		),
		Value: reservedJSON,
	})

	_, hasApprovedAnnotation := oldPodObj.Annotations[api.InternalAnnotationResourcesApproved]

	hasKnownAnnotations := len(patches) > 1 || hasApprovedAnnotation

	// If there's no other known annotations at this point, it's possible that the VM's annotations
	// are completely empty. If so, any operations to add an annotation will fail because the
	// 'annotations' field doesn't exist!
	//
	// So we'll try a simple patch to create the annotations field as part of the operation, and
	// then fall through to the normal one if that gets a conflict:
	if !hasKnownAnnotations {
		addPatches := []patch.Operation{
			{
				Op:    patch.OpTest,
				Path:  "/metadata/annotations",
				Value: (*struct{})(nil), // typed nil, so that it shows up as 'null'
			},
			{
				Op:    patch.OpAdd,
				Path:  "/metadata/annotations",
				Value: struct{}{},
			},
			patches[0],
		}
		err := s.patchVM(newPod.VirtualMachine, addPatches)
		if err != nil {
			if apierrors.IsInvalid(err) {
				logger.Warn(
					"Failed to add-path patch VirtualMachine because preconditions failed, trying again with normal path",
					zap.Any("patches", addPatches),
					zap.Error(err),
				)
				// fall through below...
			} else {
				logger.Error("Failed to add-path patch VirtualMachine", zap.Any("patches", addPatches), zap.Error(err))
				return err
			}
		} else {
			// we successfully patched the VM!
			logger.Info("Patched VirtualMachine for approved resources", zap.Any("patches", addPatches))
			return nil
		}
	}

	err := s.patchVM(newPod.VirtualMachine, patches)
	// When a JSON patch "test" fails, the API server returns 422 which is internally represented in
	// the k8s error types as a "StatusReasonInvalid".
	// We'll special-case that here -- it's still an error but we want to be more clear about it.
	if err != nil {
		if apierrors.IsInvalid(err) {
			logger.Warn("Failed to patch VirtualMachine because preconditions failed", zap.Any("patches", patches), zap.Error(err))
			return errors.New("local pod state doesn't match most recent VM state")
		} else {
			logger.Error("Failed to patch VirtualMachine", zap.Any("patches", patches), zap.Error(err))
			return err
		}
	}
	logger.Info("Patched VirtualMachine for approved resources", zap.Any("patches", patches))
	return nil
}

func (s *PluginState) deletePod(logger *zap.Logger, pod *corev1.Pod, expectExists bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		var ok bool
		if nodeName, ok = s.tentativelyScheduled[pod.UID]; !ok {
			logger.Info("Nothing to do for Pod deletion as it has no Node")
			return nil
		}
	}

	logger = logger.With(logFieldForNodeName(nodeName))

	ns, ok := s.nodes[nodeName]
	if !ok {
		logger.Error("Deleting Pod from internal state on a Node that doesn't exist")
		return nil // nothing we can do, all the local state is node-scoped
	}

	defer s.updateNodeMetricsAndRequeue(logger, ns)

	// Check if the pod exists:
	oldPod, exists := ns.node.GetPod(pod.UID)

	if !exists && expectExists {
		logger.Warn("Deleting Pod that unexpectedly doesn't exist in local state")
	} else if exists && !expectExists {
		logger.Warn("Deleting Pod that unexpectedly exists in local state")
	}

	// Clear any extra state for this pod
	delete(ns.requestedMigrations, pod.UID)
	delete(ns.podsVMPatchedAt, pod.UID)
	if exists {
		// ... and run the actual removal in Speculatively() so we can log the before/after in a single
		// line, and for panic safety.
		oldNode := ns.node
		ns.node.Speculatively(func(n *state.Node) (commit bool) {
			n.RemovePod(pod.UID)

			logger.Info(
				"Removed Pod from Node",
				zap.Object("Pod", oldPod),
				zap.Object("OldNode", oldNode),
				zap.Object("Node", n),
			)

			return true
		})
	} else {
		logger.Info(
			"Node unchanged from Pod deletion because it already isn't in local state",
			zap.Object("Node", ns.node),
		)
	}

	// Remove from tentatively scheduled, if it's there.
	// We need to do this last because earlier stages depend on this, and we might end up with
	// incomplete deletions if we clear this first, and hit an error later.
	if tentativeNode, ok := s.tentativelyScheduled[pod.UID]; ok {
		if pod.Spec.NodeName != "" && tentativeNode != pod.Spec.NodeName {
			logger.Panic(
				"Pod was scheduled onto a different Node than tentatively recorded",
				zap.String("OriginalNodeName", tentativeNode),
				zap.String("NewNodeName", pod.Spec.NodeName),
			)
		}
		delete(s.tentativelyScheduled, pod.UID)
	}

	return nil
}
