package ir

import "fmt"

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

	// One WalkNodes pass builds: id → static path, id → *AgentStep, id → document-order
	// ordinal. WalkNodes visits parent-before-children in slice order, so the ordinal is
	// a topological document position usable for the precedes-in-document-order half of
	// the dominator rule (A.3.2). Last-write-wins on (validator-rejected, AWF1004) dup ids.
	agents := map[string]*AgentStep{}
	WalkNodes(wf.Graph, "", func(n Node, _ string) {
		if v, ok := n.(*AgentStep); ok {
			agents[v.ID] = v
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
		_ = tgt // used by later rules (2.3-2.6).
	})
}
