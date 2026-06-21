package engine

import (
	"fmt"

	"github.com/valbaudo/awf/ir"
)

// ComputeVerifyingTraceTarget returns the top-level rerun target for a resume
// against an EDITED definition whose change is confined to node bodies: the
// earliest-slot committed node that cannot be reused, or "" if every committed
// node is reusable.
//
// A committed path is reusable when all of:
//   - rerunSupported(p) == nil (v1: top-level or nested inside parallels only)
//   - the static node at p is a *ir.CodeStep
//   - rs.Completed[p].NodeSubtreeDigest != "" (non-legacy record)
//   - ir.NodeSubtreeDigest(static[p]) matches the recorded value
//
// If a committed path's top-level segment is not in the current workflow graph
// (addressing shift), an error is returned — the caller must hard-error.
//
// Pure function; no I/O.
func ComputeVerifyingTraceTarget(wf *ir.Workflow, rs *RunState) (string, error) {
	slots := rerunRootSlots(wf)

	// Build a static map from path to node via WalkNodes.
	static := make(map[string]ir.Node)
	ir.WalkNodes(wf.Graph, "", func(n ir.Node, path string) {
		static[path] = n
	})

	// Check all committed paths for addressing shifts first.
	committedPaths := allCommittedPaths(rs)
	for _, p := range committedPaths {
		seg := rerunFirstSegment(p)
		if _, ok := slots[seg]; !ok {
			return "", fmt.Errorf("verifying-trace: committed node %q has no top-level node %q in the current workflow (addressing shift)", p, seg)
		}
	}

	// reusable returns true iff the committed path can be skipped on verifying-trace resume.
	reusable := func(p string) bool {
		if rerunSupported(p) != nil {
			return false
		}
		n, ok := static[p]
		if !ok {
			return false
		}
		_, isCode := n.(*ir.CodeStep)
		if !isCode {
			return false
		}
		nr, committed := rs.Completed[p]
		if !committed {
			return false
		}
		if nr.NodeSubtreeDigest == "" {
			return false
		}
		currentDigest, err := ir.NodeSubtreeDigest(n)
		if err != nil {
			return false // conservative: treat error as non-reusable
		}
		return currentDigest == nr.NodeSubtreeDigest
	}

	// Find the earliest top-level slot of any non-reusable committed path.
	bestSlot := -1
	bestSeg := ""
	for _, p := range committedPaths {
		if reusable(p) {
			continue
		}
		seg := rerunFirstSegment(p)
		slot := slots[seg] // already validated above
		if bestSlot < 0 || slot < bestSlot {
			bestSlot = slot
			bestSeg = seg
		}
	}

	if bestSlot < 0 {
		// Every committed path is reusable — pure replay + run uncommitted remainder.
		return "", nil
	}

	// Return the top-level segment that has the smallest slot.
	// For a simple sequential workflow the seg IS the committed path, but for
	// containers like parallel[0] the seg is the container key.
	return bestSeg, nil
}
