package engine

import (
	"fmt"

	"github.com/valbaudo/awf/ir"
)

// SkipUnwind is the typed sentinel `Skip` (spec §5.6) raises to unwind the
// recursive interpreter walk to its target scope (workflow root, loop iter,
// try.do, parallel branch, gate attempt, map item — the last three land in
// slices 3.2 / 3.3 / 3.4). Implements `error` so it flows through the existing
// `(Outcome, error)` propagation pattern; recognizing handlers use
// `var su *SkipUnwind; errors.As(err, &su)` to distinguish it from a real failure.
//
// NOT an OutcomeError-equivalent — Skip is terminal-ok at its target scope,
// not failure. `runTry` recognizes SkipUnwind from its Do block and skips
// Catch (no catch on a skip — finally still runs).
//
// TargetPath is empty after runSkip; recognizing handlers may populate it
// when they ARE the target (for tracing). Reason is the author's free-text
// from `skip: <reason>` (spec §5.6).
//
// Phase 3 design decision 5 (revised): plain error returns + SkipUnwind
// sentinel only — no typed OutcomeError wrapper, since unconditional catch
// (decision 7) only needs `err != nil` and `var su *SkipUnwind; errors.As(err, &su)`.
type SkipUnwind struct {
	TargetPath string
	Reason     string
}

// Error renders the SkipUnwind for diagnostic purposes — engine internals
// don't log SkipUnwind directly (it's a control sentinel, not a fault), but
// if it leaks into a generic error path the message identifies what happened.
func (e *SkipUnwind) Error() string {
	return fmt.Sprintf("engine: skip unwind to %q (reason: %s)", e.TargetPath, e.Reason)
}

// runSkip is the Skip handler. Returns (OutcomeOK, &SkipUnwind{...}). The
// recursive walk propagates the tuple via the existing
// `if oc != OutcomeOK || err != nil { return oc, err }` check until a target
// scope (Run, runLoop, runTry, …) recognizes the *SkipUnwind via
// `var su *SkipUnwind; errors.As(err, &su)` and absorbs it as terminal-ok.
//
// runSkip does NOT populate SkipUnwind.TargetPath — the target is determined
// by which handler recognizes the unwind first (workflow root, the nearest
// enclosing loop/try/parallel/gate/map). Recognizing handlers may set
// TargetPath if they care about tracing it (Phase 6 obs); the engine never
// reads TargetPath in slice 3.1.
func runSkip(skip *ir.Skip) (Outcome, error) {
	return OutcomeOK, &SkipUnwind{Reason: skip.Reason}
}
