// Package engine implements the tree-walking interpreter, the Dispatcher seam, the
// commit boundary, outcome classification, RunState, the log fold, and the runtime
// addressing helpers that extend ir's static path scheme.
//
// Node addressing — engine.IterPath / AttemptPath / ItemPath composed with
// ir.PathFor / ir.ChildPath upstream — is the single source of truth for journal
// keys and OTel awf.node.path. Do NOT format `iter-N` / `attempt-N` / `item-N`
// strings elsewhere; route every runtime path through these helpers (CLAUDE.md
// invariant: "Node addressing is one pure function (engine/path)").
package engine

import "fmt"

// IterPath appends a per-iteration suffix to a loop body's static path, producing the
// runtime form the journal and OTel both use:
//
//	IterPath("loop[0].body", 3) → "loop[0].body.iter-3"
//
// `iter` is 1-based (the first iteration is iter-1) — matching the design's "iter-3"
// example in runtime-design §5 and AgentWorkflowFormat.md §8.
func IterPath(bodyPath string, iter int) string {
	return fmt.Sprintf("%s.iter-%d", bodyPath, iter)
}

// AttemptPath appends a per-attempt suffix to a gate's static path:
//
//	AttemptPath("gate[0]", 2) → "gate[0].attempt-2"
//
// `attempt` is 1-based. Callers compose `.generate` / `.evaluate` onto the result with
// ir.ChildPath (or a literal `+".generate"`) — those are the gate-internal branch names
// the validator already addresses.
func AttemptPath(gatePath string, attempt int) string {
	return fmt.Sprintf("%s.attempt-%d", gatePath, attempt)
}

// ItemPath appends a per-element suffix to a map's static path:
//
//	ItemPath("map[0]", 3) → "map[0].item-3"
//
// `item` is 0-based (matches the §5.7 array-index semantics — `map[0].item-0` is the first
// element). Phase 2 doesn't execute `map`, but the helper ships now so the addressing
// grammar is complete and Phase 3 inherits it unchanged.
func ItemPath(mapPath string, item int) string {
	return fmt.Sprintf("%s.item-%d", mapPath, item)
}
