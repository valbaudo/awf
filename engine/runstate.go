package engine

import "fmt"

// Outcome is the mechanical-only classification a step ends with (AWF standard §6).
// Quality is the gate's job — there is no `success` / `semantic_failure` class.
// The string form is the on-disk and OTel value; do not rename without a migration.
type Outcome string

const (
	// OutcomeOK — clean exit (code 0 + schema-valid output) / signal delivered.
	OutcomeOK Outcome = "ok"
	// OutcomeRetryableFailure — transient: launch/transport error, timeout, nonzero
	// exit not declared permanent, unparseable agent output.
	OutcomeRetryableFailure Outcome = "retryable_failure"
	// OutcomePermanentFailure — agent refusal / policy block, or an exit code in
	// non_retryable_exit_codes.
	OutcomePermanentFailure Outcome = "permanent_failure"
)

// ParseOutcome validates an on-disk / on-wire outcome string and returns the typed
// Outcome. Unknown strings are an error — the fold uses this as the trust boundary
// between the JSON wire format and in-memory typed state (CLAUDE.md invariant:
// "Outcomes are mechanical only"). Future code that parses an outcome from any
// untrusted source (CLI flag, OTel attribute, external API) MUST also go through
// this function instead of casting `Outcome(s)` directly.
func ParseOutcome(s string) (Outcome, error) {
	switch Outcome(s) {
	case OutcomeOK, OutcomeRetryableFailure, OutcomePermanentFailure:
		return Outcome(s), nil
	default:
		return "", fmt.Errorf("engine: unknown outcome %q (want %q | %q | %q)",
			s, OutcomeOK, OutcomeRetryableFailure, OutcomePermanentFailure)
	}
}

// NodeResult is the fold result for one completed node — stored in RunState.Completed
// keyed by node.path. Phase 2 populates this from a `node.completed` event; Phase 3+
// adds gate-attempt aggregation but the per-node shape stays the same.
//
// Both Outputs and OutputsRef (and Stdout / StdoutRef) are present: the *Ref field is
// the CAS pointer the journal stored, the materialized field is what the fold loaded
// via Blobs.Get. The template evaluator reads the materialized values; obs (Phase 6)
// reads the *Ref fields. Keeping both means callers don't re-dereference per resolution.
//
// IMPORTANT — aliasing: Outputs and Files are maps and Stdout is a slice; Go's
// value-copy of a struct shares the underlying storage. Callers MUST treat
// RunState.Completed[*].Outputs, .Files, and .Stdout as read-only — mutating an
// entry through a copied NodeResult corrupts the fold-committed record. Pinned by
// TestNodeResultCopyIsShallow (engine/runstate_test.go).
type NodeResult struct {
	Outcome    Outcome
	ExitCode   *int              // code step only (nil for agent/signal)
	Outputs    map[string]any    // typed; materialized from OutputsRef. READ-ONLY (see NodeResult doc).
	OutputsRef string            // CAS pointer (validates against Outputs)
	Stdout     []byte            // materialized stdout; READ-ONLY (see NodeResult doc). nil if step produced no stdout.
	StdoutRef  string            // CAS pointer (validates against Stdout)
	Files      map[string]string // declared path → CAS ref. READ-ONLY (see NodeResult doc).
}

// RunState is the in-memory fold of the log: the interpreter consults it to skip
// committed nodes, the template evaluator consults it to resolve refs.
//
// Built by Fold (engine/fold.go). The same code path serves first-run (empty log →
// empty RunState) and resume (folded log → populated RunState).
//
// Phase 2 fields only — Phase 3 will add signals (await), gate attempts (per
// gate[N].attempt-K), and map items (per map[N].item-K).
// RunState.Epoch ≠ state.Event.Epoch — see comment on the Epoch field below.
type RunState struct {
	RunID          string
	WorkflowDigest string
	Input          map[string]any // resolved from run.started.Data.input_ref via Blobs.Get
	// Epoch is the *runtime* resume-invocation counter — set to 1 on run.started, then
	// overwritten by each run.resumed event's payload (see Fold). This is NOT the same
	// counter as state.Event.Epoch (which is auto-stamped by state.Log on each Append
	// and bumped by OpenLog on each reopen — see state/log.go). The two counters can
	// diverge if the writer makes a mistake: slice 2.6's cli resume MUST emit
	// RunResumedData{Epoch: rs.Epoch + 1} after each fold, otherwise the runtime
	// counter goes stale while state.Event.Epoch keeps incrementing.
	Epoch uint32

	Completed map[string]NodeResult // node.path → result
	Branches  map[string]string     // if-node path → "then" | "else"
	LoopIters map[string]int        // loop-node path → max completed iteration (1-based)
}
