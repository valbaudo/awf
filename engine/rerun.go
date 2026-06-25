package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// rerunFirstSegment returns the first '.'-delimited segment of a runtime path
// (e.g. "parallel[1].branchA" -> "parallel[1]", "s0" -> "s0").
func rerunFirstSegment(path string) string {
	if i := strings.IndexByte(path, '.'); i >= 0 {
		return path[:i]
	}
	return path
}

// rerunRootSlots maps each root-graph child's first-segment form to its slot
// index, so rootSlot(p) = rerunRootSlots(wf)[rerunFirstSegment(p)]. Built from the
// canonical ir.WalkNodes walk (routes every path through ir.PathFor): no-dot paths
// are exactly the top-level nodes, visited in declaration order, so a counter gives
// an order-preserving slot. Using the canonical walker (not a hand-rolled
// kind->"<kw>[i]" switch) respects "node addressing is one pure function". WalkNodes
// excludes *Skip; that only renumbers, which is order-preserving (the comparison
// needs relative order, not absolute slot).
func rerunRootSlots(wf *ir.Workflow) map[string]int {
	out := map[string]int{}
	i := 0
	ir.WalkNodes(wf.Graph, "", func(_ ir.Node, path string) {
		if !strings.Contains(path, ".") { // no dot <=> a top-level node
			out[path] = i
			i++
		}
	})
	return out
}

// rerunSupported reports whether `--from target` is in the v1 subset: every
// segment EXCEPT the last must be a "parallel[...]" container — i.e. the target is
// a top-level node or nested only inside parallels. Targets inside a
// call/loop/gate/try/if/map-body are refused (general happens-after deferred).
func rerunSupported(target string) error {
	segs := strings.Split(target, ".")
	for _, s := range segs[:len(segs)-1] {
		if !strings.HasPrefix(s, "parallel[") {
			return fmt.Errorf("--from %q is nested inside a non-parallel container (segment %q); v1 supports a top-level node or a parallel branch only", target, s)
		}
	}
	return nil
}

// allCommittedPaths is the union of keys across the nine path-keyed RunState
// indices (the candidate set for invalidation).
func allCommittedPaths(rs *RunState) []string {
	seen := map[string]struct{}{}
	for k := range rs.Completed {
		seen[k] = struct{}{}
	}
	for k := range rs.Branches {
		seen[k] = struct{}{}
	}
	for k := range rs.LoopIters {
		seen[k] = struct{}{}
	}
	for k := range rs.GateAttempts {
		seen[k] = struct{}{}
	}
	for k := range rs.ReactRounds {
		seen[k] = struct{}{}
	}
	for k := range rs.MapItems {
		seen[k] = struct{}{}
	}
	for k := range rs.CallStarted {
		seen[k] = struct{}{}
	}
	for k := range rs.SignalReceivedAt {
		seen[k] = struct{}{}
	}
	for k := range rs.SelectedSkills {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// lastFailedPath returns the Path of the trailing node.failed event, or "".
// node.failed carries the failed frontier node's runtime path on the EVENT
// (e.Path), not its payload, and Fold ignores it — recoverable only from events.
func lastFailedPath(events []state.Event) string {
	last := ""
	for _, e := range events {
		if e.Type == EventNodeFailed && e.Path != "" {
			last = e.Path
		}
	}
	return last
}

// ResolveRerunTarget resolves a --from argument to one node path, in priority:
// (1) an exact committed path; (2) a top-level graph node segment — a CONTAINER
// like "parallel[1]"/"map[3]" has no committed key of its own (only children), so
// it is matched against rerunRootSlots; (3) the trailing node.failed event path
// (a failed uncommitted frontier node, matched by exact path or bare trailing id);
// (4) a unique TRAILING segment (a bare step id, e.g. "merge" ->
// "parallel[0].merge"). A bare id shared by two committed paths (ids are unique
// only within a sibling list) is an error listing candidates.
func ResolveRerunTarget(wf *ir.Workflow, rs *RunState, events []state.Event, arg string) (string, error) {
	paths := allCommittedPaths(rs)
	for _, p := range paths {
		if p == arg {
			return p, nil
		}
	}
	if _, ok := rerunRootSlots(wf)[arg]; ok {
		return arg, nil
	}
	// WS-6a: a failed (uncommitted) frontier node has no committed key; accept it
	// if arg names the trailing node.failed path exactly or by its bare trailing id.
	if fp := lastFailedPath(events); fp != "" {
		if arg == fp || arg == fp[strings.LastIndexByte(fp, '.')+1:] {
			return fp, nil
		}
	}
	var matches []string
	for _, p := range paths {
		if p[strings.LastIndexByte(p, '.')+1:] == arg {
			matches = append(matches, p)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("--from %q matches no committed node (give a committed runtime path, a top-level node like parallel[0], or a unique step id)", arg)
	default:
		return "", fmt.Errorf("--from %q is ambiguous (%d committed nodes share that id; name one exactly): %s", arg, len(matches), strings.Join(matches, ", "))
	}
}

// ComputeRerunInvalidation returns the sorted set of committed paths to invalidate
// for `--from target`: target's subtree ∪ every committed path whose top-level root
// slot is greater than target's. Errors if target is unsupported (rerunSupported);
// its root segment isn't in the current graph; OR any committed path's top-level
// segment is absent from the current graph (a structure-changing edit — node
// removed/renamed, or a control node reordered so its "<kw>[N]" segment changed).
// `--from` bypasses the digest pin, so refusing loud beats silently dropping a
// node and replaying stale output. `--from` assumes a STRUCTURE-PRESERVING edit
// (bodies/scripts, not graph shape); a pure top-level STEP reorder is not refused
// (ids are position-independent) but is safe (re-runs per the new order).
func ComputeRerunInvalidation(wf *ir.Workflow, rs *RunState, target string) ([]string, error) {
	if err := rerunSupported(target); err != nil {
		return nil, err
	}
	slots := rerunRootSlots(wf)
	tSlot, ok := slots[rerunFirstSegment(target)]
	if !ok {
		return nil, fmt.Errorf("--from %q: top-level node %q not found in workflow", target, rerunFirstSegment(target))
	}
	committed := allCommittedPaths(rs)
	for _, p := range committed {
		if _, known := slots[rerunFirstSegment(p)]; !known {
			return nil, fmt.Errorf("--from: committed node %q has no top-level node %q in the current workflow — its top-level structure changed since the run started; --from cannot map committed steps onto it (revert the structural change, or start a fresh run)", p, rerunFirstSegment(p))
		}
	}
	var out []string
	for _, p := range committed {
		inSubtree := p == target || strings.HasPrefix(p, target+".")
		afterRoot := slots[rerunFirstSegment(p)] > tSlot
		if inSubtree || afterRoot {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// clearInvalidatedPaths deletes each path from the nine path-keyed RunState
// indices plus SessionRefs (path-keyed, unlike the name/container-keyed
// SnapshotRefs and Signals which are NOT cleared here). Caller is single-threaded
// (fold at resume-build; engine.Run before the poller/goroutines).
func clearInvalidatedPaths(rs *RunState, paths []string) {
	for _, p := range paths {
		delete(rs.Completed, p)
		delete(rs.Branches, p)
		delete(rs.LoopIters, p)
		delete(rs.GateAttempts, p)
		delete(rs.ReactRounds, p)
		delete(rs.MapItems, p)
		delete(rs.CallStarted, p)
		delete(rs.SignalReceivedAt, p)
		delete(rs.SelectedSkills, p)
		delete(rs.SessionRefs, p) // path-keyed (unlike SnapshotRefs); a re-run generate node drops its stale session
	}
}
