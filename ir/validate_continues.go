package ir

import (
	"fmt"
	"strings"
)

// validateContinues runs the AWF1026-AWF1031 pass over every AgentStep that declares
// continues:. It is a pure function of the static IR graph — each node's static path
// (ir.WalkNodes / ir.PathFor) already encodes its full scope chain (gate[i].generate,
// map[i].body, if[i].then, loop[i].body), and WalkNodes visits in document order. The
// rules MIRROR engine/scope.go stepRuntimePath (the nested-loop reject at scope.go:543,
// the gate/map cross-scope rejects) so "validation = addressability" is exact, but the
// pass calls into no engine code: a continues: link that passes this pass is guaranteed
// resolvable by stepRuntimePath at run time.
//
// NOTE: AWF1025 is reserved for the map-image-target-static-pin rule; continues:
// validation starts at AWF1026 (next free code after the existing AWF1025).
func validateContinues(ld *LoadedDefinition, c *collector) {
	wf := ld.Workflow

	// One WalkNodes pass builds: id → *AgentStep, id → static path, id → document-order
	// ordinal. WalkNodes visits parent-before-children in slice order, so the ordinal is
	// a topological document position usable for the precedes-in-document-order half of
	// the dominator rule (A.3.2). Last-write-wins on (validator-rejected, AWF1004) dup ids.
	agents := map[string]*AgentStep{}
	paths := map[string]string{}
	order := map[string]int{}
	ord := 0
	WalkNodes(wf.Graph, "", func(n Node, path string) {
		if v, ok := n.(*AgentStep); ok {
			agents[v.ID] = v
			paths[v.ID] = path
			order[v.ID] = ord
		}
		ord++
	})

	// A.3.3 — acyclic. Walk the continues chain from each agent step that has one;
	// a revisit of a node is a cycle. Reported once per detected cycle (at the entry
	// node's path), independently of A.3.2 so a multi-edge cycle is caught even when
	// one edge is also non-dominating. The walk is bounded by len(agents). All nodes
	// in a detected cycle are marked so subsequent entry nodes in the same cycle do
	// not re-report.
	cycleReported := map[string]bool{}
	WalkNodes(wf.Graph, "", func(n Node, srcPath string) {
		as, ok := n.(*AgentStep)
		if !ok || as.Continues == "" || cycleReported[as.ID] {
			return
		}
		// visited tracks the chain walked from this entry, in order.
		visited := []string{as.ID}
		visitedSet := map[string]bool{as.ID: true}
		cur := as.Continues
		for i := 0; i < len(agents) && cur != ""; i++ {
			next, exists := agents[cur]
			if !exists {
				break // unknown target; AWF1026 covers this
			}
			if visitedSet[next.ID] {
				// Cycle detected: report once at the entry node and mark all
				// nodes in the chain as reported so they don't re-fire.
				c.errf(srcPath, "AWF1028", catalog["AWF1028"])
				for _, id := range visited {
					cycleReported[id] = true
				}
				cycleReported[next.ID] = true
				return
			}
			visited = append(visited, next.ID)
			visitedSet[next.ID] = true
			cur = next.Continues
		}
	})

	// Walk continuing steps in document order so diagnostics emit at stable paths.
	WalkNodes(wf.Graph, "", func(n Node, srcPath string) {
		as, ok := n.(*AgentStep)
		if !ok || as.Continues == "" {
			return
		}
		evaluatorSource := inEvaluateBlock(srcPath)
		// A.3.1 — target exists and is an agent step.
		tgt, isAgent := agents[as.Continues]
		if !isAgent {
			c.errf(srcPath, "AWF1026", fmt.Sprintf("%s (continues: %q)", catalog["AWF1026"], as.Continues))
			return // every downstream rule needs a real target.
		}
		// A.3.5 — evaluator sources may use continues: only as source context evidence.
		// Direct or transitive targets inside gate.evaluate would feed evaluator-only
		// transcript turns back into a judge and violate gate independence.
		if evaluatorSource && continuesChainTouchesEvaluate(as.Continues, agents, paths) {
			c.errf(srcPath, "AWF1030", fmt.Sprintf("%s (continues: %q)", catalog["AWF1030"], as.Continues))
			return
		}
		// A.3.7 — concurrent parallel-sibling reject (race fix). A parallel child has a
		// BARE path (no branch label) under parallel[N] (ir.WalkNodes), so two distinct
		// direct children A, B of the same parallel[N] satisfy dominates(A,B): A's scope
		// prefix is a boundary prefix of B and A precedes B in document order. But
		// engine/parallel.go runs the children as CONCURRENT goroutines, so B's thread
		// assembly LookupCompleted(A) nondeterministically finds A committed or not — a
		// race. Checked BEFORE the AWF1027 emit (it is a strict sub-case of "passes the
		// dominator prefix test") so the specific AWF1032 diagnostic wins over the generic
		// AWF1027 one. The fan-out pattern (both branches continue a step OUTSIDE the
		// parallel) is NOT a sibling link and stays valid.
		if parallelSiblings(paths[as.Continues], srcPath) {
			c.errf(srcPath, "AWF1032", fmt.Sprintf("%s (continues: %q)", catalog["AWF1032"], as.Continues))
			return // unaddressable at run time; later rules are moot.
		}
		// A.3.2 — dominator (incl. multiplicity). T dominates S iff every scope enclosing
		// T also encloses S (T's enclosing-scope prefix is a path-boundary prefix of S) AND
		// T precedes S in document order. The static path encodes the full scope chain, so
		// this is exactly the set stepRuntimePath can resolve (gate/map/loop/if rejected when
		// they enclose T but not S; forward/self refs rejected by the order check).
		if !dominates(paths[as.Continues], srcPath, order[as.Continues], order[as.ID]) {
			c.errf(srcPath, "AWF1027", fmt.Sprintf("%s (continues: %q)", catalog["AWF1027"], as.Continues))
			return // a non-dominating target can't be assembled; later rules are moot.
		}

		// A.3.6 — single loop scope (addressability). The TARGET's static path may contain
		// at most one loop[ segment. engine/scope.go stepRuntimePath rejects loopCount > 1;
		// this mirrors that check so "validation = addressability" is exact. The dominator
		// rule (AWF1027) counts enclosing scopes but not loop depth, so a target inside two
		// nested loops can pass AWF1027 yet be unresolvable at runtime — AWF1031 closes that gap.
		if strings.Count(paths[as.Continues], "loop[") > 1 {
			c.errf(srcPath, "AWF1031", fmt.Sprintf("%s (target %q)", catalog["AWF1031"], as.Continues))
		}

		// A.3.4 — same runtime. Normal conversation continuation keeps the existing
		// raw uses: comparison. Evaluator context evidence compares resolved base
		// adapter refs so two roles backed by the same adapter can share source context.
		if !continuesUsesCompatible(wf, as, tgt, evaluatorSource) {
			c.errf(srcPath, "AWF1029", fmt.Sprintf("%s (this step uses %q, target uses %q)", catalog["AWF1029"], as.Uses, tgt.Uses))
		}
	})
}

// dominates reports whether the target's static path tgtPath dominates the source's
// static path srcPath, given their document-order ordinals. Domination = (a) tgt's
// enclosing-scope prefix is a path-boundary prefix of src, so every scope enclosing tgt
// also encloses src (and they share the same if/else branch, gate side, map/loop body,
// etc.); and (b) tgt precedes src in document order (rejects forward + self refs). The
// id segment of tgt is stripped before the prefix test — a step does not "enclose" itself.
func dominates(tgtPath, srcPath string, tgtOrd, srcOrd int) bool {
	if tgtOrd >= srcOrd {
		return false
	}
	return hasPathPrefix(srcPath, scopePrefixOf(tgtPath))
}

// parallelSiblings reports whether tgtPath and srcPath are distinct direct children
// of the SAME parallel[N] block — i.e. they run concurrently (engine/parallel.go
// fans children out as goroutines), so the target is NOT guaranteed to have
// committed before the source's turn assembles its thread (a race). It mirrors the
// addressing fact that a parallel child diverges with NO branch label (ir.WalkNodes:
// "bare parallel[i] — no branch label"), unlike if (.then/.else) or map (.body):
// the two children's paths share the `parallel[N]` segment but the segment
// IMMEDIATELY following it differs.
//
// It walks both paths in lockstep to the first segment that differs. If the segment
// just BEFORE that divergence point is a `parallel[N]`, the two paths split into two
// different concurrent branches of that parallel → siblings (reject). Cases that
// must stay valid and return false here:
//   - the target is OUTSIDE the parallel (the fan-out pattern: both branches
//     continue a pre-fork ancestor) — there is no shared parallel[N] prefix, so the
//     divergence is at the top (preceding segment is "", not a parallel).
//   - the two steps are SEQUENTIAL within the SAME parallel child (the child is a
//     control node holding a sub-sequence): they share the child sub-path, so the
//     segment after parallel[N] is identical and the divergence (if any) is deeper,
//     after a non-parallel boundary.
//   - identical paths (a step is not its own concurrent sibling).
func parallelSiblings(tgtPath, srcPath string) bool {
	tgt := strings.Split(tgtPath, ".")
	src := strings.Split(srcPath, ".")
	n := len(tgt)
	if len(src) < n {
		n = len(src)
	}
	for i := 0; i < n; i++ {
		if tgt[i] == src[i] {
			continue
		}
		// First divergence at segment i. The shared parent boundary is segment i-1.
		// They are concurrent siblings iff that boundary is a parallel[N] segment
		// (so the two children carry bare, label-less, divergent paths under it).
		return i > 0 && strings.HasPrefix(tgt[i-1], "parallel[")
	}
	// No divergence within the shorter length: one path is a prefix of the other
	// (or they are identical) — same branch, not concurrent siblings.
	return false
}

// scopePrefixOf strips the trailing id segment from a step's static path, leaving the
// chain of enclosing scope segments ("" for a top-level step). For "gate[0].generate.ask"
// it returns "gate[0].generate"; for "draft" it returns "".
func scopePrefixOf(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[:i]
	}
	return ""
}

// hasPathPrefix reports whether prefix is a path-boundary prefix of path: either equal,
// or path == prefix + "." + rest. The "" prefix (top-level scope) matches everything.
// The boundary check stops "gate[0]" from matching "gate[01].x".
func hasPathPrefix(path, prefix string) bool {
	if prefix == "" {
		return true
	}
	return path == prefix || strings.HasPrefix(path, prefix+".")
}

// inEvaluateBlock reports whether a static path runs inside some gate's evaluate: list,
// i.e. it contains an "evaluate" segment immediately following a "gate[N]" segment.
// For example "gate[0].evaluate.judge" → true; "gate[0].generate.refine" → false.
func inEvaluateBlock(path string) bool {
	segs := strings.Split(path, ".")
	for i := 1; i < len(segs); i++ {
		if segs[i] == "evaluate" && strings.HasPrefix(segs[i-1], "gate[") {
			return true
		}
	}
	return false
}

func continuesChainTouchesEvaluate(id string, agents map[string]*AgentStep, paths map[string]string) bool {
	seen := map[string]bool{}
	for cur := id; cur != ""; {
		if seen[cur] {
			return false
		}
		seen[cur] = true
		if inEvaluateBlock(paths[cur]) {
			return true
		}
		step, ok := agents[cur]
		if !ok {
			return false
		}
		cur = step.Continues
	}
	return false
}

func resolvedBaseUses(wf *Workflow, uses string) string {
	if role, ok := wf.RoleByName(uses); ok && role.Uses != "" {
		return role.Uses
	}
	return uses
}

func continuesUsesCompatible(wf *Workflow, src, tgt *AgentStep, evaluatorSource bool) bool {
	if evaluatorSource {
		return resolvedBaseUses(wf, src.Uses) == resolvedBaseUses(wf, tgt.Uses)
	}
	return src.Uses == tgt.Uses
}
