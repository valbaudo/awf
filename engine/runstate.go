package engine

import (
	"fmt"
	"sync"
)

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
	// OutcomeRejected — a gate exhausted MaxAttempts without passing (Phase 3
	// slice 3.3, spec §5.5). The gate executor is the SOLE PRODUCER in runtime
	// code paths (ClassifyOutcome never returns it). ParseOutcome accepts it
	// at the wire boundary so OTel/CLI consumers can round-trip the value, but
	// per spec §8 only ok-steps commit: a node.completed event with
	// outcome:"rejected" is corruption (Fold rejects it — pinned by
	// TestFold_NodeCompletedRejectedFails). Rejections propagate via the gate
	// handler's return tuple + the gate.attempt event's attempt_outcome field.
	OutcomeRejected Outcome = "rejected"
)

// ParseOutcome validates an on-disk / on-wire outcome string and returns the typed
// Outcome. Unknown strings are an error — the fold uses this as the trust boundary
// between the JSON wire format and in-memory typed state (CLAUDE.md invariant:
// "Outcomes are mechanical only"). Future code that parses an outcome from any
// untrusted source (CLI flag, OTel attribute, external API) MUST also go through
// this function instead of casting `Outcome(s)` directly.
func ParseOutcome(s string) (Outcome, error) {
	switch Outcome(s) {
	case OutcomeOK, OutcomeRetryableFailure, OutcomePermanentFailure, OutcomeRejected:
		return Outcome(s), nil
	default:
		return "", fmt.Errorf("engine: unknown outcome %q (want %q | %q | %q | %q)",
			s, OutcomeOK, OutcomeRetryableFailure, OutcomePermanentFailure, OutcomeRejected)
	}
}

// AttemptResult is one element of RunState.GateAttempts[gatePath]. Records
// what happened on a single gate.attempt — the per-attempt verdict and whether
// `until` accepted it. Built by the Fold from a gate.attempt event (slice 3.3
// engine/fold.go) and by the gate executor's runtime RecordGateAttempt call
// (engine/gate.go). The template evaluator's `evaluate.*` scope reads the
// LATEST entry for the enclosing gate path (engine/scope.go).
//
// AttemptOutcome is one of AttemptPassed / AttemptRejected. N is 1-based —
// the first attempt is N=1.
//
// Verdict is the materialized typed outputs of the last evaluator step (its
// output_schema-validated map); the wire format stores VerdictRef (CAS
// pointer), the Fold materializes via Blobs.Get. READ-ONLY (callers MUST NOT
// mutate — same caveat as NodeResult.Outputs).
type AttemptResult struct {
	N              int
	AttemptOutcome string
	Verdict        map[string]any
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
// Phase 2 + Phase 3 slice 3.3 fields — Phase 3 will further add signals
// (await) and map items (per map[N].item-K).
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

	// GateAttempts records the per-attempt verdicts for every gate that has
	// committed at least one attempt. Slice 3.3 addition; gate path → ordered
	// slice of AttemptResult (oldest first). The slice grows as the gate
	// executor records additional attempts; resume's Fold rebuilds it from
	// gate.attempt events.
	//
	// The template evaluator's `evaluate.*` scope reads the LAST element for
	// the enclosing gate path on attempt n > 1 (slice 3.3 engine/scope.go).
	GateAttempts map[string][]AttemptResult

	// mu serializes access to Completed / Branches / LoopIters / GateAttempts.
	// Phase 2 callers were single-threaded; Phase 3 slice 3.2 (parallel)
	// introduced concurrent branch goroutines.
	//
	// Slice 3.2+ callers MUST use the accessor methods (LookupCompleted /
	// RecordCompleted / LookupBranch / RecordBranch / LookupLoopIters /
	// RecordLoopIter / LookupGateAttempts / RecordGateAttempt). Direct field
	// access is reserved for engine.Fold —
	// Fold runs at resume time BEFORE engine.Run, never concurrent with
	// any goroutine, so its direct map writes are race-free by construction.
	mu sync.Mutex
}

// NewRunState constructs a fresh RunState with the three maps pre-allocated
// and identity fields set. The Epoch is initialized to 1 — the first-run
// baseline; slice 2.6's resume increments via the run.resumed event in the
// fold path. Input is the optional run-input map (nil if the workflow has no
// input schema or --input wasn't passed).
//
// Eliminates the "caller forgets to allocate Completed/Branches/LoopIters"
// footgun. Fold (engine/fold.go) constructs its own RunState with capacity-
// hinted maps; this constructor is for non-Fold construction paths (CLI
// first-run, tests).
func NewRunState(runID, workflowDigest string, input map[string]any) *RunState {
	return &RunState{
		RunID:          runID,
		WorkflowDigest: workflowDigest,
		Input:          input,
		Epoch:          1,
		Completed:      map[string]NodeResult{},
		Branches:       map[string]string{},
		LoopIters:      map[string]int{},
		GateAttempts:   map[string][]AttemptResult{},
	}
}

// LookupCompleted returns the NodeResult stored for path and a present flag.
// Thread-safe; slice 3.2 (parallel) branches call this concurrently.
func (rs *RunState) LookupCompleted(path string) (NodeResult, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	nr, ok := rs.Completed[path]
	return nr, ok
}

// RecordCompleted stores nr for path. Thread-safe.
func (rs *RunState) RecordCompleted(path string, nr NodeResult) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.Completed[path] = nr
}

// LookupBranch returns the "then"|"else" recorded for if-path and a present
// flag. Thread-safe.
func (rs *RunState) LookupBranch(path string) (string, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	which, ok := rs.Branches[path]
	return which, ok
}

// RecordBranch stores which ("then"|"else") for if-path. Thread-safe.
func (rs *RunState) RecordBranch(path string, which string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.Branches[path] = which
}

// LookupLoopIters returns the latest committed iter for loop-path (0 if no
// iter committed yet). Thread-safe.
func (rs *RunState) LookupLoopIters(path string) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.LoopIters[path]
}

// RecordLoopIter stores k as the latest committed iter for loop-path.
// Thread-safe.
func (rs *RunState) RecordLoopIter(path string, k int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.LoopIters[path] = k
}

// LookupGateAttempts returns the per-attempt slice recorded for gatePath, or
// nil if no attempt has been recorded yet (the attempt-1 case for the
// template `evaluate.*` scope's empty-feedback contract). Thread-safe.
//
// READ-ONLY: callers MUST NOT mutate elements of the returned slice or any
// AttemptResult.Verdict map within it — the slice is the live internal
// backing array (same aliasing contract as NodeResult.Outputs). Pinned by
// TestGateAttemptsReturnedSliceIsReadOnly (engine/runstate_test.go).
func (rs *RunState) LookupGateAttempts(gatePath string) []AttemptResult {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.GateAttempts[gatePath]
}

// RecordGateAttempt appends ar to the slice at gatePath. The gate executor
// (engine/gate.go) calls this AFTER a successful Log.Append + Log.Sync of the
// corresponding gate.attempt event — in-memory state mirrors the durable log,
// not the other way around. Thread-safe.
func (rs *RunState) RecordGateAttempt(gatePath string, ar AttemptResult) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.GateAttempts[gatePath] = append(rs.GateAttempts[gatePath], ar)
}
