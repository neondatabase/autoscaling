package plugin

// this file primarily contains the type resourceTransition[T], for handling a number of operations
// on resources, and pretty-formatting summaries of the operations.
//
// resourceTransitions are created with the collectResourceTransition function.
//
// Handling requested resources from the autoscaler-agent is done with the handleRequested method,
// and changes from VM deletion are handled by handleDeleted.

import (
	"fmt"
	"golang.org/x/exp/constraints"
)

type resourceTransition[T constraints.Unsigned] struct {
	node    *nodeResourceState[T]
	oldNode struct {
		reserved             T
		capacityPressure     T
		pressureAccountedFor T
	}
	pod    *podResourceState[T]
	oldPod struct {
		reserved         T
		capacityPressure T
	}
}

func collectResourceTransition[T constraints.Unsigned](
	node *nodeResourceState[T], pod *podResourceState[T],
) resourceTransition[T] {
	return resourceTransition[T]{
		node: node,
		oldNode: struct {
			reserved             T
			capacityPressure     T
			pressureAccountedFor T
		}{
			reserved:             node.reserved,
			capacityPressure:     node.capacityPressure,
			pressureAccountedFor: node.pressureAccountedFor,
		},
		pod: pod,
		oldPod: struct {
			reserved         T
			capacityPressure T
		}{
			reserved:         pod.reserved,
			capacityPressure: pod.capacityPressure,
		},
	}
}

// handleRequested udpates r.pod and r.node with changes to match the requested resources, within
// what's possible given the remaining resources.
//
// A pretty-formatted summary of the outcome is returned as the verdict, for logging.
func (r resourceTransition[T]) handleRequested(requested T, startingMigration bool) (verdict string) {
	totalReservable := r.node.total - r.node.system
	remainingReservable := totalReservable - r.oldNode.reserved

	if requested <= r.pod.reserved {
		// Decrease "requests" are actually just notifications it's already happened
		r.node.reserved -= r.pod.reserved - requested
		r.pod.reserved = requested
		// pressure is now zero, because the pod no longer wants to increase resources.
		r.pod.capacityPressure = 0
		r.node.capacityPressure -= r.oldPod.capacityPressure

		// use shared verdict below.

	} else if startingMigration /* implied: && requested > r.pod.reserved */ {
		// Can't increase during migrations.
		//
		// But we _will_ add the pod's request to the node's pressure, noting that its migration
		// will resolve it.
		r.pod.capacityPressure = requested - r.pod.reserved
		r.node.capacityPressure = r.node.capacityPressure + r.pod.capacityPressure - r.oldPod.capacityPressure

		fmtString := "Denying increase %d -> %d because the pod is starting migration; " +
			"node capacityPressure %d -> %d (%d -> %d spoken for)"
		verdict = fmt.Sprintf(
			fmtString,
			// Denying increase %d -> %d because ...
			r.oldPod.reserved, requested,
			// node capacityPressure %d -> %d (%d -> %d spoken for)
			r.oldNode.capacityPressure, r.node.capacityPressure, r.oldNode.pressureAccountedFor, r.node.pressureAccountedFor,
		)
		return verdict
	} else /* typical "request for increase" */ {
		// The following comment was made 2022-11-28:
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
		// the autoscaler-agent is still required to submit requests that are multiples of compute
		// units, so the system will *eventually* stabilize. This allows us to gracefully hande many
		// kinds of stressors. Handling the resources separately *from the scheduler's point of
		// view* makes it much, much easier to deal with.
		//
		// Please think carefully before changing this.

		increase := requested - r.pod.reserved
		// Increases are bounded by what's left in the node
		maxIncrease := remainingReservable
		if increase > maxIncrease /* increases are bound by what's left in the node */ {
			r.pod.capacityPressure = increase - maxIncrease
			// adjust node pressure accordingly. We can have old < new or new > old, so we shouldn't
			// directly += or -= (implicitly relying on overflow).
			r.node.capacityPressure = r.node.capacityPressure - r.oldPod.capacityPressure + r.pod.capacityPressure
			increase = maxIncrease // cap at maxIncrease.
		} else {
			// If we're not capped by maxIncrease, relieve pressure coming from this pod
			r.node.capacityPressure -= r.pod.capacityPressure
			r.pod.capacityPressure = 0
		}
		r.pod.reserved += increase
		r.node.reserved += increase

		// use shared verdict below.
	}

	fmtString := "Register %d -> %d%s (pressure %d -> %d); " +
		"node reserved %d -> %d (of %d), " +
		"node capacityPressure %d -> %d (%d -> %d spoken for)"

	var wanted string
	if r.pod.reserved != requested {
		wanted = fmt.Sprintf(" (wanted %d)", requested)
	}

	verdict = fmt.Sprintf(
		fmtString,
		// Register %d -> %d%s (pressure %d -> %d)
		r.oldPod.reserved, r.pod.reserved, wanted, r.oldPod.capacityPressure, r.pod.capacityPressure,
		// node reserved %d -> %d (of %d)
		r.oldNode.reserved, r.node.reserved, totalReservable,
		// node capacityPressure %d -> %d (%d -> %d spoken for)
		r.oldNode.capacityPressure, r.node.capacityPressure, r.oldNode.pressureAccountedFor, r.node.pressureAccountedFor,
	)
	return verdict
}

// handleDeleted updates r.node with changes to match the removal of r.pod
//
// A pretty-formatted summary of the changes is returned as the verdict, for logging.
func (r resourceTransition[T]) handleDeleted(currentlyMigrating bool) (verdict string) {
	r.node.reserved -= r.pod.reserved
	r.node.capacityPressure -= r.pod.capacityPressure

	if currentlyMigrating {
		r.node.pressureAccountedFor -= r.pod.reserved + r.pod.capacityPressure
	}

	fmtString := "pod had %d; node reserved %d -> %d, " +
		"node capacityPressure %d -> %d (%d -> %d spoken for)"
	verdict = fmt.Sprintf(
		fmtString,
		// pod had %d; node reserved %d -> %d
		r.pod.reserved, r.oldNode.reserved, r.node.reserved,
		// node capacityPressure %d -> %d (%d -> %d spoken for)
		r.oldNode.capacityPressure, r.node.capacityPressure, r.oldNode.pressureAccountedFor, r.node.pressureAccountedFor,
	)
	return verdict
}
