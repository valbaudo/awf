package engine

import "github.com/valbaudo/awf/ir"

// Extracted from engine/scope.go during the slice 3.3 post-merge cleanup
// (split was 420 lines, 5 concerns). StepPathIndex + walkNodes are a pure
// IR walker — they don't read RunState and aren't bound to a Scope — so
// they live in their own file. Other code references them unchanged.

// StepPathIndex walks wf.Graph and returns a map from step id → static IR path.
// It recurses into every control kind that can hold steps (If / Loop / Try /
// Parallel / Gate / Map), so a step buried anywhere is addressable. The static
// path is the producer's *definition* address; engine.Scope.stepRuntimePath
// translates it to the runtime address (inserting iter-K / attempt-M / item-K)
// at resolution time.
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
// path) entry for each step under `list`, recursing into every control kind's
// child branches. Skip has no step-bearing children. The branch labels match
// ir.ChildPath / ir.PathFor exactly so the static paths line up with the
// runtime-addressing scheme (engine/path.go).
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
		case *ir.Try:
			walkNodes(v.Do, ir.ChildPath(parent, "try", i, "do"), out)
			walkNodes(v.Catch, ir.ChildPath(parent, "try", i, "catch"), out)
			walkNodes(v.Finally, ir.ChildPath(parent, "try", i, "finally"), out)
		case *ir.Parallel:
			walkNodes(v.Children, ir.PathFor(parent, "parallel", "", i), out)
		case *ir.Gate:
			walkNodes(v.Generate, ir.ChildPath(parent, "gate", i, "generate"), out)
			walkNodes(v.Evaluate, ir.ChildPath(parent, "gate", i, "evaluate"), out)
		case *ir.Map:
			walkNodes(v.Body, ir.ChildPath(parent, "map", i, "body"), out)
			// Skip has no step-bearing children.
		}
	}
}
