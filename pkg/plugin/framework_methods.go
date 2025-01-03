package plugin

import (
	"context"
	"fmt"
	"math/rand"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/neondatabase/autoscaling/pkg/plugin/metrics"
	"github.com/neondatabase/autoscaling/pkg/plugin/reconcile"
	"github.com/neondatabase/autoscaling/pkg/plugin/state"
)

const PluginName = "AutoscaleEnforcer"

// AutoscaleEnforcer implements Kubernetes scheduling plugins to account for available autoscaling
// resources during scheduling.
//
// For more info on k8s scheduling plugins, see:
// https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/
type AutoscaleEnforcer struct {
	logger  *zap.Logger
	state   *PluginState
	metrics *metrics.Framework
}

// Compile-time checks that AutoscaleEnforcer actually implements the interfaces we want it to
var (
	_ framework.Plugin           = (*AutoscaleEnforcer)(nil)
	_ framework.PostFilterPlugin = (*AutoscaleEnforcer)(nil)
	_ framework.FilterPlugin     = (*AutoscaleEnforcer)(nil)
	_ framework.ScorePlugin      = (*AutoscaleEnforcer)(nil)
	_ framework.ReservePlugin    = (*AutoscaleEnforcer)(nil)
)

// Name returns the name of the AutoscaleEnforcer plugin
//
// Name implements framework.Plugin.
func (e *AutoscaleEnforcer) Name() string {
	return PluginName
}

func logFieldForNodeName(nodeName string) zap.Field {
	return zap.Object("Node", zapcore.ObjectMarshalerFunc(
		func(enc zapcore.ObjectEncoder) error {
			enc.AddString("Name", nodeName)
			return nil
		},
	))
}

func (e *AutoscaleEnforcer) checkSchedulerName(logger *zap.Logger, pod *corev1.Pod) *framework.Status {
	if e.state.config.SchedulerName != pod.Spec.SchedulerName {
		err := fmt.Errorf(
			"Mismatched SchedulerName for pod: our config has %q, but the pod has %q",
			e.state.config.SchedulerName, pod.Spec.SchedulerName,
		)
		logger.Error("Pod has unexpected SchedulerName", zap.Error(err))
		return framework.NewStatus(framework.Error, err.Error())
	}
	return nil
}

// PostFilter is used by us for metrics on filter cycles that reject a Pod by filtering out all
// applicable nodes.
//
// Quoting the docs for PostFilter:
//
// > These plugins are called after Filter phase, but only when no feasible nodes were found for the
// > pod.
//
// PostFilter implements framework.PostFilterPlugin.
func (e *AutoscaleEnforcer) PostFilter(
	ctx context.Context,
	state *framework.CycleState,
	pod *corev1.Pod,
	filteredNodeStatusMap framework.NodeToStatusMap,
) (_ *framework.PostFilterResult, status *framework.Status) {
	ignored := e.state.config.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("PostFilter", pod, ignored)
	defer func() {
		e.metrics.IncFailIfnotSuccess("PostFilter", pod, ignored, status)
	}()

	logger := e.logger.With(
		zap.String("method", "PostFilter"),
		reconcile.ObjectMetaLogField("FilterPod", pod),
	)
	logger.Error("Pod rejected by all Filter method calls")

	return nil, nil // PostFilterResult is optional, nil Status is success.
}

// Filter gives our plugin a chance to signal that a pod shouldn't be put onto a particular node
//
// Filter implements framework.FilterPlugin.
func (e *AutoscaleEnforcer) Filter(
	ctx context.Context,
	_state *framework.CycleState,
	pod *corev1.Pod,
	nodeInfo *framework.NodeInfo,
) (status *framework.Status) {
	ignored := e.state.config.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("Filter", pod, ignored)
	defer func() {
		e.metrics.IncFailIfnotSuccess("Filter", pod, ignored, status)
	}()

	nodeName := nodeInfo.Node().Name

	logger := e.logger.With(
		zap.String("method", "Filter"),
		reconcile.ObjectMetaLogField("FilterPod", pod),
		reconcile.ObjectMetaLogField("Node", nodeInfo.Node()),
	)

	logger.Info("Handling Filter request")

	if status := e.checkSchedulerName(logger, pod); status != nil {
		return status
	}

	podState, err := state.PodStateFromK8sObj(pod)
	if err != nil {
		msg := "Error extracting local information for Pod"
		logger.Error(msg, zap.Error(err))
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("%s: %s", msg, err.Error()),
		)
	}

	// precreate a map for the pods that are proposed to exist on this node, so that we're not doing
	// this with the lock acquired.
	proposedPods := make(map[types.UID]*framework.PodInfo)
	for _, p := range nodeInfo.Pods {
		proposedPods[p.Pod.UID] = p
	}

	e.state.mu.Lock()
	defer e.state.mu.Unlock()

	ns, ok := e.state.nodes[nodeName]
	if !ok {
		msg := "Node not found in local state"
		logger.Error(msg)
		return framework.NewStatus(framework.Error, fmt.Sprintf("%s: %s", msg, err.Error()))
	}

	var approve bool
	ns.node.Speculatively(func(n *state.Node) (commit bool) {
		approve = e.filterCheck(logger, ns.node, n, podState, proposedPods)
		return false // never commit these changes; we're just using this for a temp node.
	})

	if !approve {
		return framework.NewStatus(framework.Unschedulable, "Not enough resources for Pod")
	} else {
		return nil
	}
}

func (e *AutoscaleEnforcer) filterCheck(
	logger *zap.Logger,
	oldNode *state.Node,
	tmpNode *state.Node,
	filterPod state.Pod,
	otherPods map[types.UID]*framework.PodInfo,
) (ok bool) {
	type podInfo struct {
		Namespace string
		Name      string
		UID       types.UID
	}

	// Our strategy here is to make the set of pods on the temporary node match what was supplied as
	// the proposal by the core scheduler -- basically making sure that it's exactly otherPods and
	// nothing more.
	//
	// We *could* derive a new node state from scratch using the node and pod objects we were
	// provided, but this risks us making decisions on inconsistent state (maybe the core
	// scheduler's view is outdated), and error handling is trickier -- here, we can at least use
	// the last known good state of the pod.
	var localNotInProposed []podInfo
	for uid, pod := range tmpNode.Pods() {
		if _, ok := otherPods[uid]; !ok {
			// pod is not in the set given to the filter method. We should remove it from the temp
			// node, and mark that for later.
			tmpNode.RemovePod(uid)
			localNotInProposed = append(localNotInProposed, podInfo{
				Namespace: pod.Namespace,
				Name:      pod.Name,
				UID:       pod.UID,
			})
		} else {
			// Otherwise, the pod *is* in the set for filter, and so we should remove it from
			// 'otherPods' to mark it as already included.
			delete(otherPods, uid)
		}
	}

	// all that remains in otherPods is the pods that were not removed from iterating through
	// tmpNode's pods.
	var proposedNotInLocalState []podInfo
	for _, p := range otherPods {
		proposedNotInLocalState = append(proposedNotInLocalState, podInfo{
			Namespace: p.Pod.Namespace,
			Name:      p.Pod.Name,
			UID:       p.Pod.UID,
		})

		pod, err := state.PodStateFromK8sObj(p.Pod)
		if err != nil {
			logger.Error(
				"Ignoring extra Pod in Filter stage because extracting custom state failed",
				reconcile.ObjectMetaLogField("Pod", p.Pod),
				zap.Error(err),
			)
			continue
		}

		tmpNode.AddPod(pod)
	}

	// At this point:
	// * oldNode has the actual local state for the node
	// * tmpNode has the state as given by the filter pods
	// * localNotInProposed are the pods in oldNode but not tmpNode
	// * proposedNotInLocalState are the pods in tmpNode but not oldNode
	//
	// We'll use (another) Speculatively() to simultaneously show all these, plus the state
	// resulting from adding the Pod to filter.
	var canAddToNode bool
	tmpNode.Speculatively(func(n *state.Node) (commit bool) {
		n.AddPod(filterPod)
		canAddToNode = !n.OverBudget()

		var msg string
		if canAddToNode {
			msg = "Allowing Pod placement onto this Node"
		} else {
			msg = "Rejecting Pod placement onto this Node"
		}
		logger.Info(
			msg,
			zap.Object("Node", oldNode),
			zap.Object("FilterNode", tmpNode),
			zap.Object("FilterNodeWithPod", n),
			zap.Object("Pod", filterPod),
			zap.Any("LocalPodsNotInFilterState", localNotInProposed),
			zap.Any("FilterPodsNotInLocalState", proposedNotInLocalState),
		)

		return false // don't commit. Doesn't really matter because we're operating on the temp node.
	})
	return canAddToNode
}

// Score allows our plugin to express which nodes should be preferred for scheduling new pods onto
//
// Even though this function is given (pod, node) pairs, our scoring is only really dependent on
// values of the node. However, we have special handling for when the pod no longer fits in the node
// (even though it might have during the Filter plugin) - we can't return a failure, because that
// would cause *all* scheduling of the pod to fail, so we instead return the minimum score.
//
// The scores might not be consistent with each other, due to ongoing changes in the node. That's
// ok, because nothing relies on strict correctness here, and they should be approximately correct
// anyways.
//
// Score implements framework.ScorePlugin.
func (e *AutoscaleEnforcer) Score(
	ctx context.Context,
	_state *framework.CycleState,
	pod *corev1.Pod,
	nodeName string,
) (_ int64, status *framework.Status) {
	ignored := e.state.config.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("NormalizeScore", pod, ignored)
	defer func() {
		e.metrics.IncFailIfnotSuccess("NormalizeScore", pod, ignored, status)
	}()

	logger := e.logger.With(
		zap.String("method", "NormalizeScore"),
		reconcile.ObjectMetaLogField("Pod", pod),
	)

	logger.Info("Handling Score request", logFieldForNodeName(nodeName))

	if status := e.checkSchedulerName(logger, pod); status != nil {
		return framework.MinNodeScore, status
	}

	podState, err := state.PodStateFromK8sObj(pod)
	if err != nil {
		msg := "Error extracting local information for Pod"
		logger.Error(msg, zap.Error(err))
		return framework.MinNodeScore, framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("%s: %s", msg, err.Error()),
		)
	}

	e.state.mu.Lock()
	defer e.state.mu.Unlock()

	ns, ok := e.state.nodes[nodeName]
	if !ok {
		msg := "Node not found in local state"
		logger.Error(msg)
		status := framework.NewStatus(framework.Error, fmt.Sprintf("%s: %s", msg, err.Error()))
		return framework.MinNodeScore, status
	}

	var score int64

	ns.node.Speculatively(func(tmp *state.Node) (commit bool) {
		tmp.AddPod(podState)

		overBudget := tmp.OverBudget()
		if overBudget {
			score = framework.MinNodeScore
			logger.Warn(
				"No room for Pod on Node, giving minimum score (typically handled by Filter instead)",
				zap.Int64("Score", score),
				zap.Object("NodeWithPod", tmp),
			)
		} else {
			cfg := e.state.config.Scoring
			cpuScore := calculateScore(cfg, tmp.CPU.Reserved, tmp.CPU.Total, e.state.maxNodeCPU)
			memScore := calculateScore(cfg, tmp.Mem.Reserved, tmp.Mem.Total, e.state.maxNodeMem)
			scoreFraction := min(cpuScore, memScore)

			scoreLen := framework.MaxNodeScore - framework.MinNodeScore
			score = framework.MinNodeScore + int64(float64(scoreLen)*scoreFraction)

			logger.Info(
				"Scored Pod placement for Node",
				zap.Int64("Score", score),
				zap.Float64("CPUFraction", cpuScore),
				zap.Float64("MemFraction", memScore),
				zap.Object("NodeWithPod", tmp),
			)
		}

		return false // never commit, we're doing this just to check.
	})

	return score, nil
}

type floatable interface {
	AsFloat64() float64
}

// Refer to the comments in ScoringConfig for more. Also, see: https://www.desmos.com/calculator/wg8s0yn63s
func calculateScore[T floatable](
	cfg ScoringConfig,
	reserved T,
	total T,
	maxTotalSeen T,
) float64 {
	y0 := cfg.MinUsageScore
	y1 := cfg.MaxUsageScore
	xp := cfg.ScorePeak

	fraction := reserved.AsFloat64() / total.AsFloat64()
	scale := total.AsFloat64() / maxTotalSeen.AsFloat64()

	score := float64(1) // if fraction == nodeConf.ScorePeak
	if fraction < cfg.ScorePeak {
		score = y0 + (1-y0)/xp*fraction
	} else if fraction > cfg.ScorePeak {
		score = y1 + (1-y1)/(1-xp)*(1-fraction)
	}

	return score * scale
}

// NormalizeScore weights scores uniformly in the range [minScore, trueScore], where
// minScore is framework.MinNodeScore + 1.
//
// NormalizeScore implements framework.ScoreExtensions.
func (e *AutoscaleEnforcer) NormalizeScore(
	ctx context.Context,
	state *framework.CycleState,
	pod *corev1.Pod,
	scores framework.NodeScoreList,
) (status *framework.Status) {
	ignored := e.state.config.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("NormalizeScore", pod, ignored)
	defer func() {
		e.metrics.IncFailIfnotSuccess("NormalizeScore", pod, ignored, status)
	}()

	logger := e.logger.With(
		zap.String("method", "NormalizeScore"),
		reconcile.ObjectMetaLogField("Pod", pod),
	)

	type scoring struct {
		Node     string
		OldScore int64
		NewScore int64
	}

	var scoreInfos []scoring

	for _, node := range scores {
		oldScore := node.Score

		// rand.Intn will panic if we pass in 0
		if oldScore == 0 {
			scoreInfos = append(scoreInfos, scoring{
				Node:     node.Name,
				OldScore: oldScore,
				NewScore: node.Score,
			})
			continue
		}

		// This is different from framework.MinNodeScore. We use framework.MinNodeScore
		// to indicate that a pod should not be placed on a node. The lowest
		// actual score we assign a node is thus framework.MinNodeScore + 1
		minScore := framework.MinNodeScore + 1

		// We want to pick a score in the range [minScore, score], so use score + 1 - minScore, as
		// rand.Intn picks a number in the *half open* range [0, n).
		newScore := minScore + int64(rand.Intn(int(oldScore+1-minScore)))
		node.Score = newScore
		scoreInfos = append(scoreInfos, scoring{
			Node:     node.Name,
			OldScore: oldScore,
			NewScore: newScore,
		})
	}

	logger.Info("Randomized Node scores for Pod", zap.Any("scores", scoreInfos))
	return nil
}

// ScoreExtensions is required for framework.ScorePlugin, and can return nil if it's not used.
// However, we do use it, to randomize scores (when enabled).
func (e *AutoscaleEnforcer) ScoreExtensions() framework.ScoreExtensions {
	if e.state.config.Scoring.Randomize {
		return e
	} else {
		return nil
	}
}

// Reserve signals to our plugin that a particular pod will (probably) be bound to a node, giving us
// a chance to both (a) reserve the resources it needs within the node and (b) reject the pod if
// there aren't enough.
//
// Reserve implements framework.ReservePlugin.
func (e *AutoscaleEnforcer) Reserve(
	ctx context.Context,
	_state *framework.CycleState,
	pod *corev1.Pod,
	nodeName string,
) (status *framework.Status) {
	ignored := e.state.config.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("Reserve", pod, ignored)
	defer func() {
		e.metrics.IncFailIfnotSuccess("Reserve", pod, ignored, status)
	}()

	logger := e.logger.With(
		zap.String("method", "Reserve"),
		reconcile.ObjectMetaLogField("Pod", pod),
	)

	if ignored {
		logger.Info("Skipping Reserve request for ignored namespace", logFieldForNodeName(nodeName))
		return nil
	}

	logger.Info("Handling Reserve request", logFieldForNodeName(nodeName))

	if status := e.checkSchedulerName(logger, pod); status != nil {
		return status
	}

	podState, err := state.PodStateFromK8sObj(pod)
	if err != nil {
		msg := "Error extracting local information for Pod"
		logger.Error(msg, zap.Error(err))
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			fmt.Sprintf("%s: %s", msg, err.Error()),
		)
	}

	e.state.mu.Lock()
	defer e.state.mu.Unlock()

	if _, ok := e.state.tentativelyScheduled[pod.UID]; ok {
		msg := "Pod already exists in set of tentatively scheduled pods"
		logger.Error(msg)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, msg)
	}

	ns, ok := e.state.nodes[nodeName]
	if !ok {
		msg := "Node not found in local state"
		logger.Error(msg)
		return framework.NewStatus(framework.Error, fmt.Sprintf("%s: %s", msg, err.Error()))
	}

	// use Speculatively() to compare before/after
	//
	// Note that we always allow the change to go through, even though we *could* deny the Reserve()
	// if there isn't room. We don't deny, because that's ultimately less reliable.
	// For more, see https://github.com/neondatabase/autoscaling/issues/869
	ns.node.Speculatively(func(n *state.Node) (commit bool) {
		n.AddPod(podState)
		e.state.tentativelyScheduled[pod.UID] = nodeName

		logger.Info(
			"Reserved tentatively scheduled Pod on Node",
			zap.Object("Pod", podState),
			zap.Object("OldNode", ns.node),
			zap.Object("Node", n),
		)

		return true // Yes, commit these changes.
	})

	if ns.node.OverBudget() {
		e.metrics.IncReserveOverBudget(ignored, ns.node)
	}

	return nil
}

// Unreserve marks a pod as no longer on-track to being bound to a node, so we can release the
// resources we previously reserved for it.
//
// Note: the documentation for ReservePlugin indicates that Unreserve both (a) must be idempotent
// and (b) may be called without a previous call to Reserve for the same pod.
//
// Unreserve implements framework.ReservePlugin.
func (e *AutoscaleEnforcer) Unreserve(
	ctx context.Context,
	_state *framework.CycleState,
	pod *corev1.Pod,
	nodeName string,
) {
	ignored := e.state.config.ignoredNamespace(pod.Namespace)

	e.metrics.IncMethodCall("Unreserve", pod, ignored)

	logger := e.logger.With(
		zap.String("method", "Unreserve"),
		reconcile.ObjectMetaLogField("Pod", pod),
	)

	logger.Info("Handling Unreserve request", logFieldForNodeName(nodeName))

	e.state.mu.Lock()
	defer e.state.mu.Unlock()

	nn, ok := e.state.tentativelyScheduled[pod.UID]
	if !ok {
		logger.Warn("Cannot unreserve Pod, it isn't in the set of tentatively scheduled pods")
		return
	} else if nn != nodeName {
		logger.Panic(
			"Pod is tentatively scheduled on an unexpected node",
			zap.String("ExpectedNode", nodeName),
			zap.String("ActualNode", nn),
		)
		return
	}

	ns, ok := e.state.nodes[nodeName]
	if !ok {
		logger.Error("Node not found in local state", logFieldForNodeName(nodeName))
	}

	ns.node.Speculatively(func(n *state.Node) (commit bool) {
		p, ok := n.GetPod(pod.UID)
		if !ok {
			logger.Panic("Pod unexpectedly doesn't exist on Node", logFieldForNodeName(nodeName))
			return
		}
		n.RemovePod(pod.UID)
		delete(e.state.tentativelyScheduled, pod.UID)

		logger.Info(
			"Unreserved tentatively scheduled Pod",
			zap.Object("Pod", p),
			zap.Object("OldNode", ns.node),
			zap.Object("Node", n),
		)
		return true // yes, commit these changes.
	})
}
