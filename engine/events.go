package engine

// Phase 2.1 event-type names — the events the fold dispatches on. These are the
// wire-format string values stored in state.Event.Type; renaming any of them would
// invalidate every existing log. The vocabulary expands as later slices add writers:
// 2.4 added "retry.attempt"; 2.5 adds "node.failed" + "run.finished" (terminal events
// the interpreter / CLI emit). node.started is intentionally deferred — no Phase 2
// consumer (Phase 6's obs is the natural consumer; the Fold's default-switch-arm
// means a later writer can land additively without breaking old logs). Phase 3 adds
// "node.skipped" (3.1), "gate.attempt" (3.3), "map.item" (3.4), and
// "signal.received" / "run.paused" / "run.cancelled" (3.5). Future phases add
// "agent.event" / "io.chunk" / … (Phase 6 obs).
const (
	EventRunStarted    = "run.started"
	EventRunResumed    = "run.resumed"
	EventNodeCompleted = "node.completed"
	EventBranchTaken   = "branch.taken"
	EventLoopIter      = "loop.iter"
	EventRetryAttempt  = "retry.attempt"
	EventNodeFailed    = "node.failed"
	EventRunFinished   = "run.finished"
	// Phase 3 slice 3.1 addition. Observational only — Fold default-arm-ignores
	// the event. The state effect of a skip comes from the target scope recording
	// its own normal completion event (loop.iter for a skipped iter, run.finished
	// for a root skip, etc.). Phase 6 obs will project node.skipped as a "skipped"
	// span marker; cli inspect/trace renders it.
	EventNodeSkipped = "node.skipped"
)

const (
	// Phase 3 slice 3.3. Committed by the gate executor (engine/gate.go) at the
	// END of each attempt — i.e., AFTER the evaluator's last step committed and
	// the gate's `until` evaluated against the verdict. Crash≠verdict invariant:
	// a mechanical failure of any generate/evaluate step propagates BEFORE this
	// event is written (design §D step 1-2); only a real evaluation consumes an
	// attempt.
	EventGateAttempt = "gate.attempt"
)

const (
	// Phase 3 slice 3.4. Committed by the map handler (engine/map.go) per item,
	// AFTER the item's body terminates (whether the body produced an ok-final-
	// outcome, a failure, or a skip-target-detected ok). N is the 0-based item
	// index (matching engine.ItemPath's "item-N" suffix and spec §5.7's
	// `{{ <as>.index }}` 0-based convention). Status is one of ItemPassed /
	// ItemFailed.
	EventMapItem = "map.item"
)

// Map item statuses — the Status field on MapItemData. NOT the same vocabulary
// as the Outcome enum (engine/runstate.go) or AttemptPassed/AttemptRejected:
// a map item's body can succeed (item_passed) or fail (item_failed); the map
// as a WHOLE returns OutcomeOK if the success count meets MinSuccess, else
// returns an error.
const (
	ItemPassed = "item_passed"
	ItemFailed = "item_failed"
)

// MapItemData is the payload of a map.item event (Phase 3 slice 3.4).
// N is 0-based (per engine.ItemPath + spec §5.7). Status is one of
// ItemPassed / ItemFailed.
//
// NB: unlike gate.attempt (which carries verdict_ref), map.item carries NO
// item-value reference — the bound over[N] value is derived from re-evaluating
// `over` on resume entry (slice 3.4 Design Q3). Adding it later would be a
// backward-compatible extension (omitempty on the new field).
type MapItemData struct {
	N      int    `json:"n"`
	Status string `json:"status"`
}

// Gate attempt outcomes — the AttemptOutcome field on GateAttemptData. NOT
// the same vocabulary as the Outcome enum (engine/runstate.go): a gate attempt
// can pass or reject as a verdict; the gate as a WHOLE returns OutcomeOK on
// passed-attempt OR OutcomeRejected on max-attempts-reached.
const (
	AttemptPassed   = "attempt_passed"
	AttemptRejected = "attempt_rejected"
)

const (
	// Phase 3 slice 3.5. signal.received: the interpreter writes this AFTER
	// signal.Broker hands off a delivery payload AND that payload validates
	// against the SignalStep's output_schema. The payload is CAS'd into Blobs
	// first (commit-atomicity invariant per spec §8); SignalReceivedData.PayloadRef
	// holds the resulting blob ref. Multiple deliveries to the same await are
	// impossible (the await consumes exactly one and commits via node.completed),
	// but multiple signal.received events MAY appear in a log for distinct await
	// steps consuming distinct signals.
	EventSignalReceived = "signal.received"

	// Phase 3 slice 3.5. run.paused: the interpreter writes this when control-
	// file polling (engine/controls.go) detects a pause.json file in the run's
	// control directory. NON-TERMINAL — `awf resume` does NOT refuse on
	// run.paused (unlike run.finished / node.failed / run.cancelled).
	EventRunPaused = "run.paused"

	// Phase 3 slice 3.5. run.cancelled: the interpreter writes this when
	// control-file polling detects a cancel.json file. TERMINAL — `awf resume`
	// refuses (slice 2.6's 3-class refusal grows to 4). Cancel propagates ctx-
	// cancel through running goroutines; finally blocks run on ctx-cancel
	// (slice 3.1 invariant); the event is appended + fsync'd before engine.Run
	// returns (Outcome(""), signal.ErrCancelled).
	EventRunCancelled = "run.cancelled"
)

// SignalReceivedData is the payload of a signal.received event (Phase 3 slice
// 3.5). Name is the signal-name the await step blocked on. Seq is the broker-
// assigned monotonic counter (1-based per name; see signal/broker.go).
// PayloadRef is the CAS pointer for the typed payload. Empty PayloadRef means
// the signal carried no payload (signal step with no output_schema, valid per
// spec §4.3).
type SignalReceivedData struct {
	Name       string `json:"name"`
	Seq        int    `json:"seq"`
	PayloadRef string `json:"payload_ref,omitempty"`
}

// RunPausedData is the payload of a run.paused event (Phase 3 slice 3.5).
// NodePath is the runtime path the operator requested via `awf pause --before
// <node-path>` (empty for unconditional pause). Reason is the operator's
// free-text from `awf pause --reason <text>` ("" if not set).
//
// NodePath does NOT pin where execution resumes — `awf resume` always re-enters
// at the uncommitted frontier per spec §8. NodePath is observational only.
type RunPausedData struct {
	NodePath string `json:"node_path,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// RunCancelledData is the payload of a run.cancelled event (Phase 3 slice 3.5).
// Reason is the operator-supplied free-text from `awf cancel --reason <text>`
// ("" if not set). TERMINAL event — `awf resume` refuses any log with this
// event in it.
type RunCancelledData struct {
	Reason string `json:"reason,omitempty"`
}

// GateAttemptData is the payload of a gate.attempt event (Phase 3 slice 3.3).
// N is 1-based. AttemptOutcome is one of AttemptPassed / AttemptRejected.
//
// VerdictRef points at the last evaluator step's typed outputs in Blobs
// (same CAS namespace as NodeCompletedData.OutputsRef); the Fold
// dereferences it to populate RunState.GateAttempts[gatePath]'s
// AttemptResult.Verdict. Per spec §5.5 the final evaluate node MUST
// declare output_schema, so VerdictRef is always non-empty for a
// well-formed gate.attempt; omitempty is only to avoid a spurious
// "verdict_ref":"" key on the wire.
type GateAttemptData struct {
	N              int    `json:"n"`
	AttemptOutcome string `json:"attempt_outcome"`
	VerdictRef     string `json:"verdict_ref,omitempty"`
}

// RunStartedData is the payload of the first event in a run (and the only event the
// run.id, workflow_digest, and input_ref live in — the fold reads them from here).
//
// Phase 2: Runtimes is always empty (no `uses:` execution). Phase 5 populates it
// with {ref, resolved-version} per agent step, and resume verifies the resolved
// versions against the live registry. `omitempty` on Runtimes means both nil and
// empty-slice writers produce identical on-disk JSON (the key is absent) — avoids
// the silent `"runtimes":null` vs `"runtimes":[]` wire drift a Phase 5 writer would
// otherwise create by forgetting to initialize an empty slice.
type RunStartedData struct {
	RunID          string            `json:"run_id"`
	WorkflowDigest string            `json:"workflow_digest"`
	InputRef       string            `json:"input_ref,omitempty"` // empty if Workflow.Input is nil
	Runtimes       []ResolvedRuntime `json:"runtimes,omitempty"`
}

// ResolvedRuntime is one element of RunStartedData.Runtimes — a `uses:` ref + the
// concrete version it resolved to. Phase 2 doesn't populate this; the type exists
// so the wire format is stable from Phase 5 (when agent adapters land).
type ResolvedRuntime struct {
	Ref     string `json:"ref"`
	Version string `json:"version"`
}

// RunResumedData is the payload of the run.resumed event — emitted at the top of every
// `awf resume` by slice 2.6. The Epoch counter is the resume-invocation index (1 after
// the first `awf run`; 2 after the first `awf resume`; …).
type RunResumedData struct {
	Epoch uint32 `json:"epoch"`
}

// NodeCompletedData is the commit-class event: the engine appends this only after the
// step's typed outputs and any declared output_files have been Put into Blobs. Its
// existence in the log IS the completion record (spec §8). On resume the fold reads
// these to skip already-committed nodes.
//
// OutputsRef is empty if the step has no output_schema (no typed outputs); StdoutRef
// is empty if the step produced no stdout (or has none — agent/signal); Files is
// empty if the step has no output_files. omitempty keeps the on-disk JSON minimal.
type NodeCompletedData struct {
	Outcome    string            `json:"outcome"` // always "ok" — only ok-steps commit
	ExitCode   *int              `json:"exit_code,omitempty"`
	OutputsRef string            `json:"outputs_ref,omitempty"`
	StdoutRef  string            `json:"stdout_ref,omitempty"` // CAS pointer; empty if step produced no stdout (or has none — agent/signal)
	Files      map[string]string `json:"files,omitempty"`      // declared path → CAS ref
}

// BranchTakenData is the if-decision marker (spec §5.1). Fold uses Which to know which
// branch was visited so resume re-walks the same one. Always "then" or "else".
type BranchTakenData struct {
	Which string `json:"which"` // "then" | "else"
}

// LoopIterData is the per-iteration marker emitted at the end of each loop body (spec
// §5.2 — do-while, so this fires AFTER the iteration's body completes). Fold tracks
// the max N per loop path so resume knows how many iterations have committed.
type LoopIterData struct {
	N int `json:"n"` // 1-based iteration number
}

// RetryAttemptData is the payload of a retry.attempt event — emitted by
// engine.RunWithRetry once per non-final attempt (so on an N-attempt run that
// ultimately succeeds or exhausts, N-1 retry.attempt events land before the
// final node.completed / node.failed). Durability class is non-critical —
// node.completed remains the only authoritative completion record per spec §8,
// so retry.attempt rides the next fsync (RunWithRetry never calls Log.Sync()
// after one).
//
// Outcome is the per-attempt classified outcome (retryable_failure /
// permanent_failure — never ok, since an ok attempt isn't recorded as a retry
// step). Error is the free-text rendering of the attempt's error (transport,
// parse, schema failure).
type RetryAttemptData struct {
	N       int    `json:"n"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// NodeFailedData is the payload of a node.failed event — emitted by the
// interpreter when a step terminates without committing. Outcome is always
// "retryable_failure" (exhausted retries) or "permanent_failure" (declared
// non_retryable_exit_codes hit, or the interpreter classifying a template-eval
// error per slice 2.5 Design question 7). Error is the free-text rendering of
// the underlying cause; on retryable exhaustion it's the LAST attempt's error
// (the same string RunWithRetry returns to the interpreter).
//
// The fold ignores node.failed events — they're observational only. Phase 6's
// obs will project them as failed spans; slice 2.6's resume will refuse to
// resume a run whose tail event is node.failed (the run is terminal).
type NodeFailedData struct {
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// RunFinishedData is the payload of the run.finished event — the terminal
// marker the CLI appends after engine.Run returns. Outcome is the run-level
// rollup: "ok" if every step committed; otherwise the failing step's outcome.
// The fold ignores run.finished (it's observational); slice 2.6's resume
// refuses to resume a run with a run.finished event in its log (the run is
// terminal and shouldn't be re-entered).
type RunFinishedData struct {
	Outcome string `json:"outcome"`
}

// NodeSkippedData is the observational marker emitted as a Skip unwinds
// through a scope (Phase 3 design §B). Path is the path of the skipped scope
// (e.g. "loop[0].body.iter-2" for a skipped loop iter, "" for a root skip).
// Reason is the author's free-text from `skip: <reason>` (spec §5.6).
//
// Fold IGNORES this event (default arm). The state effect of a skip comes
// from the target scope recording its own normal completion event (e.g.
// loop.iter{k} for a skipped iter).
type NodeSkippedData struct {
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason,omitempty"`
}
