package engine

import "github.com/valbaudo/awf/ir"

// StepPathIndex walks wf.Graph and returns a map from step id → static IR path.
// It indexes every step, wherever it sits (the recursion + path-construction is
// ir.WalkNodes). The static path is the producer's *definition* address;
// engine.Scope.stepRuntimePath translates it to the runtime address (inserting
// iter-K / attempt-M / item-K) at resolution time.
//
// Duplicate step ids are caught by the validator (AWF1004); this trusts the input
// was validated and last-write-wins on duplicates.
func StepPathIndex(wf *ir.Workflow) map[string]string {
	out := map[string]string{}
	ir.WalkNodes(wf.Graph, "", func(n ir.Node, path string) {
		switch v := n.(type) {
		case *ir.CodeStep:
			out[v.ID] = path
		case *ir.AgentStep:
			out[v.ID] = path
		case *ir.SignalStep:
			out[v.ID] = path
		}
	})
	return out
}
