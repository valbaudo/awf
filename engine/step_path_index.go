package engine

import "github.com/valbaudo/awf/ir"

// Extracted from engine/scope.go during the slice 3.3 post-merge cleanup
// (split was 420 lines, 5 concerns). StepPathIndex + walkNodes are a pure
// IR walker — they don't read RunState and aren't bound to a Scope — so
// they live in their own file. Other code references them unchanged.

// StepPathIndex walks wf.Graph and returns a map from step id → static IR path.
// Phase 2 supports CodeStep / AgentStep / SignalStep / If / Loop kinds; the
// remaining control kinds (Try / Parallel / Gate / Map / Skip) are skipped —
// the interpreter (slice 2.5) errors on them at runtime in Phase 2, so any
// step buried inside them is unreachable. Phase 3+ extends this walker.
//
// Duplicate step ids are caught by the validator (AWF1004, slice 1.4) — this
// function trusts the input was validated and last-write-wins on duplicates.
//
// The returned strings are exactly what ir.PathFor / ir.ChildPath produce; see
// ir/path_test.go for the canonical examples ("triage" / "loop[1].body.echo" /
// "loop[1].body.if[1].then.deep_step").
func StepPathIndex(wf *ir.Workflow) map[string]string {
	out := map[string]string{}
	walkNodes(wf.Graph, "", out)
	return out
}

// walkNodes is StepPathIndex's recursive worker. Appends a (step id → static
// path) entry for each step under `list`, recursing into If branches and Loop
// bodies. Phase 3 kinds (Try / Parallel / Gate / Map / Skip) are silently
// skipped — slice 2.5's interpreter errors on them at runtime in Phase 2.
func walkNodes(list ir.NodeList, parent string, out map[string]string) {
	for i, n := range list {
		switch v := n.(type) {
		case *ir.CodeStep:
			out[v.ID] = ir.PathFor(parent, "", v.ID, i)
		case *ir.AgentStep:
			out[v.ID] = ir.PathFor(parent, "", v.ID, i)
		case *ir.SignalStep:
			out[v.ID] = ir.PathFor(parent, "", v.ID, i)
		case *ir.If:
			walkNodes(v.Then, ir.ChildPath(parent, "if", i, "then"), out)
			walkNodes(v.Else, ir.ChildPath(parent, "if", i, "else"), out)
		case *ir.Loop:
			walkNodes(v.Body, ir.ChildPath(parent, "loop", i, "body"), out)
			// Try / Parallel / Gate / Map / Skip — Phase 2 doesn't execute them; skip.
			// Slice 2.5 will fail at the interpreter for any workflow that uses them.
			// Phase 3+ extends this walker to recurse into them.
		}
	}
}
