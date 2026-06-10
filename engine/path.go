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

import (
	"strconv"
)

// iterSep / attemptSep / itemSep are the separators between a loop / gate / map
// runtime path and its per-instance index, in the runtime address grammar. These
// tokens are the single source of truth: IterPath / AttemptPath / ItemPath
// compose them, and the same-instance detector in engine/scope.go
// (stepRuntimePath via instanceFromCtx) prefix-matches ctxPath against them.
// Renaming any would invalidate every existing log; the concentration here is the
// CLAUDE.md "node addressing is one pure function" invariant in action.
const (
	iterSep    = ".iter-"
	attemptSep = ".attempt-"
	itemSep    = ".item-"
)

// Exported segment prefixes for callers that need to recognize the
// runtime-addressing tokens (e.g. engine/gate_path.go's
// enclosingGateForEvaluate string-walks ctxPaths to find an enclosing
// gate). Centralized here so the addressing-token vocabulary has a single
// source of truth.
const (
	AttemptSegmentPrefix = "attempt-"
	GateSegmentPrefix    = "gate["
	CallWorkflowSegment  = "workflow"
)

// IterPath appends a per-iteration suffix to a loop body's static path, producing the
// runtime form the journal and OTel both use:
//
//	IterPath("loop[0].body", 3) → "loop[0].body.iter-3"
//
// `iter` is 1-based (the first iteration is iter-1) — matching the design's "iter-3"
// example in runtime-design §5 and awf-workflow(5) (CHECKPOINTING AND RESUME).
func IterPath(bodyPath string, iter int) string {
	return bodyPath + iterSep + strconv.Itoa(iter)
}

// AttemptPath appends a per-attempt suffix to a gate's static path:
//
//	AttemptPath("gate[0]", 2) → "gate[0].attempt-2"
//
// `attempt` is 1-based. Callers compose `.generate` / `.evaluate` onto the result with
// ir.ChildPath (or a literal `+".generate"`) — those are the gate-internal branch names
// the validator already addresses.
func AttemptPath(gatePath string, attempt int) string {
	return gatePath + attemptSep + strconv.Itoa(attempt)
}

// ItemPath appends a per-element suffix to a map's static path:
//
//	ItemPath("map[0]", 3) → "map[0].item-3"
//
// `item` is 0-based — matching the `{{ <as>.index }}` array-index binding documented in
// spec §5.7 (the first element is item-0; conventional array indexing). Phase 2 doesn't
// execute `map`, but the helper ships now so the addressing grammar is complete and
// Phase 3 inherits it unchanged.
func ItemPath(mapPath string, item int) string {
	return mapPath + itemSep + strconv.Itoa(item)
}

// ItemStepPath appends a body-step path tail to a map item's runtime path,
// producing the journal key the engine reads a per-item body step's committed
// output from:
//
//	ItemStepPath("map[0]", 3, "score")        → "map[0].item-3.score"
//	ItemStepPath("map[0]", 3, "stepA.stepB")  → "map[0].item-3.stepA.stepB"
//	ItemStepPath("map[0]", 3, "")             → "map[0].item-3"
//
// `suffix` is the (possibly multi-segment, possibly empty) body path tail — e.g.
// the last body step id for prune's score lookup, or the WalkNodes suffix for
// reduce/aggregate fan-in. Centralized here (not hand-joined at the call sites)
// so the runtime-address '.'-join stays one source of truth (CLAUDE.md "node
// addressing is one pure function").
func ItemStepPath(mapPath string, item int, suffix string) string {
	p := ItemPath(mapPath, item)
	if suffix == "" {
		return p
	}
	return p + "." + suffix
}

// CallWorkflowRuntimePath appends the reserved child-workflow segment under a
// call-step path.
func CallWorkflowRuntimePath(callPath string) string {
	if callPath == "" {
		return CallWorkflowSegment
	}
	return callPath + "." + CallWorkflowSegment
}

// QualifiedContainerKey scopes a workflow-declared container name to the
// runtime parent that owns the handle.
func QualifiedContainerKey(runtimeParent, container string) string {
	if runtimeParent == "" {
		return container
	}
	return runtimeParent + "::" + container
}

// ParentPath returns the runtime-address parent of a node path, and false when
// the node is a top-level child of the run root (no parent segment).
//
// Every scope boundary in the runtime address grammar is a '.'-join — control
// keywords are "keyword[N]" (bracket-bounded, '.'-free), branch labels
// (.then/.else/.do/.catch/.finally/.body/.generate/.evaluate), iteration
// suffixes (.iter-N / .attempt-N / .item-N), and step ids (validated '.'-free)
// — so the parent is the path with its final '.'-segment trimmed. This is the
// inverse of ir.ChildPath / IterPath / AttemptPath / ItemPath and lives here so
// the addressing grammar stays one source of truth (CLAUDE.md invariant). obs
// (Phase 6) walks it to synthesize control-scope spans.
func ParentPath(path string) (string, bool) {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[:i], true
		}
	}
	return "", false
}
