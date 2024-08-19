package plugin

// tests for the core logic in resourceTransitioner and related methods.

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
)

func Test_handleReserve(t *testing.T) {
	cases := []struct {
		name string

		podMin             uint
		podMax             uint
		nodeTotal          uint
		nodeReservedBefore uint
		nodeBufferBefore   uint

		overbudget bool
		verdict    string // set to empty to skip testing; otherwise it can be quite verbose...

		nodeReservedAfter uint
		nodeBufferAfter   uint
	}{
		{
			name: "SimpleWithinBudget",

			podMin:             4,
			podMax:             4,
			nodeTotal:          12,
			nodeReservedBefore: 6,
			nodeBufferBefore:   0,

			overbudget:        false,
			verdict:           "node reserved 6 + 4 -> 10 of total 12",
			nodeReservedAfter: 10,
			nodeBufferAfter:   0,
		},
		{
			name: "SimpleWithinBudgetWithBuffer",

			podMin:             4,
			podMax:             7,
			nodeTotal:          12,
			nodeReservedBefore: 4,
			nodeBufferBefore:   2,

			overbudget:        false,
			verdict:           "node reserved 4 [buffer 2] + 7 [buffer 3] -> 11 [buffer 5] of total 12",
			nodeReservedAfter: 11,
			nodeBufferAfter:   5,
		},
		{
			name: "ExactlyWithinBudget",

			podMin:             8,
			podMax:             8,
			nodeTotal:          12,
			nodeReservedBefore: 4,
			nodeBufferBefore:   0,

			overbudget:        false,
			verdict:           "node reserved 4 + 8 -> 12 of total 12",
			nodeReservedAfter: 12,
			nodeBufferAfter:   0,
		},
		{
			name: "BecomesOverbudget",

			podMin:             8,
			podMax:             8,
			nodeTotal:          12,
			nodeReservedBefore: 7,
			nodeBufferBefore:   0,

			overbudget:        true,
			verdict:           "node reserved 7 + 8 -> 15 of total 12",
			nodeReservedAfter: 15,
			nodeBufferAfter:   0,
		},
		{
			name: "OverbudgetOnlyWithBuffer",

			podMin:             4,
			podMax:             8,
			nodeTotal:          12,
			nodeReservedBefore: 7,
			nodeBufferBefore:   5,

			overbudget:        true,
			verdict:           "node reserved 7 [buffer 5] + 8 [buffer 4] -> 15 [buffer 9] of total 12",
			nodeReservedAfter: 15,
			nodeBufferAfter:   9,
		},
		{
			name: "AlreadyOverbudget",

			podMin:             8,
			podMax:             8,
			nodeTotal:          12,
			nodeReservedBefore: 16,
			nodeBufferBefore:   0,

			overbudget:        true,
			verdict:           "node reserved 16 + 8 -> 24 of total 12",
			nodeReservedAfter: 24,
			nodeBufferAfter:   0,
		},
		{
			name: "AlreadyOverbudgetWithBuffer",

			podMin:             4,
			podMax:             8,
			nodeTotal:          12,
			nodeReservedBefore: 16,
			nodeBufferBefore:   8,

			overbudget:        true,
			verdict:           "node reserved 16 [buffer 8] + 8 [buffer 4] -> 24 [buffer 12] of total 12",
			nodeReservedAfter: 24,
			nodeBufferAfter:   12,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nodeState := &nodeResourceState[uint]{
				Total:                c.nodeTotal,
				Watermark:            0,
				Reserved:             c.nodeReservedBefore,
				Buffer:               c.nodeBufferBefore,
				CapacityPressure:     0,
				PressureAccountedFor: 0,
			}
			podState := &podResourceState[uint]{
				Reserved:         c.podMax,
				Buffer:           c.podMax - c.podMin,
				CapacityPressure: 0,
				Min:              c.podMin,
				Max:              c.podMax,
			}

			rt := makeResourceTransitioner(nodeState, podState)

			overbudget, verdict := rt.handleReserve()

			assert.Equal(t, c.nodeReservedAfter, nodeState.Reserved)
			assert.Equal(t, c.nodeBufferAfter, nodeState.Buffer)
			assert.Equal(t, c.overbudget, overbudget)
			if c.verdict != "" {
				assert.Equal(t, c.verdict, verdict)
			}
		})
	}
}

// testing (resourceTransitioner[T]).handleRequested() for cases where migration is never involved
// (i.e. startingMigration is always false, and nodeResourceState.PressureAccountedFor is always 0).
func Test_handleRequested_nomigration(t *testing.T) {
	type node struct {
		reserved uint
		buffer   uint
		pressure uint
	}

	type pod struct {
		reserved uint
		buffer   uint
		pressure uint
	}

	cases := []struct {
		name string

		podMin     uint
		podMax     uint
		podBefore  pod
		nodeTotal  uint
		nodeBefore node

		requested  uint
		lastPermit *uint
		factor     uint

		verdict   string // set to empty to skip testing; otherwise it can be quite verbose...
		podAfter  pod
		nodeAfter node
	}{
		{
			name:   "SimpleUpscaleApprovalWithinLimits",
			podMin: 1,
			podMax: 4,
			podBefore: pod{
				reserved: 2,
				buffer:   0,
				pressure: 0,
			},
			nodeTotal: 10,
			nodeBefore: node{
				reserved: 5,
				buffer:   0,
				pressure: 0,
			},

			requested:  3,
			lastPermit: lo.ToPtr[uint](2),
			factor:     1,

			verdict: "Register 2 -> 3 (pressure 0 -> 0); node reserved 5 -> 6 (of 10), node capacityPressure 0 -> 0 (0 -> 0 spoken for)",
			podAfter: pod{
				reserved: 3,
				buffer:   0,
				pressure: 0,
			},
			nodeAfter: node{
				reserved: 6,
				buffer:   0,
				pressure: 0,
			},
		},
		{
			name:   "SimpleDownscaleApprovalWithinLimits",
			podMin: 1,
			podMax: 4,
			podBefore: pod{
				reserved: 3,
				buffer:   0,
				pressure: 0,
			},
			nodeTotal: 10,
			nodeBefore: node{
				reserved: 6,
				buffer:   0,
				pressure: 0,
			},

			requested:  2,
			lastPermit: lo.ToPtr[uint](3),
			factor:     1,

			verdict: "Register 3 -> 2 (pressure 0 -> 0); node reserved 6 -> 5 (of 10), node capacityPressure 0 -> 0 (0 -> 0 spoken for)",
			podAfter: pod{
				reserved: 2,
				buffer:   0,
				pressure: 0,
			},
			nodeAfter: node{
				reserved: 5,
				buffer:   0,
				pressure: 0,
			},
		},
		{
			name:   "SimpleNoopApproval",
			podMin: 1,
			podMax: 4,
			podBefore: pod{
				reserved: 2,
				buffer:   0,
				pressure: 0,
			},
			nodeTotal: 10,
			nodeBefore: node{
				reserved: 5,
				buffer:   0,
				pressure: 0,
			},

			requested:  2,
			lastPermit: lo.ToPtr[uint](2),
			factor:     1,

			verdict: "Register 2 -> 2 (pressure 0 -> 0); node reserved 5 -> 5 (of 10), node capacityPressure 0 -> 0 (0 -> 0 spoken for)",
			podAfter: pod{
				reserved: 2,
				buffer:   0,
				pressure: 0,
			},
			nodeAfter: node{
				reserved: 5,
				buffer:   0,
				pressure: 0,
			},
		},
		{
			name:   "NoopRemovesBuffer",
			podMin: 1,
			podMax: 4,
			podBefore: pod{
				reserved: 4,
				buffer:   2,
				pressure: 0,
			},
			nodeTotal: 10,
			nodeBefore: node{
				reserved: 7,
				buffer:   2,
				pressure: 0,
			},

			requested:  2,
			lastPermit: nil,
			factor:     1,

			verdict: "Register 4 [buffer 2] -> 2 (pressure 0 -> 0); node reserved 7 [buffer 2] -> 5 [buffer 0] (of 10), node capacityPressure 0 -> 0 (0 -> 0 spoken for)",
			podAfter: pod{
				reserved: 2,
				buffer:   0,
				pressure: 0,
			},
			nodeAfter: node{
				reserved: 5,
				buffer:   0,
				pressure: 0,
			},
		},
		{
			name:   "UpscaleApprovedUpToLimits",
			podMin: 1,
			podMax: 5,
			podBefore: pod{
				reserved: 1,
				buffer:   0,
				pressure: 0,
			},
			nodeTotal: 10,
			nodeBefore: node{
				reserved: 8,
				buffer:   0,
				pressure: 0,
			},

			requested:  5,
			lastPermit: lo.ToPtr[uint](1),
			factor:     1,

			verdict: "Register 1 -> 3 (wanted 5) (pressure 0 -> 2); node reserved 8 -> 10 (of 10), node capacityPressure 0 -> 2 (0 -> 0 spoken for)",
			podAfter: pod{
				reserved: 3,
				buffer:   0,
				pressure: 2,
			},
			nodeAfter: node{
				reserved: 10,
				buffer:   0,
				pressure: 2,
			},
		},
		{
			name:   "UpscaleRejectedAtLimitsWithUnevenFactor",
			podMin: 2,
			podMax: 8,
			podBefore: pod{
				reserved: 4,
				buffer:   0,
				pressure: 0,
			},
			nodeTotal: 10,
			nodeBefore: node{
				reserved: 7,
				buffer:   0,
				pressure: 0,
			},

			requested:  8,
			lastPermit: lo.ToPtr[uint](4),
			factor:     2,

			// FIXME: This is actually incorrect / not working. The request should be rejected with
			// only 6 reserved, because otherwise we end up over the total (as seen below).
			verdict: "",
			podAfter: pod{
				reserved: 8,
				buffer:   0,
				pressure: 0,
			},
			nodeAfter: node{
				reserved: 11,
				buffer:   0,
				pressure: 0,
			},
		},
		{
			name:   "UpscaleRejectedAboveLimits",
			podMin: 1,
			podMax: 4,
			podBefore: pod{
				reserved: 2,
				buffer:   0,
				pressure: 0,
			},
			nodeTotal: 10,
			nodeBefore: node{
				reserved: 12,
				buffer:   0,
				pressure: 0,
			},

			requested:  3,
			lastPermit: lo.ToPtr[uint](2),
			factor:     1,

			// FIXME: This is actually incorrect / not working. The request should be rejected with
			// only 2 reserved, because otherwise we end up over the total (as seen below).
			verdict: "",
			podAfter: pod{
				reserved: 3,
				buffer:   0,
				pressure: 0,
			},
			nodeAfter: node{
				reserved: 13,
				buffer:   0,
				pressure: 0,
			},
		},
		{
			name:   "DownscaleApprovedAboveLimits",
			podMin: 1,
			podMax: 5,
			podBefore: pod{
				reserved: 4,
				buffer:   0,
				pressure: 0,
			},
			nodeTotal: 10,
			nodeBefore: node{
				reserved: 15,
				buffer:   0,
				pressure: 0,
			},

			requested:  2,
			lastPermit: lo.ToPtr[uint](4),
			factor:     1,

			verdict: "Register 4 -> 2 (pressure 0 -> 0); node reserved 15 -> 13 (of 10), node capacityPressure 0 -> 0 (0 -> 0 spoken for)",
			podAfter: pod{
				reserved: 2,
				buffer:   0,
				pressure: 0,
			},
			nodeAfter: node{
				reserved: 13,
				buffer:   0,
				pressure: 0,
			},
		},
		{
			name:   "UpscaleApprovedUpToPermitAboveLimits",
			podMin: 1,
			podMax: 4,
			podBefore: pod{
				reserved: 2,
				buffer:   0,
				pressure: 0,
			},
			nodeTotal: 10,
			nodeBefore: node{
				reserved: 12,
				buffer:   0,
				pressure: 0,
			},

			requested:  3,
			lastPermit: lo.ToPtr[uint](3),
			factor:     1,

			verdict: "Register 2 -> 3 (pressure 0 -> 0); node reserved 12 -> 13 (of 10), node capacityPressure 0 -> 0 (0 -> 0 spoken for)",
			podAfter: pod{
				reserved: 3,
				buffer:   0,
				pressure: 0,
			},
			nodeAfter: node{
				reserved: 13,
				buffer:   0,
				pressure: 0,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ns := &nodeResourceState[uint]{
				Total:                c.nodeTotal,
				Watermark:            0,
				Reserved:             c.nodeBefore.reserved,
				Buffer:               c.nodeBefore.buffer,
				CapacityPressure:     c.podBefore.pressure,
				PressureAccountedFor: 0,
			}
			ps := &podResourceState[uint]{
				Reserved:         c.podBefore.reserved,
				Buffer:           c.podBefore.buffer,
				CapacityPressure: c.podBefore.pressure,
				Min:              c.podMin,
				Max:              c.podMax,
			}

			rt := makeResourceTransitioner(ns, ps)

			startingMigration := false
			verdict := rt.handleRequested(c.requested, c.lastPermit, startingMigration, c.factor)

			assert.Equal(t, c.podAfter.reserved, ps.Reserved)
			assert.Equal(t, c.podAfter.buffer, ps.Buffer)
			assert.Equal(t, c.podAfter.pressure, ps.CapacityPressure)
			assert.Equal(t, c.nodeAfter.reserved, ns.Reserved)
			assert.Equal(t, c.nodeAfter.buffer, ns.Buffer)
			assert.Equal(t, c.nodeAfter.pressure, ns.CapacityPressure)
			if c.verdict != "" {
				assert.Equal(t, c.verdict, verdict)
			}
		})
	}
}
