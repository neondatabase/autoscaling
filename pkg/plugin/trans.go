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

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type resourceTransition[T constraints.Unsigned] struct {
	node    *nodeResourceState[T]
	oldNode struct {
		reserved             T
		buffer               T
		capacityPressure     T
		pressureAccountedFor T
	}
	pod    *podResourceState[T]
	oldPod struct {
		reserved         T
		buffer           T
		capacityPressure T
	}
}

func collectResourceTransition[T constraints.Unsigned](
	node *nodeResourceState[T],
	pod *podResourceState[T],
) resourceTransition[T] {
	return resourceTransition[T]{
		node: node,
		oldNode: struct {
			reserved             T
			buffer               T
			capacityPressure     T
			pressureAccountedFor T
		}{
			reserved:             node.Reserved,
			buffer:               node.Buffer,
			capacityPressure:     node.CapacityPressure,
			pressureAccountedFor: node.PressureAccountedFor,
		},
		pod: pod,
		oldPod: struct {
			reserved         T
			buffer           T
			capacityPressure T
		}{
			reserved:         pod.Reserved,
			buffer:           pod.Buffer,
			capacityPressure: pod.CapacityPressure,
		},
	}
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

// handleRequested updates r.pod and r.node with changes to match the requested resources, within
// what's possible given the remaining resources.
//
// A pretty-formatted summary of the outcome is returned as the verdict, for logging.
func (r resourceTransition[T]) handleRequested(requested T, startingMigration bool, onlyThousands bool) (verdict string) {
	totalReservable := r.node.Total
	// note: it's possible to temporarily have reserved > totalReservable, after loading state or
	// config change; we have to use SaturatingSub here to account for that.
	remainingReservable := util.SaturatingSub(totalReservable, r.oldNode.reserved)

	// Note: The correctness of this function depends on the autoscaler-agents and previous
	// scheduler being well-behaved. This function will fail to prevent overcommitting when:
	//
	//  1. A previous scheduler allowed more than it should have, and the autoscaler-agents are
	//     sticking to those resource levels; or
	//  2. autoscaler-agents are making an initial communication that requests an increase from
	//     their current resource allocation
	//
	// So long as neither of the above happen, we should be ok, because we'll inevitably lower the
	// "buffered" amount until all pods for this node have contacted us, and the reserved amount
	// should then be back under r.node.total. It _may_ still be above totalReservable, but that's
	// expected to happen sometimes!

	if requested <= r.pod.Reserved {
		// Decrease "requests" are actually just notifications it's already happened
		r.node.Reserved -= r.pod.Reserved - requested
		r.pod.Reserved = requested
		// pressure is now zero, because the pod no longer wants to increase resources.
		r.pod.CapacityPressure = 0
		r.node.CapacityPressure -= r.oldPod.capacityPressure

		// use shared verdict below.

	} else if startingMigration /* implied: && requested > r.pod.reserved */ {
		// Can't increase during migrations.
		//
		// But we _will_ add the pod's request to the node's pressure, noting that its migration
		// will resolve it.
		r.pod.CapacityPressure = requested - r.pod.Reserved
		r.node.CapacityPressure = r.node.CapacityPressure + r.pod.CapacityPressure - r.oldPod.capacityPressure

		// note: we don't need to handle buffer here because migration is never started as the first
		// communication, so buffers will be zero already.
		if r.pod.Buffer != 0 {
			panic(errors.New("r.pod.buffer != 0"))
		}

		fmtString := "Denying increase %d -> %d because the pod is starting migration; " +
			"node capacityPressure %d -> %d (%d -> %d spoken for)"
		verdict = fmt.Sprintf(
			fmtString,
			// Denying increase %d -> %d because ...
			r.oldPod.reserved, requested,
			// node capacityPressure %d -> %d (%d -> %d spoken for)
			r.oldNode.capacityPressure, r.node.CapacityPressure, r.oldNode.pressureAccountedFor, r.node.PressureAccountedFor,
		)
		return verdict
	} else /* typical "request for increase" */ {
		// The following comment was made 2022-11-28 (updated 2023-04-06):
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

		increase := requested - r.pod.Reserved
		// Increases are bounded by what's left in the node
		maxIncrease := remainingReservable
		// if only in thousands, round down maxIncrease to nearest multiple of 1000
		if onlyThousands {
			thousand := T(100) * 10 // avoid compiler complaining about 1000 > maximum int8
			maxIncrease = (maxIncrease / thousand) * thousand
		}
		if increase > maxIncrease /* increases are bound by what's left in the node */ {
			r.pod.CapacityPressure = increase - maxIncrease
			// adjust node pressure accordingly. We can have old < new or new > old, so we shouldn't
			// directly += or -= (implicitly relying on overflow).
			r.node.CapacityPressure = r.node.CapacityPressure - r.oldPod.capacityPressure + r.pod.CapacityPressure
			increase = maxIncrease // cap at maxIncrease.
		} else {
			// If we're not capped by maxIncrease, relieve pressure coming from this pod
			r.node.CapacityPressure -= r.pod.CapacityPressure
			r.pod.CapacityPressure = 0
		}
		r.pod.Reserved += increase
		r.node.Reserved += increase

		// use shared verdict below.
	}

	fmtString := "Register %d%s -> %d%s (pressure %d -> %d); " +
		"node reserved %d%s -> %d%s (of %d), " +
		"node capacityPressure %d -> %d (%d -> %d spoken for)"

	var podBuffer string
	var oldNodeBuffer string
	var newNodeBuffer string
	if r.pod.Buffer != 0 {
		podBuffer = fmt.Sprintf(" [buffer %d]", r.pod.Buffer)
		oldNodeBuffer = fmt.Sprintf(" [buffer %d]", r.oldNode.buffer)

		r.node.Buffer -= r.pod.Buffer
		r.pod.Buffer = 0

		newNodeBuffer = fmt.Sprintf(" [buffer %d]", r.node.Buffer)
	}

	var wanted string
	if r.pod.Reserved != requested {
		wanted = fmt.Sprintf(" (wanted %d)", requested)
	}

	verdict = fmt.Sprintf(
		fmtString,
		// Register %d%s -> %d%s (pressure %d -> %d)
		r.oldPod.reserved, podBuffer, r.pod.Reserved, wanted, r.oldPod.capacityPressure, r.pod.CapacityPressure,
		// node reserved %d%s -> %d%s (of %d)
		r.oldNode.reserved, oldNodeBuffer, r.node.Reserved, newNodeBuffer, totalReservable,
		// node capacityPressure %d -> %d (%d -> %d spoken for)
		r.oldNode.capacityPressure, r.node.CapacityPressure, r.oldNode.pressureAccountedFor, r.node.PressureAccountedFor,
	)
	return verdict
}

// handleDeleted updates r.node with changes to match the removal of r.pod
//
// A pretty-formatted summary of the changes is returned as the verdict, for logging.
func (r resourceTransition[T]) handleDeleted(currentlyMigrating bool) (verdict string) {
	r.node.Reserved -= r.pod.Reserved
	r.node.CapacityPressure -= r.pod.CapacityPressure

	if currentlyMigrating {
		r.node.PressureAccountedFor -= r.pod.Reserved + r.pod.CapacityPressure
	}

	fmtString := "pod had %d; node reserved %d -> %d, " +
		"node capacityPressure %d -> %d (%d -> %d spoken for)"
	verdict = fmt.Sprintf(
		fmtString,
		// pod had %d; node reserved %d -> %d
		r.pod.Reserved, r.oldNode.reserved, r.node.Reserved,
		// node capacityPressure %d -> %d (%d -> %d spoken for)
		r.oldNode.capacityPressure, r.node.CapacityPressure, r.oldNode.pressureAccountedFor, r.node.PressureAccountedFor,
	)
	return verdict
}

func (r resourceTransition[T]) handleNonAutoscalingUsageChange(newUsage T) (verdict string) {
	diff := newUsage - r.pod.Reserved
	r.pod.Reserved = newUsage
	r.node.Reserved += diff
	verdict = fmt.Sprintf(
		"pod reserved (%v -> %v), node reserved (%v -> %v)",
		r.oldPod.reserved, r.pod.Reserved, r.oldNode.reserved, r.node.Reserved,
	)
	return verdict
}

// handleAutoscalingDisabled updates r.node with changes to clear any buffer and capacityPressure
// from r.pod
//
// A pretty-formatted summary of the changes is returned as the verdict, for logging.
func (r resourceTransition[T]) handleAutoscalingDisabled() (verdict string) {
	// buffer is included in reserved, so we reduce everything by buffer.
	buffer := r.pod.Buffer
	valuesToReduce := []*T{&r.node.Reserved, &r.node.Buffer, &r.pod.Reserved, &r.pod.Buffer}
	for _, v := range valuesToReduce {
		*v -= buffer
	}

	r.node.CapacityPressure -= r.pod.CapacityPressure
	r.pod.CapacityPressure = 0

	var nodeBufferChange string
	if r.oldPod.buffer != 0 {
		nodeBufferChange = fmt.Sprintf(" [buffer %d -> %d]", r.oldNode.buffer, r.node.Buffer)
	}

	fmtString := "pod had buffer %d, capacityPressure %d; " +
		"node reserved %d -> %d%s, capacityPressure %d -> %d"
	verdict = fmt.Sprintf(
		fmtString,
		// pod had buffer %d, capacityPressure %d;
		r.oldPod.buffer, r.oldPod.capacityPressure,
		// node reserved %d -> %d%s, capacityPressure %d -> %d
		r.oldNode.reserved, r.node.Reserved, nodeBufferChange, r.oldNode.capacityPressure, r.node.CapacityPressure,
	)
	return verdict
}

// handleStartMigration updates r.node with changes to clear any buffer and capacityPressure from
// r.pod.
//
// If the pod is the migration source, this method *also* increases the node's PressureAccountedFor
// to match the pod's resource usage.
func (r resourceTransition[T]) handleStartMigration(source bool) (verdict string) {
	// This method is basically the same as handleAutoscalingDisabled, except we also update the
	// node's PressureAccountedFor because any pressure generated by the pod will be resolved once
	// the migration completes and the pod gets deleted.

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
		r.oldPod.buffer, r.oldPod.capacityPressure,
		// node reserved %d -> %d, capacityPressure %d -> %d
		r.oldNode.reserved, r.node.Reserved, r.oldNode.capacityPressure, r.node.CapacityPressure, r.oldNode.pressureAccountedFor, r.node.PressureAccountedFor,
	)
	return verdict
}

func handleUpdatedLimits[T constraints.Unsigned](
	node *nodeResourceState[T],
	pod *podResourceState[T],
	receivedContact bool,
	newMin T,
	newMax T,
) (verdict string) {
	if newMin == pod.Min && newMax == pod.Max {
		return fmt.Sprintf("limits unchanged (min = %d, max = %d)", newMin, newMax)
	}

	// if we haven't yet been contacted by the autoscaler-agent, then we should update
	// {node,pod}.Buffer based on the change in the maximum bound so that we can make a best-effort
	// attempt to avoid overcommitting. This solution can't be perfect (because we're intentionally
	// not using the "hard" limits provided by NeonVM, which would be overly conservative).
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
	updateBuffer := !receivedContact && pod.Max != newMax
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

// handleDeletedPod is kind of like handleDeleted, except that it returns both verdicts side by
// side instead of being generic over the resource
func handleDeletedPod(
	node *nodeState,
	pod podOtherResourceState,
	memSlotSize *resource.Quantity,
) (cpuVerdict string, memVerdict string) {

	oldRes := node.otherResources
	newRes := node.otherResources.subPod(memSlotSize, pod)
	node.otherResources = newRes

	oldNodeCpuReserved := node.vCPU.Reserved
	oldNodeMemReserved := node.memSlots.Reserved

	node.vCPU.Reserved = node.vCPU.Reserved - oldRes.ReservedCPU + newRes.ReservedCPU
	node.memSlots.Reserved = node.memSlots.Reserved - oldRes.ReservedMemSlots + newRes.ReservedMemSlots

	cpuVerdict = fmt.Sprintf(
		"pod had %v (raw), node otherPods (%v -> %v raw, %v margin, %d -> %d rounded), node reserved %d -> %d",
		&pod.RawCPU, &oldRes.RawCPU, &newRes.RawCPU, newRes.MarginCPU, oldRes.ReservedCPU, newRes.ReservedCPU,
		oldNodeCpuReserved, node.vCPU.Reserved,
	)
	memVerdict = fmt.Sprintf(
		"pod had %v (raw), node otherPods (%v -> %v raw, %v margin, %d -> %d slots), node reserved %d -> %d slots",
		&pod.RawMemory, &oldRes.RawMemory, &newRes.RawMemory, newRes.MarginMemory, oldRes.ReservedMemSlots, newRes.ReservedMemSlots,
		oldNodeMemReserved, node.memSlots.Reserved,
	)

	return
}
