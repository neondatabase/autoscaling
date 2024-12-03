package state_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/plugin/state"
	"github.com/neondatabase/autoscaling/pkg/util"
)

const defaultWatermarkFraction = 0.8

func podUID(id int) types.UID {
	return types.UID(fmt.Sprintf("pod-uid-%d", id))
}

func fixedPod(id int, cpu vmv1.MilliCPU, mem api.Bytes) state.Pod {
	// Jan 1 2000 at 00:00:00 UTC
	createdAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

	return state.Pod{
		NamespacedName: util.NamespacedName{
			Name:      fmt.Sprintf("pod-name-%d", id),
			Namespace: "test-namespace",
		},
		UID:            podUID(id),
		CreatedAt:      createdAt,
		VirtualMachine: lo.Empty[util.NamespacedName](),
		Migratable:     false,
		AlwaysMigrate:  false,
		Migrating:      false,
		CPU: state.PodResources[vmv1.MilliCPU]{
			Reserved:  cpu,
			Requested: cpu,
			Factor:    0,
		},
		Mem: state.PodResources[api.Bytes]{
			Reserved:  mem,
			Requested: mem,
			Factor:    0,
		},
	}
}

func TestBasicNodeOperations(t *testing.T) {
	cpu := vmv1.MilliCPU(1000)
	gib := api.Bytes(1024 * 1024 * 1024)

	node := state.NodeStateFromParams(
		"node-1",
		10*cpu,
		40*gib,
		defaultWatermarkFraction,
		map[string]string{},
	)

	node.AddPod(fixedPod(1, 2*cpu, 8*gib))
	node.AddPod(fixedPod(2, 1*cpu, 4*gib))
	assert.Equal(t, state.NodeResources[vmv1.MilliCPU]{
		Total:     10 * cpu,
		Reserved:  3 * cpu,
		Watermark: 8 * cpu,
		Migrating: 0,
	}, node.CPU)
	assert.Equal(t, state.NodeResources[api.Bytes]{
		Total:     40 * gib,
		Reserved:  12 * gib,
		Watermark: 32 * gib,
		Migrating: 0,
	}, node.Mem)

	node.RemovePod(podUID(2))

	assert.Equal(t, state.NodeResources[vmv1.MilliCPU]{
		Total:     10 * cpu,
		Reserved:  2 * cpu,
		Watermark: 8 * cpu,
		Migrating: 0,
	}, node.CPU)
	assert.Equal(t, state.NodeResources[api.Bytes]{
		Total:     40 * gib,
		Reserved:  8 * gib,
		Watermark: 32 * gib,
		Migrating: 0,
	}, node.Mem)
}

func TestSpeculativeNodeOperations(t *testing.T) {
	cpu := vmv1.MilliCPU(1000)
	gib := api.Bytes(1024 * 1024 * 1024)

	node := state.NodeStateFromParams(
		"node-1",
		10*cpu,
		40*gib,
		defaultWatermarkFraction,
		map[string]string{},
	)

	// add a single pod to start with
	node.AddPod(fixedPod(1, 2*cpu, 8*gib))
	assert.Equal(t, state.NodeResources[vmv1.MilliCPU]{
		Total:     10 * cpu,
		Reserved:  2 * cpu,
		Watermark: 8 * cpu,
		Migrating: 0,
	}, node.CPU)
	assert.Equal(t, state.NodeResources[api.Bytes]{
		Total:     40 * gib,
		Reserved:  8 * gib,
		Watermark: 32 * gib,
		Migrating: 0,
	}, node.Mem)

	// try out removing a pod + adding a new one, but don't go through with it
	modifyNode := func(n *state.Node) {
		n.RemovePod(podUID(1))
		n.AddPod(fixedPod(2, 3*cpu, 12*gib))
	}
	node.Speculatively(func(n *state.Node) (commit bool) {
		modifyNode(n)
		return false
	})
	// Check that the changes were not made:
	assert.Equal(t, true, ok(node.GetPod(podUID(1))))
	assert.Equal(t, false, ok(node.GetPod(podUID(2))))
	assert.Equal(t, state.NodeResources[vmv1.MilliCPU]{
		Total:     10 * cpu,
		Reserved:  2 * cpu,
		Watermark: 8 * cpu,
		Migrating: 0,
	}, node.CPU)
	assert.Equal(t, state.NodeResources[api.Bytes]{
		Total:     40 * gib,
		Reserved:  8 * gib,
		Watermark: 32 * gib,
		Migrating: 0,
	}, node.Mem)

	// same as before, but actually do it
	node.Speculatively(func(n *state.Node) (commit bool) {
		modifyNode(n)
		return true
	})
	// Check that the changes were made:
	assert.Equal(t, false, ok(node.GetPod(podUID(1))))
	assert.Equal(t, true, ok(node.GetPod(podUID(2))))
	assert.Equal(t, state.NodeResources[vmv1.MilliCPU]{
		Total:     10 * cpu,
		Reserved:  3 * cpu,
		Watermark: 8 * cpu,
		Migrating: 0,
	}, node.CPU)
	assert.Equal(t, state.NodeResources[api.Bytes]{
		Total:     40 * gib,
		Reserved:  12 * gib,
		Watermark: 32 * gib,
		Migrating: 0,
	}, node.Mem)
}

func TestPodReconciling(t *testing.T) {
	type node struct {
		cpu vmv1.MilliCPU
		mem api.Bytes
	}

	type resources[T any] struct {
		reserved  T
		requested T
	}

	type pod struct {
		cpu resources[vmv1.MilliCPU]
		mem resources[api.Bytes]
	}

	cpu := vmv1.MilliCPU(1000)
	gib := api.Bytes(1024 * 1024 * 1024)

	factorCPU := cpu
	factorMem := 4 * gib

	makePod := func(p pod) state.Pod {
		// Jan 1 2000 at 00:00:00 UTC
		createdAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

		return state.Pod{
			NamespacedName: util.NamespacedName{
				Name:      "pod-name",
				Namespace: "test-namespace",
			},
			UID:       "pod-uid",
			CreatedAt: createdAt,
			VirtualMachine: util.NamespacedName{
				Name:      "vm-name",
				Namespace: "test-namespace",
			},
			Migratable:    false,
			AlwaysMigrate: false,
			Migrating:     false,
			CPU: state.PodResources[vmv1.MilliCPU]{
				Reserved:  p.cpu.reserved,
				Requested: p.cpu.requested,
				Factor:    factorCPU,
			},
			Mem: state.PodResources[api.Bytes]{
				Reserved:  p.mem.reserved,
				Requested: p.mem.requested,
				Factor:    factorMem,
			},
		}
	}

	cases := []struct {
		name       string
		nodeTotals node
		nodeBefore node
		podBefore  pod
		done       bool
		nodeAfter  node
		podAfter   pod
	}{
		{
			name: "normal-pod-nothing-to-do",
			nodeTotals: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			nodeBefore: node{
				cpu: 3 * factorCPU,
				mem: 3 * factorMem,
			},
			podBefore: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  3 * factorCPU,
					requested: 3 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  3 * factorMem,
					requested: 3 * factorMem,
				},
			},
			done: true,
			nodeAfter: node{
				cpu: 3 * factorCPU,
				mem: 3 * factorMem,
			},
			podAfter: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  3 * factorCPU,
					requested: 3 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  3 * factorMem,
					requested: 3 * factorMem,
				},
			},
		},
		{
			// Pod that upscales from 3 -> 5 CU
			name: "upscale-full",
			nodeTotals: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			nodeBefore: node{
				cpu: 3 * factorCPU,
				mem: 3 * factorMem,
			},
			podBefore: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  3 * factorCPU,
					requested: 5 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  3 * factorMem,
					requested: 5 * factorMem,
				},
			},
			done: true,
			nodeAfter: node{
				cpu: 5 * factorCPU,
				mem: 5 * factorMem,
			},
			podAfter: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  5 * factorCPU,
					requested: 5 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  5 * factorMem,
					requested: 5 * factorMem,
				},
			},
		},
		{
			// Pod that upscales from 3 -> 5 CU, exactly landing at the node limits
			name: "upscale-full-exact",
			nodeTotals: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			nodeBefore: node{
				cpu: 8 * factorCPU,
				mem: 8 * factorMem,
			},
			podBefore: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  3 * factorCPU,
					requested: 5 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  3 * factorMem,
					requested: 5 * factorMem,
				},
			},
			done: true,
			nodeAfter: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			podAfter: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  5 * factorCPU,
					requested: 5 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  5 * factorMem,
					requested: 5 * factorMem,
				},
			},
		},
		{
			// Pod that wants to upscale from 3 -> 5 CU, but the node is too full
			name: "upscale-partial",
			nodeTotals: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			nodeBefore: node{
				cpu: 9 * factorCPU,
				mem: 9 * factorMem,
			},
			podBefore: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  3 * factorCPU,
					requested: 5 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  3 * factorMem,
					requested: 5 * factorMem,
				},
			},
			done: false,
			nodeAfter: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			podAfter: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  4 * factorCPU,
					requested: 5 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  4 * factorMem,
					requested: 5 * factorMem,
				},
			},
		},
		{
			// Pod that wants to upscale from 3 -> 5 CU, but can't make any progress because the
			// node is already completely full
			name: "upscale-blocked",
			nodeTotals: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			nodeBefore: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			podBefore: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  3 * factorCPU,
					requested: 5 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  3 * factorMem,
					requested: 5 * factorMem,
				},
			},
			done: false,
			nodeAfter: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			podAfter: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  3 * factorCPU,
					requested: 5 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  3 * factorMem,
					requested: 5 * factorMem,
				},
			},
		},
		{
			// Pod that wants to upscale from 3 -> 5 CU, but the node is too full to completely
			// satisfy. AND the node won't end up exactly full.
			name: "upscale-blocked",
			nodeTotals: node{
				cpu: 10*factorCPU + (factorCPU / 2),
				mem: 10*factorMem + (factorMem / 4),
			},
			nodeBefore: node{
				cpu: 9 * factorCPU,
				mem: 9 * factorMem,
			},
			podBefore: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  3 * factorCPU,
					requested: 5 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  3 * factorMem,
					requested: 5 * factorMem,
				},
			},
			done: false,
			nodeAfter: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			podAfter: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  4 * factorCPU,
					requested: 5 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  4 * factorMem,
					requested: 5 * factorMem,
				},
			},
		},
		{
			// Pod that doesn't want anything, and the node is over-full
			name: "overfull-nothing-to-do",
			nodeTotals: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			nodeBefore: node{
				cpu: 11 * factorCPU,
				mem: 11 * factorMem,
			},
			podBefore: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  3 * factorCPU,
					requested: 3 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  3 * factorMem,
					requested: 3 * factorMem,
				},
			},
			done: true,
			nodeAfter: node{
				cpu: 11 * factorCPU,
				mem: 11 * factorMem,
			},
			podAfter: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  3 * factorCPU,
					requested: 3 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  3 * factorMem,
					requested: 3 * factorMem,
				},
			},
		},
		{
			// Pod that wants to downscale, and the node is over-full before & after
			name: "overfull-downscale",
			nodeTotals: node{
				cpu: 10 * factorCPU,
				mem: 10 * factorMem,
			},
			nodeBefore: node{
				cpu: 12 * factorCPU,
				mem: 12 * factorMem,
			},
			podBefore: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  4 * factorCPU,
					requested: 3 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  4 * factorMem,
					requested: 3 * factorMem,
				},
			},
			done: true,
			nodeAfter: node{
				cpu: 11 * factorCPU,
				mem: 11 * factorMem,
			},
			podAfter: pod{
				cpu: resources[vmv1.MilliCPU]{
					reserved:  3 * factorCPU,
					requested: 3 * factorCPU,
				},
				mem: resources[api.Bytes]{
					reserved:  3 * factorMem,
					requested: 3 * factorMem,
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			makeNode := func(reservedCPU vmv1.MilliCPU, reservedMem api.Bytes) *state.Node {
				n := state.NodeStateFromParams(
					"node-name",
					c.nodeTotals.cpu,
					c.nodeTotals.mem,
					defaultWatermarkFraction,
					map[string]string{},
				)
				n.CPU.Reserved = reservedCPU
				n.Mem.Reserved = reservedMem
				return n
			}

			node := makeNode(
				c.nodeBefore.cpu-c.podBefore.cpu.reserved,
				c.nodeBefore.mem-c.podBefore.mem.reserved,
			)
			node.AddPod(makePod(c.podBefore))

			// Check that the initial node equals what we expect
			nodeBefore := makeNode(c.nodeBefore.cpu, c.nodeBefore.mem)
			assert.Equal(t, nodeBefore.CPU, node.CPU)
			assert.Equal(t, nodeBefore.Mem, node.Mem)

			// Do the reconciling
			p, ok := node.GetPod("pod-uid")
			assert.Equal(t, true, ok)
			done := node.ReconcilePodReserved(&p)

			// Check the pod matches what we expect
			assert.Equal(t, makePod(c.podAfter), p)
			// ... and, check that the modified pod *and* the pod in the node still match.
			p, ok = node.GetPod("pod-uid")
			assert.Equal(t, true, ok)
			assert.Equal(t, makePod(c.podAfter), p)
			// + check whether we're done matches:
			assert.Equal(t, c.done, done)
			// + check the node:
			nodeAfter := makeNode(c.nodeAfter.cpu, c.nodeAfter.mem)
			assert.Equal(t, nodeAfter.CPU, node.CPU)
			assert.Equal(t, nodeAfter.Mem, node.Mem)
		})
	}
}
