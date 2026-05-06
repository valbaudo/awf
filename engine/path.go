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
	"fmt"
	"strconv"
)

// iterSep is the separator between a loop body's path and its iteration
// number in the runtime address grammar. The token lives here as the single
// source of truth — other helpers (IterPath, iterPrefix) compose around it,
// and prefix-matching callers (engine/scope.go's iterForLoop) consume it via
// iterPrefix. Renaming this token would invalidate every existing log; the
// concentration here is the CLAUDE.md "node addressing is one pure function"
// invariant in action.
const iterSep = ".iter-"

// Exported segment prefixes for callers that need to recognize the
// runtime-addressing tokens (e.g. engine/gate_path.go's
// enclosingGateForEvaluate string-walks ctxPaths to find an enclosing
// gate). Centralized here so the addressing-token vocabulary has a single
// source of truth.
const (
	AttemptSegmentPrefix = "attempt-"
	GateSegmentPrefix    = "gate["
)

// iterPrefix returns the prefix shared by every iter-suffixed runtime path
// for a given loop body, e.g. iterPrefix("loop[0].body") → "loop[0].body.iter-".
// Used by engine.Scope to detect "is ctxPath inside this loop body's iter-K?"
// via strings.HasPrefix without hard-coding the iter token.
func iterPrefix(bodyPath string) string {
	return bodyPath + iterSep
}

// IterPath appends a per-iteration suffix to a loop body's static path, producing the
// runtime form the journal and OTel both use:
//
//	IterPath("loop[0].body", 3) → "loop[0].body.iter-3"
//
// `iter` is 1-based (the first iteration is iter-1) — matching the design's "iter-3"
// example in runtime-design §5 and awf-workflow(5) (CHECKPOINTING AND RESUME).
func IterPath(bodyPath string, iter int) string {
	return iterPrefix(bodyPath) + strconv.Itoa(iter)
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
// `item` is 0-based — matching the `{{ <as>.index }}` array-index binding documented in
// spec §5.7 (the first element is item-0; conventional array indexing). Phase 2 doesn't
// execute `map`, but the helper ships now so the addressing grammar is complete and
// Phase 3 inherits it unchanged.
func ItemPath(mapPath string, item int) string {
	return fmt.Sprintf("%s.item-%d", mapPath, item)
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
