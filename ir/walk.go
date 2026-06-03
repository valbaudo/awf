package ir

import "fmt"

// WalkNodes is the single pre-order traversal of a NodeList. It calls visit once
// per node — in slice order, parent before children — with the node's OWN static
// IR path, then recurses into every child branch using ir.PathFor / ir.ChildPath,
// centralizing the branch-label convention AND the Parallel quirk (children are
// addressed under the bare parallel[i] path, no branch label) so the family of
// index/collect walkers can never drift from the runtime addressing in
// engine/path.go. Slice order is load-bearing: indexProducers / StepPathIndex are
// last-write-wins on (validator-rejected) duplicate ids, so the order must match
// the old walkers' `for i, n := range`.
//
// visit is called for every node EXCEPT *Skip — skip carries no step/map identity
// and no addressable child, and is absent from the §8 addressing grammar, so none
// of the index/collect callers act on it (byte-identical to the walkers this
// replaces). Leaf-only callers (step indexers) no-op on control kinds via their
// type switch; control-emitting callers (the map indexers) act on the kind they
// care about. All per-caller state (accumulator maps, *collector) is captured by
// the closure, never threaded through this signature — that minimalism is what
// lets the pure builders share one shape. Walkers that carry traversal-direction
// state (walkRefs's evaluateAllowed) or do node-anchored validation on most kinds
// (walkStructural) are deliberately NOT built on this; see their definitions.
//
// ir.Node is a sealed sum type (the unexported isNode marker), so default is
// unreachable for valid input; it panics — like go/ast.Walk and the existing
// cli/runtimes.go walkAgentRefsNodes — so a future node kind added without
// updating this switch fails loudly instead of being silently dropped. This is
// forward-compat documentation, not a coverage guarantee; the static guarantee
// (gochecksumtype + //sumtype:decl on ir.Node) is a separate, out-of-scope call.
func WalkNodes(list NodeList, parent string, visit func(n Node, path string)) {
	for i, n := range list {
		switch v := n.(type) {
		case *CodeStep:
			visit(v, PathFor(parent, "", v.ID, i))
		case *AgentStep:
			visit(v, PathFor(parent, "", v.ID, i))
		case *SignalStep:
			visit(v, PathFor(parent, "", v.ID, i))
		case *Skip:
			// No step/map identity, no addressable child, not in the §8 grammar —
			// an explicit no-op (NOT fall-through) so the default arm stays a true
			// "unhandled kind" guard. Matches all the walkers this replaces.
		case *If:
			visit(v, PathFor(parent, "if", "", i))
			WalkNodes(v.Then, ChildPath(parent, "if", i, "then"), visit)
			WalkNodes(v.Else, ChildPath(parent, "if", i, "else"), visit)
		case *Loop:
			visit(v, PathFor(parent, "loop", "", i))
			WalkNodes(v.Body, ChildPath(parent, "loop", i, "body"), visit)
		case *Try:
			visit(v, PathFor(parent, "try", "", i))
			WalkNodes(v.Do, ChildPath(parent, "try", i, "do"), visit)
			WalkNodes(v.Catch, ChildPath(parent, "try", i, "catch"), visit)
			WalkNodes(v.Finally, ChildPath(parent, "try", i, "finally"), visit)
		case *Parallel:
			path := PathFor(parent, "parallel", "", i)
			visit(v, path)
			WalkNodes(v.Children, path, visit) // bare parallel[i] — no branch label
		case *Gate:
			visit(v, PathFor(parent, "gate", "", i))
			WalkNodes(v.Generate, ChildPath(parent, "gate", i, "generate"), visit)
			WalkNodes(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), visit)
		case *Map:
			visit(v, PathFor(parent, "map", "", i))
			WalkNodes(v.Body, ChildPath(parent, "map", i, "body"), visit)
		default:
			panic(fmt.Sprintf("ir.WalkNodes: unexpected node type %T", n))
		}
	}
}
