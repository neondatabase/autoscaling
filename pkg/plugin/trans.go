package plugin

// this file primarily contains the type resourceTransition[T], for handling a number of operations
// on resources, and pretty-formatting summaries of the operations. There are also other, unrelated
// methods to perform similar functionality.
//
// resourceTransitions are created with the collectResourceTransition function.
//
// Handling requested resources from the autoscaler-agent is done with the handleRequested method,
// and changes from VM deletion are handled by handleDeleted.

import (
	"errors"
	"fmt"

	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/constraints"

	"github.com/neondatabase/autoscaling/pkg/util"
)

// resourceTransitioner maintains the current state of its resource and handles the transition
// into a new state. A resource is associated with a pod, and the pod is associated with a node.
type resourceTransitioner[T constraints.Unsigned] struct {
	// node represents the current resource state of the node
	node *nodeResourceState[T]
	// pod represents the current resource state of the pod.
	// pod belongs to the node.
	pod *podResourceState[T]
}

func makeResourceTransitioner[T constraints.Unsigned](
	node *nodeResourceState[T], pod *podResourceState[T],
) resourceTransitioner[T] {
	return resourceTransitioner[T]{
		node: node,
		pod:  pod,
	}
}

// resourceState represents a resource state in its pod and its node. This is not necessarily the
// current state. It represents the resource state at a point in time.
type resourceState[T constraints.Unsigned] struct {
	node nodeResourceState[T]
	pod  podResourceState[T]
}

// snapshotState snapshots the current state of the resource transitioner by making a copy of
// its state.
func (r resourceTransitioner[T]) snapshotState() resourceState[T] {
	return resourceState[T]{*r.node, *r.pod}
}

// verdictSet represents a set of verdicts from some operation, for ease of logging
type verdictSet struct {
	cpu string
	mem string
}

// MarshalLogObject implements zapcore.ObjectMarshaler
func (s verdictSet) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("cpu", s.cpu)
	enc.AddString("mem", s.mem)
	return nil
}

// handleReserve adds the resources from the pod to the node, reporting if the node was over-budget
//
// Unlike handleRequested, this method should be called to add a NEW pod to the node.
//
// This is used in combination with Xact to speculatively *try* reserving a pod, and then revert if
// it would result in being over-budget.
func (r resourceTransitioner[T]) handleReserve() (overbudget bool, verdict string) {
	callback := func(oldState, newState resourceState[T]) string {
		if oldState.pod.Buffer != 0 {
			return fmt.Sprintf(
				"node reserved %v [buffer %v] + %v [buffer %v] -> %v [buffer %v] of total %v",
				// node reserved %v [buffer %v] + %v [buffer %v] ->
				oldState.node.Reserved, oldState.node.Buffer, newState.pod.Reserved, newState.pod.Buffer,
				// -> %v [buffer %v] of total %v
				newState.node.Reserved, newState.node.Buffer, oldState.node.Total,
			)
		} else {
			return fmt.Sprintf(
				"node reserved %v + %v -> %v of total %v",
				oldState.node.Reserved, newState.pod.Reserved, newState.node.Reserved, oldState.node.Total,
			)
		}
	}

	callbackUnexpected := func(message string) verdictCallback[T] {
		return func(_, _ resourceState[T]) string {
			panic(errors.New(message))
		}
	}

	// Currently, the caller provides the requested value via the Pod's Reserved field.
	// In order to convert this to work with handleRequestedGeneric, we need to explicitly represent
	// the increase from zero to pod.Reserved, so we do that by setting the Pod's value to zero and
	// passing in the requested amount separately.
	requested := r.pod.Reserved
	r.pod.Reserved = 0

	verdict = r.handleRequestedGeneric(
		requested,
		requestedOptions[T]{
			// by setting factor and forceApprovalMinimum to the requested amount, we force that
			// handleRequestedGeneric MUST reserve exactly that amount.
			// Then, we leave it up to the caller to accept/reject by returning whether the node was
			// overbudget, at the very end.
			factor:               requested,
			forceApprovalMinimum: requested,
			// only used for migrations
			convertIncreaseIntoPressure: false,
			// Yes, add buffer, because this is for reserving a pod for the first time. If the pod
			// was already known, it's the caller's responsibility to set buffer appropriately.
			addBuffer: true,

			callbackNoChange:                  callback,
			callbackDecreaseAutoApproved:      callbackUnexpected("got 'decrease approved' from logic to reserve new pod"),
			callbackIncreaseTurnedToPressure:  callback,
			callbackIncreaseRejected:          callbackUnexpected("got 'increase rejected' from logic to reserve new pod, but it is infallible"),
			callbackIncreasePartiallyApproved: callbackUnexpected("got 'partially approved' from logic to reserve new pod, but it is infallible"),
			callbackIncreaseFullyApproved:     callback,
		},
	)

	overbudget = r.node.Reserved > r.node.Total

	return overbudget, verdict
}

// handleRequested updates r.pod and r.node with changes to match the requested resources, within
// what's possible given the remaining resources.
//
// Any permitted increases are required to be a multiple of factor.
//
// Unlike handleReserve, this method should be called to update the resources for a preexisting pod
// on the node.
//
// A pretty-formatted summary of the outcome is returned as the verdict, for logging.
func (r resourceTransitioner[T]) handleRequested(
	requested T,
	lastPermit *T,
	startingMigration bool,
	factor T,
) (verdict string) {
	normalVerdictCallback := func(oldState, newState resourceState[T]) string {
		fmtString := "Register %d%s -> %d%s (pressure %d -> %d); " +
			"node reserved %d%s -> %d%s (of %d), " +
			"node capacityPressure %d -> %d (%d -> %d spoken for)"

		var oldPodBuffer string
		var oldNodeBuffer string
		var newNodeBuffer string
		if oldState.pod.Buffer != 0 {
			oldPodBuffer = fmt.Sprintf(" [buffer %d]", oldState.pod.Buffer)
			oldNodeBuffer = fmt.Sprintf(" [buffer %d]", oldState.node.Buffer)
			newNodeBuffer = fmt.Sprintf(" [buffer %d]", newState.node.Buffer)
		}

		var wanted string
		if newState.pod.Reserved != requested {
			wanted = fmt.Sprintf(" (wanted %d)", requested)
		}

		return fmt.Sprintf(
			fmtString,
			// Register %d%s -> %d%s (pressure %d -> %d)
			oldState.pod.Reserved, oldPodBuffer, newState.pod.Reserved, wanted, oldState.pod.CapacityPressure, newState.pod.CapacityPressure,
			// node reserved %d%s -> %d%s (of %d)
			oldState.node.Reserved, oldNodeBuffer, newState.node.Reserved, newNodeBuffer, oldState.node.Total,
			// node capacityPressure %d -> %d (%d -> %d spoken for)
			oldState.node.CapacityPressure, newState.node.CapacityPressure, oldState.node.PressureAccountedFor, newState.node.PressureAccountedFor,
		)
	}

	migrationVerdictCallback := func(oldState, newState resourceState[T]) string {
		fmtString := "Denying increase %d -> %d because the pod is starting migration; " +
			"node capacityPressure %d -> %d (%d -> %d spoken for)"

		return fmt.Sprintf(
			fmtString,
			// Denying increase %d -> %d because ...
			oldState.pod.Reserved, requested,
			// node capacityPressure %d -> %d (%d -> %d spoken for)
			oldState.node.CapacityPressure, newState.node.CapacityPressure, oldState.node.PressureAccountedFor, newState.node.PressureAccountedFor,
		)
	}

	var forceApprovalMinimum T
	if lastPermit != nil {
		forceApprovalMinimum = *lastPermit
	}

	return r.handleRequestedGeneric(
		requested,
		requestedOptions[T]{
			factor:               factor,
			forceApprovalMinimum: forceApprovalMinimum,
			// Can't increase during migrations.
			//
			// But we _will_ add the pod's request to the node's pressure, noting that its migration
			// will resolve it.
			convertIncreaseIntoPressure: startingMigration,
			// don't add buffer to the node; autoscaler-agent requests should reset it.
			addBuffer: false,

			callbackNoChange:                  normalVerdictCallback,
			callbackDecreaseAutoApproved:      normalVerdictCallback,
			callbackIncreaseTurnedToPressure:  migrationVerdictCallback,
			callbackIncreaseRejected:          normalVerdictCallback,
			callbackIncreasePartiallyApproved: normalVerdictCallback,
			callbackIncreaseFullyApproved:     normalVerdictCallback,
		},
	)
}

type requestedOptions[T constraints.Unsigned] struct {
	// factor provides a multiple binding the result of any increases from handleRequestedGeneric()
	//
	// For handling autoscaler-agent requests, this is the value of a compute unit's worth of that
	// resource (e.g. 0.25 CPU or 1 GiB memory).
	// For initially reserving a Pod, factor is set equal to the total additional resources, which
	// turns handleRequestedGeneric() into a binary function that either grants the entire request,
	// or none of it.
	factor T

	// forceApprovalMinimum sets the threshold above which handleRequestedGeneric() is allowed to
	// reject the request - i.e. if the request is less than or equal to forceApprovalMinimum, it
	// must be approved.
	//
	// This is typically set to a non-zero value when reserving resources for a Pod that has already
	// been scheduled (so there's nothing we can do about it), or when handling an autoscaler-agent
	// request that provides what a previous scheduler approved (via lastPermit).
	forceApprovalMinimum T

	// convertIncreaseIntoPressure causes handleRequestedGeneric() to reject any requested increases
	// in reserved resources, and instead add the amount of the increase to the CapacityPressure of
	// the Pod and Node.
	convertIncreaseIntoPressure bool

	// addBuffer causes handleRequestedGeneric() to additionally add the pod's Buffer field to the
	// node, under the assumption that the Buffer is completely new.
	//
	// Note that if addBuffer is true, buffer will be added *even if the reservation is rejected*.
	addBuffer bool

	callbackNoChange                  verdictCallback[T]
	callbackDecreaseAutoApproved      verdictCallback[T]
	callbackIncreaseTurnedToPressure  verdictCallback[T]
	callbackIncreaseRejected          verdictCallback[T]
	callbackIncreasePartiallyApproved verdictCallback[T]
	callbackIncreaseFullyApproved     verdictCallback[T]
}

type verdictCallback[T constraints.Unsigned] func(oldState, newState resourceState[T]) string

func (r resourceTransitioner[T]) handleRequestedGeneric(
	requested T,
	opts requestedOptions[T],
) (verdict string) {
	oldState := r.snapshotState()

	var verdictGenerator verdictCallback[T]

	if requested <= r.pod.Reserved {
		// Decrease "requests" are actually just notifications it's already happened
		r.node.Reserved -= r.pod.Reserved - requested
		r.pod.Reserved = requested
		// pressure is now zero, because the pod no longer wants to increase resources.
		r.pod.CapacityPressure = 0
		r.node.CapacityPressure -= oldState.pod.CapacityPressure

		if requested == r.pod.Reserved {
			verdictGenerator = opts.callbackNoChange
		} else /* requested < r.pod.Reserved */ {
			verdictGenerator = opts.callbackDecreaseAutoApproved
		}
	} else if opts.convertIncreaseIntoPressure /* implied: requested > pod.Reserved */ {
		r.pod.CapacityPressure = requested - r.pod.Reserved
		r.node.CapacityPressure = r.node.CapacityPressure + r.pod.CapacityPressure - oldState.pod.CapacityPressure

		verdictGenerator = opts.callbackIncreaseTurnedToPressure
	} else /* implied: requested > pod.Reserved && !opts.convertIncreaseIntoPressure */ {
		// The following comment was made 2022-11-28 (updated 2023-04-06, 2024-05-DD): (TODO: set date)
		//
		// Note: this function as currently written will actively cause the autoscaler-agent to use
		// resources that are uneven w.r.t. the number of compute units they represent.
		//
		// For example, we might have a request to go from 3CPU/3Gi -> 4CPU/4Gi but we only allow
		// 4CPU/3Gi, which would instead be 4 compute units of CPU but 3 compute units of memory.
		// When the autoscaler-agent receives the permit, it naively carries it out, giving itself a
		// resource allocation that isn't a multiple of compute units.
		//
		// This obviously isn't great. However, this *is* the most resilient solution, and it is
		// significantly simpler to implement, so it is the one I went with. As it currently stands,
		// the autoscaler-agent is still expected to submit requests that are multiples of compute
		// units, so the system should *eventually* stabilize (provided that the autoscaler-agent is
		// not violating its own guarantees). This allows us to gracefully handle many kinds of
		// stressors. Handling the resources separately *from the scheduler's point of view* makes
		// it much, much easier to deal with.
		//
		// Please think carefully before changing this.

		// note: it's entirely possible to have Reserved > Total, under a variety of
		// undesirable-but-impossible-to-prevent circumstances.
		remainingReservable := util.SaturatingSub(r.node.Total, r.node.Reserved)

		increase := requested - r.pod.Reserved

		// Increases are bounded by what's left in the node, rounded down to the nearest multiple of
		// the factor.
		maxIncrease := (remainingReservable / opts.factor) * opts.factor
		// ... but we must allow at least opts.forceApprovalMinimum
		increaseFromForceApproval := util.SaturatingSub(opts.forceApprovalMinimum, r.pod.Reserved)
		maxIncrease = util.Max(maxIncrease, increaseFromForceApproval)

		if increase > maxIncrease /* increases are bound by what's left in the node */ {
			r.pod.CapacityPressure = increase - maxIncrease
			// adjust node pressure accordingly. We can have old < new or new > old, so we shouldn't
			// directly += or -= (implicitly relying on overflow).
			r.node.CapacityPressure = r.node.CapacityPressure - oldState.pod.CapacityPressure + r.pod.CapacityPressure
			increase = maxIncrease // cap at maxIncrease.

			verdictGenerator = opts.callbackIncreasePartiallyApproved
		} else {
			// If we're not capped by maxIncrease, relieve pressure coming from this pod
			r.node.CapacityPressure -= r.pod.CapacityPressure
			r.pod.CapacityPressure = 0

			verdictGenerator = opts.callbackIncreaseFullyApproved
		}
		r.pod.Reserved += increase
		r.node.Reserved += increase
	}

	if r.pod.Buffer != 0 {
		if opts.addBuffer {
			r.node.Buffer += r.pod.Buffer
		} else /* !opts.addBuffer - buffer is only needed until the first request, so we can reset it */ {
			r.node.Buffer -= r.pod.Buffer
			r.pod.Buffer = 0
		}
	}

	newState := r.snapshotState()
	return verdictGenerator(oldState, newState)
}

// handleDeleted updates r.node with changes to match the removal of r.pod
//
// A pretty-formatted summary of the changes is returned as the verdict, for logging.
func (r resourceTransitioner[T]) handleDeleted(currentlyMigrating bool) (verdict string) {
	oldState := r.snapshotState()

	r.node.Reserved -= r.pod.Reserved
	r.node.CapacityPressure -= r.pod.CapacityPressure

	if currentlyMigrating {
		r.node.PressureAccountedFor -= r.pod.Reserved + r.pod.CapacityPressure
	}

	var podBuffer string
	var oldNodeBuffer string
	var newNodeBuffer string
	if r.pod.Buffer != 0 {
		r.node.Buffer -= r.pod.Buffer

		podBuffer = fmt.Sprintf(" [buffer %d]", r.pod.Buffer)
		oldNodeBuffer = fmt.Sprintf(" [buffer %d]", oldState.node.Buffer)
		newNodeBuffer = fmt.Sprintf(" [buffer %d]", r.node.Buffer)
	}

	fmtString := "pod had %d%s; node reserved %d%s -> %d%s, " +
		"node capacityPressure %d -> %d (%d -> %d spoken for)"
	verdict = fmt.Sprintf(
		fmtString,
		// pod had %d%s; node reserved %d%s -> %d%s
		r.pod.Reserved, podBuffer, oldState.node.Reserved, oldNodeBuffer, r.node.Reserved, newNodeBuffer,
		// node capacityPressure %d -> %d (%d -> %d spoken for)
		oldState.node.CapacityPressure, r.node.CapacityPressure, oldState.node.PressureAccountedFor, r.node.PressureAccountedFor,
	)
	return verdict
}

func (r resourceTransitioner[T]) handleNonAutoscalingUsageChange(newUsage T) (verdict string) {
	oldState := r.snapshotState()

	diff := newUsage - r.pod.Reserved
	r.pod.Reserved = newUsage
	r.node.Reserved += diff
	verdict = fmt.Sprintf(
		"pod reserved (%v -> %v), node reserved (%v -> %v)",
		oldState.pod.Reserved, r.pod.Reserved, oldState.node.Reserved, r.node.Reserved,
	)
	return verdict
}

// handleAutoscalingDisabled updates r.node with changes to clear any buffer and capacityPressure
// from r.pod
//
// A pretty-formatted summary of the changes is returned as the verdict, for logging.
func (r resourceTransitioner[T]) handleAutoscalingDisabled() (verdict string) {
	oldState := r.snapshotState()

	// buffer is included in reserved, so we reduce everything by buffer.
	buffer := r.pod.Buffer
	valuesToReduce := []*T{&r.node.Reserved, &r.node.Buffer, &r.pod.Reserved, &r.pod.Buffer}
	for _, v := range valuesToReduce {
		*v -= buffer
	}

	r.node.CapacityPressure -= r.pod.CapacityPressure
	r.pod.CapacityPressure = 0

	var nodeBufferChange string
	if oldState.pod.Buffer != 0 {
		nodeBufferChange = fmt.Sprintf(" [buffer %d -> %d]", oldState.node.Buffer, r.node.Buffer)
	}

	fmtString := "pod had buffer %d, capacityPressure %d; " +
		"node reserved %d -> %d%s, capacityPressure %d -> %d"
	verdict = fmt.Sprintf(
		fmtString,
		// pod had buffer %d, capacityPressure %d;
		oldState.pod.Buffer, oldState.pod.CapacityPressure,
		// node reserved %d -> %d%s, capacityPressure %d -> %d
		oldState.node.Reserved, r.node.Reserved, nodeBufferChange, oldState.node.CapacityPressure, r.node.CapacityPressure,
	)
	return verdict
}

// handleStartMigration updates r.node with changes to clear any buffer and capacityPressure from
// r.pod.
//
// If the pod is the migration source, this method *also* increases the node's PressureAccountedFor
// to match the pod's resource usage.
//
//nolint:unparam // linter complains about 'source'. FIXME: needs more work to figure this out.
func (r resourceTransitioner[T]) handleStartMigration(source bool) (verdict string) {
	// This method is basically the same as handleAutoscalingDisabled, except we also update the
	// node's PressureAccountedFor because any pressure generated by the pod will be resolved once
	// the migration completes and the pod gets deleted.

	oldState := r.snapshotState()

	buffer := r.pod.Buffer
	valuesToReduce := []*T{&r.node.Reserved, &r.node.Buffer, &r.pod.Reserved, &r.pod.Buffer}
	for _, v := range valuesToReduce {
		*v -= buffer
	}

	r.node.CapacityPressure -= r.pod.CapacityPressure
	r.pod.CapacityPressure = 0

	r.node.PressureAccountedFor += r.pod.Reserved

	fmtString := "pod had buffer %d, capacityPressure %d; " +
		"node reserved %d -> %d, capacityPressure %d -> %d, pressureAccountedFor %d -> %d"
	verdict = fmt.Sprintf(
		fmtString,
		// pod had buffer %d, capacityPressure %d;
		oldState.pod.Buffer, oldState.pod.CapacityPressure,
		// node reserved %d -> %d, capacityPressure %d -> %d
		oldState.node.Reserved, r.node.Reserved, oldState.node.CapacityPressure, r.node.CapacityPressure, oldState.node.PressureAccountedFor, r.node.PressureAccountedFor,
	)
	return verdict
}

func handleUpdatedLimits[T constraints.Unsigned](
	node *nodeResourceState[T],
	pod *podResourceState[T],
	newMin T,
	newMax T,
) (verdict string) {
	if newMin == pod.Min && newMax == pod.Max {
		return fmt.Sprintf("limits unchanged (min = %d, max = %d)", newMin, newMax)
	}

	// If the maximum bound has changed, then we should update {node,pod}.Buffer based it so that we
	// can make a best-effort attempt to avoid overcommitting. This solution can't be perfect
	// (because we're intentionally not using the "hard" limits provided by NeonVM, which would be
	// overly conservative).
	// However. This solution should be *good enough* - the cases it protects against are already
	// exceptionally rare, and the imperfections even more so.
	//
	// To be clear, the cases we're worried about are things like the following sequence of events:
	//
	//   1. VM is at 4 CPU (of max 4)
	//   2. Scheduler dies, autoscaler-agent loses contact
	//   3. autoscaler-agent downscales to 2 CPU
	//   3. VM Cpu.Max gets set to 2 (autoscaler-agent misses this)
	//   4. Scheduler appears, observes Cpu.Max = 2
	//   5. VM Cpu.Max gets set to 4
	//   6. autoscaler-agent observes Cpu.Max is still 4
	//   7. autoscaler-agent scales VM up to 4 CPU, which it is able to do because a previous
	//      scheduler approved 4 CPU.
	//   <-- INCONSISTENT STATE -->
	//   8. autoscaler-agent reconnects with scheduler, informing it that it's using 4 CPU
	//
	// Again: we can't handle this perfectly with the current system. However, a good best-effort
	// attempt to prevent this is worthwhile here. (realistically, the things we can't prevent would
	// require a "perfect storm" of other failures in order to be relevant - which is good!)
	bufferVerdict := ""
	updateBuffer := pod.Max != newMax
	if updateBuffer {
		oldPodBuffer := pod.Buffer
		oldNodeBuffer := node.Buffer
		oldPodReserved := pod.Reserved
		oldNodeReserved := node.Reserved

		// Recalculate Reserved and Buffer from scratch because it's easier than doing the math
		// directly.
		//
		// Note that we don't want to reserve *below* what we think the VM is using if the bounds
		// decrease; it may be that the autoscaler-agent has not yet reacted to that.
		using := pod.Reserved - pod.Buffer
		pod.Reserved = util.Max(newMax, using)
		pod.Buffer = pod.Reserved - using

		node.Reserved = node.Reserved + pod.Reserved - oldPodReserved
		node.Buffer = node.Buffer + pod.Buffer - oldPodBuffer

		bufferVerdict = fmt.Sprintf(
			". no contact yet: pod reserved %d -> %d (buffer %d -> %d), node reserved %d -> %d (buffer %d -> %d)",
			oldPodReserved, pod.Reserved, oldPodBuffer, pod.Buffer,
			oldNodeReserved, node.Reserved, oldNodeBuffer, node.Buffer,
		)
	}

	oldMin := pod.Min
	oldMax := pod.Max

	pod.Min = newMin
	pod.Max = newMax

	return fmt.Sprintf("updated min %d -> %d, max %d -> %d%s", oldMin, newMin, oldMax, newMax, bufferVerdict)
}
