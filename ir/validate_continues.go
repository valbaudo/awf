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
		// A.3.1 — target exists and is an agent step.
		tgt, isAgent := agents[as.Continues]
		if !isAgent {
			c.errf(srcPath, "AWF1026", fmt.Sprintf("%s (continues: %q)", catalog["AWF1026"], as.Continues))
			return // every downstream rule needs a real target.
		}
		_ = tgt // used by later rules (2.4-2.6).

		// A.3.2 — dominator (incl. multiplicity). T dominates S iff every scope enclosing
		// T also encloses S (T's enclosing-scope prefix is a path-boundary prefix of S) AND
		// T precedes S in document order. The static path encodes the full scope chain, so
		// this is exactly the set stepRuntimePath can resolve (gate/map/loop/if rejected when
		// they enclose T but not S; forward/self refs rejected by the order check).
		if !dominates(paths[as.Continues], srcPath, order[as.Continues], order[as.ID]) {
			c.errf(srcPath, "AWF1027", fmt.Sprintf("%s (continues: %q)", catalog["AWF1027"], as.Continues))
			return // a non-dominating target can't be assembled; later rules are moot.
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
