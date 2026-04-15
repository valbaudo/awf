package engine

// Phase 2.1 event-type names — the events the fold dispatches on. These are the
// wire-format string values stored in state.Event.Type; renaming any of them would
// invalidate every existing log. The vocabulary expands as later slices add writers:
// 2.4 added "retry.attempt"; 2.5 adds "node.failed" + "run.finished" (terminal events
// the interpreter / CLI emit). node.started is intentionally deferred — no Phase 2
// consumer (Phase 6's obs is the natural consumer; the Fold's default-switch-arm
// means a later writer can land additively without breaking old logs). Future phases
// add "signal.received" / "map.item" / "agent.event" / "io.chunk" / ….
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
