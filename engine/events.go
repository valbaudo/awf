package engine

import "github.com/valbaudo/awf/agent"

// Phase 2.1 event-type names — the events the fold dispatches on. These are the
// wire-format string values stored in state.Event.Type; renaming any of them would
// invalidate every existing log. The vocabulary expands as later slices add writers:
// 2.4 added "retry.attempt"; 2.5 adds "node.failed" + "run.finished" (terminal events
// the interpreter / CLI emit). node.started writer landed in Phase 6 slice 6.1 (obs
// is the natural consumer; the Fold's default-switch-arm means a later writer can land
// additively without breaking old logs). Phase 3 adds "node.skipped" (3.1),
// "gate.attempt" (3.3), "map.item" (3.4), and "signal.received" / "run.paused" /
// "run.cancelled" (3.5). Phase 5 slice 5.2 added "agent.event". Still future:
// "io.chunk".
const (
	EventRunStarted = "run.started"
	// EventNodeStarted is emitted by the interpreter when a STEP node enters
	// dispatch (Phase 6 slice 6.1). OBSERVATIONAL — Fold ignores it (default
	// arm); obs projects it as the START of a two-event span. See NodeStartedData.
	EventNodeStarted   = "node.started"
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

// ReasonImageUnavailable is the sole tolerated per-item failure cause on a
// map.item's Reason field (P6a): a non-empty runtime image reference that could
// not be booted. A deterministic render/definition fault is NOT a reason — it
// fails the whole map as permanent_failure (see engine/map.go).
const ReasonImageUnavailable = "image_unavailable"

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
	// ImageDigest is the content digest of the runtime-resolved image this
	// element booted (P6a) — empty for a statically-imaged map. omitempty keeps
	// pre-P6a logs and static maps byte-identical (additive, like SnapshotRef).
	ImageDigest string `json:"image_digest,omitempty"`
	// Reason is a machine-readable INFRA cause for a tolerated item_failed (P6a):
	// currently only ReasonImageUnavailable (a non-empty runtime image ref that
	// could not be booted). Empty for item_passed and for a plain body failure.
	// It distinguishes "couldn't boot this element" from "this element ran and
	// produced a negative result" (the Tekton/Temporal infra-vs-result split)
	// WITHOUT a new status — the two-value Status tally and MinSuccess math are
	// untouched; it is NOT a quality/result verdict (that is the gate's job). A
	// deterministic render/definition fault is NOT a reason — it fails the whole
	// map as permanent_failure. Audit/forensic field: no production reader today
	// (consumers are the docker RepoDigests follow-up + a future obs projection).
	Reason string `json:"reason,omitempty"`
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

// Backend kind constants — the wire vocabulary written into
// RunStartedData.Backend (engine.RunStartedData) and the matching
// --backend flag values consumed by cli/run.go. These three uses MUST
// agree on spelling; centralizing the constants here triple-checks them
// at compile time.
//
// BackendNative is the production default (slice 4.7). A pre-slice-4.5
// log (no Backend field in its run.started payload) decodes to "" —
// cli/resume.go maps "" → BackendDocker so legacy logs resume against
// the slice-4.5 default. This IS a behavior change for legacy logs
// (pre-slice-4.5 cli.Run hard-wired fake); documented in the slice-4.5
// PR body's Migration section. Native runs are NOT resumable — see
// cli/backend.go:readBackendKindFromLog for the resume-side guard.
const (
	BackendFake   = "fake"
	BackendDocker = "docker"
	BackendNative = "native"
)

// RunStartedData is the payload of the first event in a run (and the only
// event the run.id, workflow_digest, input_ref, and backend kind live in —
// the fold reads them from here).
//
// Backend is the kind string the cli/run.go writer set from the --backend
// flag (slice 4.5; one of BackendFake / BackendDocker / BackendNative). cli/resume.go reads
// it back to pick the same Backend on resume — no --backend flag mismatch
// class. Empty in pre-slice-4.5 logs (omitempty); consumer maps "" →
// BackendDocker. BackendNative is non-resumable (slice 4.7) — resume of
// a native log returns a typed error rather than dispatching.
//
// Phase 2: Runtimes is always empty (no `uses:` execution). Phase 5
// populates it with {ref, resolved-version} per agent step, and resume
// verifies the resolved versions against the live registry. `omitempty` on
// Runtimes means both nil and empty-slice writers produce identical
// on-disk JSON (the key is absent) — avoids the silent `"runtimes":null`
// vs `"runtimes":[]` wire drift a Phase 5 writer would otherwise create
// by forgetting to initialize an empty slice.
type RunStartedData struct {
	RunID           string            `json:"run_id"`
	WorkflowDigest  string            `json:"workflow_digest"`
	WorkflowID      string            `json:"workflow_id,omitempty"`      // slice 6.1 — obs awf.workflow.id (standard §9); empty in pre-6.1 logs
	WorkflowVersion int               `json:"workflow_version,omitempty"` // slice 6.1 — obs awf.workflow.version; 0 in pre-6.1 logs
	InputRef        string            `json:"input_ref,omitempty"`        // empty if Workflow.Input is nil
	Backend         string            `json:"backend,omitempty"`          // slice 4.5; "" → BackendDocker on resume
	Runtimes        []ResolvedRuntime `json:"runtimes,omitempty"`
}

// ResolvedRuntime is one element of RunStartedData.Runtimes — a `uses:` ref + the
// concrete version it resolved to + the IR-declared container the version was
// resolved IN (per Phase 5 slice 5.1, decision 5: different containers may have
// different `claude` binaries on PATH, so drift detection is scoped per-(ref,
// container) pair, not just per-ref). Phase 2-4 don't populate this; the type
// exists so the wire format is stable from Phase 5.
type ResolvedRuntime struct {
	Ref       string `json:"ref"`
	Version   string `json:"version"`
	Container string `json:"container,omitempty"` // slice 5.1 — IR-declared container name (spec §3); different containers may have different binary versions
}

// RunResumedData is the payload of the run.resumed event — emitted at the top of every
// `awf resume` by slice 2.6. The Epoch counter is the resume-invocation index (1 after
// the first `awf run`; 2 after the first `awf resume`; …).
type RunResumedData struct {
	Epoch uint32 `json:"epoch"`
}

// NodeStartedData is the payload of a node.started event (Phase 6 slice 6.1).
// Emitted by the interpreter when a STEP node enters dispatch (after the resume
// short-circuit, so replayed nodes don't re-emit). Kind is "code" | "agent" |
// "signal". OBSERVATIONAL — Fold ignores it (default arm); obs projects it as
// the START of a two-event span, finalized by the matching node.completed /
// node.failed. A node.started with no terminal event is the Pending/Incomplete
// (in-flight or crashed) span.
type NodeStartedData struct {
	Kind string `json:"kind"`
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
	// Slice 6.1 — the adapter's per-step metrics, persisted VERBATIM (zero engine
	// interpretation). nil/omitted for code & signal steps. omitempty keeps
	// legacy logs decoding to nil (additive, like Backend slice 4.5 / Runtimes
	// slice 5.1). obs projects this into awf.cost.* / gen_ai.usage.*.
	Metrics *agent.MetricSet `json:"metrics,omitempty"`
	// Slice 7.1 — recorded ONLY when snapshot:workspace captured a CoW diff
	// (the dispatcher Put it to Blobs pre-commit; Commit records the ref, never
	// re-Puts). omitempty keeps pre-Phase-7 logs and non-snapshot steps byte-
	// identical (additive, like Metrics slice 6.1). Resume folds these into a
	// separate snapshot map; obs reads them off this event.
	SnapshotRef string `json:"snapshot_ref,omitempty"` // CoW workspace diff ref (snapshot:workspace only)
	Container   string `json:"container,omitempty"`    // bare container name (resume snapshot mapping + obs)
	// TranscriptRef is the CAS pointer for the participating turn's verbatim {user,
	// assistant} pair (continues: threading). Empty for non-participating steps.
	// omitempty keeps non-conversation logs byte-identical (additive, like SnapshotRef).
	TranscriptRef string `json:"transcript_ref,omitempty"`
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

const (
	// Phase 5 slice 5.2. agent.event: the interpreter writes one entry per
	// agent.AgentEvent buffered in DispatchResult.AgentEvents, BEFORE the
	// node.completed commit. OBSERVATIONAL — Fold ignores it (default arm).
	// Phase 6 obs will project these as OTel span events. Mirrors how
	// retry.attempt is treated: appended for trace/obs, invisible to resume.
	EventAgentEvent = "agent.event"
)

// AgentEventData is the payload of an agent.event log entry. The dispatcher
// (engine/local_dispatcher.go runAgent) drains <-chan agent.AgentEvent from
// adapter.Launch and buffers them into DispatchResult.AgentEvents; the
// interpreter-level engine/agent_step.go writes one AgentEventData per
// buffered event via Log.Append BEFORE Commit (so the journal records the
// stream alongside the node it belongs to).
//
// Payload offload policy: PayloadInline carries the raw event bytes when
// `Size <= agentEventInlineThreshold` (4096 bytes, mirroring io.chunk per
// the Phase 5 spec slice 5.2 row). Larger payloads land in Blobs and
// PayloadRef carries the CAS pointer; PayloadInline is then nil. Always
// exactly ONE of PayloadInline / PayloadRef is non-empty.
//
// agent.event is OBSERVATIONAL — Fold ignores it. Resume reconstructs
// RunState from node.completed events; agent events are for trace/obs only
// (Phase 6 will project them as OTel span events). This mirrors how
// retry.attempt is treated.
type AgentEventData struct {
	Kind          string `json:"kind"`                     // adapter-specific (e.g. Claude Code: "system", "assistant", "user", "tool_use", "tool_result", "thinking", "result", "rate_limit")
	Stream        string `json:"stream,omitempty"`         // "stdout" | "stderr"
	Size          int    `json:"size"`                     // payload byte count (whether inline or offloaded)
	PayloadInline []byte `json:"payload_inline,omitempty"` // raw event bytes when Size <= threshold
	PayloadRef    string `json:"payload_ref,omitempty"`    // CAS pointer when Size > threshold
}

// agentEventInlineThreshold is the per-event inline/offload boundary, in
// bytes. Mirrors io.chunk per the Phase 5 spec slice 5.2 row ("payload
// offloaded to Blobs for > 4 KiB, mirrors io.chunk's threshold"). Private
// because slice 5.2 is the only consumer (engine/agent_step.go in Task 8).
const agentEventInlineThreshold = 4096

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
