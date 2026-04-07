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
// Both Outputs and OutputsRef are present: OutputsRef is the CAS pointer the journal
// stored, Outputs is the typed value the fold materialized via Blobs.Get. The template
// evaluator reads Outputs; obs (Phase 6) reads OutputsRef. Keeping both means callers
// don't re-dereference per resolution.
type NodeResult struct {
	Outcome    Outcome
	ExitCode   *int              // code step only (nil for agent/signal)
	Outputs    map[string]any    // typed; materialized from OutputsRef
	OutputsRef string            // CAS pointer (validates against Outputs)
	Files      map[string]string // declared path → CAS ref (no materialization — too large)
}

// RunState is the in-memory fold of the log: the interpreter consults it to skip
// committed nodes, the template evaluator consults it to resolve refs.
//
// Built by Fold (engine/fold.go). The same code path serves first-run (empty log →
// empty RunState) and resume (folded log → populated RunState).
//
// Phase 2 fields only — Phase 3 will add signals (await), gate attempts (per
// gate[N].attempt-K), and map items (per map[N].item-K).
type RunState struct {
	RunID          string
	WorkflowDigest string
	Input          map[string]any // resolved from run.started.Data.input_ref via Blobs.Get
	Epoch          uint32         // bumped by state.Log on each reopen (= each `awf resume`); the run.resumed event records each bump (added in slice 2.1 Task 3)

	Completed map[string]NodeResult // node.path → result
	Branches  map[string]string     // if-node path → "then" | "else"
	LoopIters map[string]int        // loop-node path → max completed iteration (1-based)
}
