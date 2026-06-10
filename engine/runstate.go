package engine

import (
	"fmt"
	"slices"
	"sync"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
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

// MapItemRecord is one element of RunState.MapItems[mapPath]. Records what
// happened on a single map item — its bound over[N] value and whether body
// completed ok (slice 3.4, design §E).
//
// N is 0-based (matching engine.ItemPath's "item-N" suffix + spec §5.7's
// `{{ <as>.index }}` convention). Status is one of ItemPassed / ItemFailed
// (defined in engine/events.go).
//
// ItemValue is the materialized over[N] value, populated by the map executor
// (engine/map.go) BEFORE dispatching the item's body. On resume, the Fold
// populates Status from the map.item event but leaves ItemValue NIL
// (Design Q3); the runtime re-evaluates `over` and calls UpdateMapItemValue
// to fill it in before body re-execution. READ-ONLY (callers MUST NOT mutate
// — same caveat as NodeResult.Outputs).
type MapItemRecord struct {
	N         int
	ItemValue any
	Status    string
	// ImageDigest / Reason are folded from the map.item event (P6a) — the durable
	// record of what this committed element booted and, on failure, why. Read-back,
	// never re-derived (run state; ItemValue is re-derived from `over`).
	// Audit/forensic only: NO production reader today — committed map items are
	// replayed-as-skipped on resume and never re-boot, so the digest is not a
	// re-pin input. Consumers are the docker RepoDigests follow-up + a future obs
	// projection.
	ImageDigest string
	Reason      string
}

// SignalEntry is one element of RunState.Signals[name]. Records a delivered
// signal — observational only. Slice 3.5 addition.
//
// Seq is the broker-assigned monotonic counter per signal name (1-based;
// matches signal/broker.go's seq allocation). PayloadRef is the CAS pointer;
// callers that need the materialized payload call blobs.Get(PayloadRef) +
// json.Unmarshal at use (the deferred-materialization pattern from Temporal).
//
// REFS ONLY — no materialized Payload field. See SignalReceivedEntry's
// doc-comment for the rationale (non-object payloads break json.Unmarshal
// into map[string]any at Fold time).
type SignalEntry struct {
	Seq        int
	PayloadRef string
}

// PauseMarker is the in-memory mirror of the latest run.paused event.
// LookupPaused returns nil if no pause is active. NON-TERMINAL — the engine
// does NOT halt on a stale LookupPaused; the controls polling helper is the
// live decision-maker.
//
// NodePath is the runtime path the operator requested via `awf pause --before
// <node-path>` (empty for unconditional pause). Reason is the operator's
// free-text from `awf pause --reason <text>`.
type PauseMarker struct {
	NodePath string
	Reason   string
}

// SignalReceivedEntry is the path-keyed record of a journaled signal.received
// event (slice 3.5 design Q7 — half-commit resume mechanism). Fold populates
// SignalReceivedAt[event.Path] from each signal.received event; the
// runSignalStep handler checks for an existing entry BEFORE calling
// broker.Receive — if present, it's the half-commit case (signal.received
// landed but node.completed did not), so the handler skips the Receive +
// signal.received-append and writes only the missing node.completed.
//
// Seq is the broker-assigned counter from the originating signal-<name>-<seq>.json.
// PayloadRef is the CAS pointer (the same one node.completed will reference).
//
// REFS ONLY — no materialized Payload field. An earlier draft stored the
// typed payload directly, but that assumed payloads are always JSON objects;
// unschema'd signals can carry non-object payloads (spec §4.3 allows it),
// which would have broken Fold's json.Unmarshal into map[string]any. The
// refined design stores only refs; the half-commit handler re-derives the
// typed payload via Blobs.Get + ValidateAgainstSchema(payload, schema) when
// the SignalStep has an output_schema declared.
type SignalReceivedEntry struct {
	Seq        int
	PayloadRef string
}

// CallStartedRecord is the folded state for one call.started event. It pins the
// materialized subworkflow input to the durable CAS ref that was committed in
// the event, plus the runtime resolutions scoped to that call.
//
// Input is the materialized JSON object from InputRef. READ-ONLY (callers MUST
// NOT mutate — same caveat as NodeResult.Outputs).
type CallStartedRecord struct {
	Input      map[string]any
	InputRef   string
	InputFiles map[string]string
	Runtimes   []ResolvedRuntime
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
	Transcript agent.ThreadTurn  // materialized from TranscriptRef by Fold (continues: threading). READ-ONLY. Zero value when the step didn't participate.
}

// RunState is the in-memory fold of the log: the interpreter consults it to skip
// committed nodes, the template evaluator consults it to resolve refs.
//
// Built by Fold (engine/fold.go). The same code path serves first-run (empty log →
// empty RunState) and resume (folded log → populated RunState).
//
// Phase 2 + Phase 3 complete. All planned Phase 3 fields have landed: gate
// attempts (slice 3.3), map items (slice 3.4), and signals/pause/cancel
// (slice 3.5).
// RunState.Epoch ≠ state.Event.Epoch — see comment on the Epoch field below.
type RunState struct {
	RunID          string
	WorkflowDigest string
	Input          map[string]any // resolved from run.started.Data.input_ref via Blobs.Get
	// Assets is the recorded run-start asset manifest. Fold restores it from
	// run.started without dereferencing refs; engine.Run may also seed it from
	// RunOptions for first-run execution before any resume fold exists.
	Assets map[string]RunStartedAsset
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

	// MapItems records the per-item status for every map that has committed
	// at least one map.item event. Slice 3.4 addition; map path → ordered
	// slice of MapItemRecord in arrival order (the order RecordMapItem was
	// called, which mirrors log-append order). NOT N-ascending: concurrent
	// item-body goroutines may commit out-of-N-order, so item N=3 can land
	// in the slice before N=1. Pinned by TestRunStateMapItemsRoundTrip.
	//
	// LookupByN(N) helpers are NOT provided — the map handler walks the
	// slice (typically ≤ Concurrency items in flight) when it needs to
	// check "is item N already committed?"
	//
	// The template evaluator's `<as>` resolution scope (engine/scope.go)
	// reads MapItems to find the bound value for an enclosing map's item.
	MapItems map[string][]MapItemRecord

	// Signals records per-name signal deliveries observationally — slice 3.5
	// addition. Signal name → ordered slice of SignalEntry{Seq, PayloadRef}.
	// Fold rebuilds from signal.received events on resume; the await step
	// (engine/signal_step.go) appends to it after each commit. PURELY
	// OBSERVATIONAL: no handler reads it for control flow (the path-keyed
	// SignalReceivedAt below is the half-commit-resume lookup). Phase 6 obs
	// will project this as a per-signal delivery timeline.
	//
	// Each Append is single-writer (interpreter is the only writer); concurrent
	// Lookups are safe under the mu.
	Signals map[string][]SignalEntry

	// CallStarted records durable subworkflow-call input pins. Call path →
	// materialized input + input ref + per-call runtime resolutions. Built by
	// Fold from call.started events; runtime writers mirror fresh commits via
	// RecordCallStarted.
	CallStarted map[string]CallStartedRecord

	// Paused is the latest non-cleared pause marker. Nil if no pause is
	// active. Slice 3.5 addition.
	Paused *PauseMarker

	// Cancelled is true iff a run.cancelled event has been folded in OR the
	// background poller (engine/controls.go) detected cancel.json mid-run.
	Cancelled bool

	// CancelReason is the operator-supplied free-text from cancel.json's
	// Reason field. Set by the poller alongside Cancelled. engine.Run reads
	// it when appending the run.cancelled event.
	CancelReason string

	// SignalReceivedAt is the path-keyed half-commit-resume mechanism (slice
	// 3.5 design Q7). Populated by Fold from signal.received events; read by
	// runSignalStep before calling broker.Receive — if a SignalReceivedEntry
	// exists for the await's path, the prior run committed signal.received
	// but not node.completed; the handler skips the Receive call + the
	// signal.received append and writes only node.completed.
	SignalReceivedAt map[string]SignalReceivedEntry

	// SnapshotRefs maps a container name to its latest committed snapshot ref
	// (snapshot:workspace containers only); resume restores each from this ref.
	// Slice 7.1 addition. Built by Fold from node.completed events' SnapshotRef
	// / Container fields — last write wins per container (a container is
	// snapshotted at every commit, so the last one is the resume point). Empty
	// for non-snapshot containers and pre-Phase-7 logs (the event fields are
	// omitempty, so the Fold guard skips them).
	SnapshotRefs map[string]string

	// SelectedSkills records skills.selected decisions by runtime node path.
	// Agent steps consult it on resume to replay routed skill IDs without
	// re-running the router against a possibly changed query.
	SelectedSkills map[string]SkillsSelectedData

	// continues: threading derivations — built once per run from wf (whole-graph
	// walks), reused by every runAgentStep that assembles a thread. Guarded by
	// their own sync.Once so a parallel/map fan-out computes each once, race-free
	// (distinct from mu, which guards the mutable run-state maps).
	threadOnce      sync.Once
	stepPathIdx     map[string]string
	agentStepIdx    map[string]*ir.AgentStep
	threadTargetSet map[string]bool

	// mu serializes access to Completed / Branches / LoopIters / GateAttempts
	// / MapItems / Signals / CallStarted / SignalReceivedAt / SelectedSkills / Paused /
	// Cancelled / CancelReason.
	// Phase 2 callers were single-threaded; Phase 3 slice 3.2
	// (parallel) introduced concurrent branch goroutines, and slice 3.4 (map)
	// adds concurrent item-body goroutines.
	//
	// Slice 3.2+ callers MUST use the accessor methods (LookupCompleted /
	// RecordCompleted / LookupBranch / RecordBranch / LookupLoopIters /
	// RecordLoopIter / LookupGateAttempts / RecordGateAttempt /
	// LookupMapItems / RecordMapItem / UpdateMapItemValue / AppendSignal /
	// LookupSignals / LookupCallStarted / RecordCallStarted /
	// LookupSignalReceivedAt / RecordSignalReceivedAt / SetPaused /
	// LookupPaused / SetCancelled / IsCancelled / SetCancelReason /
	// LookupCancelReason). Direct field
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
		RunID:            runID,
		WorkflowDigest:   workflowDigest,
		Input:            input,
		Epoch:            1,
		Completed:        map[string]NodeResult{},
		Branches:         map[string]string{},
		LoopIters:        map[string]int{},
		GateAttempts:     map[string][]AttemptResult{},
		MapItems:         map[string][]MapItemRecord{},
		Signals:          map[string][]SignalEntry{},
		CallStarted:      map[string]CallStartedRecord{},
		SignalReceivedAt: map[string]SignalReceivedEntry{},
		SnapshotRefs:     map[string]string{},
		SelectedSkills:   map[string]SkillsSelectedData{},
		// Paused, Cancelled, CancelReason — zero values are correct
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

// LookupMapItems returns a SHALLOW COPY of the per-item slice recorded for
// mapPath (a fresh slice header pointing at copies of the MapItemRecord values;
// the MapItemRecord.ItemValue interfaces still alias the underlying maps).
// Returns nil if no item has been recorded yet. Thread-safe.
//
// Why a copy, not the live backing array (slice 3.4 design Q3 + C1):
// updateMapItemStatus mutates the live backing array under mu when a body
// goroutine terminates. If LookupMapItems returned the live header, concurrent
// resolveAsBinding callers reading other items' fields would race with that
// update (the mutation is past the LookupMapItems mu.Unlock). A shallow copy
// gives the caller an immutable snapshot — race-clean under -race.
//
// READ-ONLY: callers MUST NOT mutate the returned slice or the
// MapItemRecord.ItemValue maps within it (aliasing through `any` is shared).
// Pinned by TestMapItemRecordCopyIsShallow (ItemValue maps are aliased; the
// slice header is fresh).
func (rs *RunState) LookupMapItems(mapPath string) []MapItemRecord {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	src := rs.MapItems[mapPath]
	if src == nil {
		return nil
	}
	cp := make([]MapItemRecord, len(src))
	copy(cp, src)
	return cp
}

// RecordMapItem appends mr to the slice at mapPath. The map executor
// (engine/map.go) calls this AFTER a successful Log.Append + Log.Sync of the
// corresponding map.item event — in-memory state mirrors the durable log,
// not the other way around. Thread-safe.
func (rs *RunState) RecordMapItem(mapPath string, mr MapItemRecord) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.MapItems[mapPath] = append(rs.MapItems[mapPath], mr)
}

// UpdateMapItemValue sets the ItemValue field of the existing MapItemRecord
// at (mapPath, n). Used post-resume by the map executor: Fold rebuilds
// MapItems from map.item events with ItemValue:nil; the executor re-evaluates
// `over` and calls this to populate ItemValue before the body's templates
// resolve `<as>` refs (Design Q3).
//
// No-op if no record at (mapPath, n) exists — UpdateMapItemValue is an
// in-memory mirror update, not a writer of truth. Thread-safe.
func (rs *RunState) UpdateMapItemValue(mapPath string, n int, value any) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	items := rs.MapItems[mapPath]
	for i, mr := range items {
		if mr.N == n {
			items[i].ItemValue = value
			return
		}
	}
}

// AppendSignal enqueues e on the per-name queue. The interpreter calls this
// AFTER a successful Log.Append + Log.Sync of the corresponding signal.received
// event (in-memory mirrors durable log, not the other way around). Thread-safe.
func (rs *RunState) AppendSignal(name string, e SignalEntry) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.Signals[name] = append(rs.Signals[name], e)
}

// LookupSignals returns the queue for name (nil if no signal has been
// recorded). Thread-safe. READ-ONLY — callers MUST NOT mutate.
func (rs *RunState) LookupSignals(name string) []SignalEntry {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	src := rs.Signals[name]
	if src == nil {
		return nil
	}
	cp := make([]SignalEntry, len(src))
	copy(cp, src)
	return cp
}

// LookupCallStarted returns the call.started record stored for path and a
// present flag. Thread-safe. READ-ONLY — callers MUST NOT mutate the returned
// record's Input map, InputFiles map, or Runtimes slice.
func (rs *RunState) LookupCallStarted(path string) (CallStartedRecord, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rec, ok := rs.CallStarted[path]
	return rec, ok
}

// CallStartedPaths returns the recorded call.started paths in deterministic
// order. Thread-safe; the returned slice is a copy.
func (rs *RunState) CallStartedPaths() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.CallStarted) == 0 {
		return nil
	}
	out := make([]string, 0, len(rs.CallStarted))
	for path := range rs.CallStarted {
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

// RecordCallStarted stores rec for path. The call executor calls this AFTER a
// successful Log.Append + Log.Sync of the corresponding call.started event —
// in-memory state mirrors the durable log, not the other way around.
// Thread-safe.
func (rs *RunState) RecordCallStarted(path string, rec CallStartedRecord) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rec.InputFiles = cloneStringMap(rec.InputFiles)
	rs.CallStarted[path] = rec
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	cp := make(map[string]string, len(src))
	for k, v := range src {
		cp[k] = v
	}
	return cp
}

// LookupSignalReceivedAt returns the SignalReceivedEntry stored for path and
// a present flag. The half-commit-resume mechanism (slice 3.5 design Q7):
// runSignalStep checks this BEFORE calling broker.Receive. Thread-safe.
func (rs *RunState) LookupSignalReceivedAt(path string) (SignalReceivedEntry, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	e, ok := rs.SignalReceivedAt[path]
	return e, ok
}

// LookupSelectedSkills returns the skills.selected payload recorded for path.
// The returned value is a defensive copy; callers may inspect it but must not
// mutate RunState through it.
func (rs *RunState) LookupSelectedSkills(path string) (SkillsSelectedData, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	d, ok := rs.SelectedSkills[path]
	if !ok {
		return SkillsSelectedData{}, false
	}
	return cloneSkillsSelectedData(d), true
}

// RecordSelectedSkills stores the skills.selected payload for path. The
// interpreter calls this only after the corresponding event has been appended
// and synced; Fold uses it when reconstructing state on resume.
func (rs *RunState) RecordSelectedSkills(path string, data SkillsSelectedData) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.SelectedSkills == nil {
		rs.SelectedSkills = map[string]SkillsSelectedData{}
	}
	rs.SelectedSkills[path] = cloneSkillsSelectedData(data)
}

// RecordSignalReceivedAt stores e for path. Used by Fold (engine/fold.go's
// signal.received arm) when reconstructing post-resume state. The runtime
// path also goes through this when committing a fresh signal.received —
// keeps the in-memory mirror current. Thread-safe.
func (rs *RunState) RecordSignalReceivedAt(path string, e SignalReceivedEntry) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.SignalReceivedAt[path] = e
}

// SetPaused sets the in-memory pause marker. nil clears. Thread-safe.
func (rs *RunState) SetPaused(pm *PauseMarker) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.Paused = pm
}

// LookupPaused returns the current marker (nil if not paused). Thread-safe.
// Returns a SHALLOW COPY so concurrent readers can't mutate the live marker.
func (rs *RunState) LookupPaused() *PauseMarker {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.Paused == nil {
		return nil
	}
	cp := *rs.Paused
	return &cp
}

// SetCancelled sets the cancelled flag. Thread-safe.
func (rs *RunState) SetCancelled(v bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.Cancelled = v
}

// IsCancelled reports the cancelled flag. Thread-safe.
func (rs *RunState) IsCancelled() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.Cancelled
}

// SetCancelReason stores the operator-supplied reason from cancel.json. The
// background poller (engine/controls.go) calls this alongside SetCancelled
// when cancel.json is detected. engine.Run reads it when appending the
// run.cancelled event. Thread-safe.
//
// (Field-vs-method naming: the field is `CancelReason`; the accessor is
// `LookupCancelReason` to avoid the Go field/method name collision. Same
// pattern as Paused / LookupPaused.)
func (rs *RunState) SetCancelReason(reason string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.CancelReason = reason
}

// LookupCancelReason returns the stored reason. Empty if no reason was
// supplied or SetCancelReason has not been called. Thread-safe.
func (rs *RunState) LookupCancelReason() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.CancelReason
}

// initThreadIndexes builds the three continues: derivations once. Called via
// the accessors below; the sync.Once makes the whole-graph walks run a single
// time per run regardless of how many concurrent runAgentStep callers hit it.
func (rs *RunState) initThreadIndexes(wf *ir.Workflow) {
	rs.threadOnce.Do(func() {
		rs.stepPathIdx = StepPathIndex(wf)
		rs.agentStepIdx = map[string]*ir.AgentStep{}
		rs.threadTargetSet = map[string]bool{}
		ir.WalkNodes(wf.Graph, "", func(n ir.Node, _ string) {
			as, ok := n.(*ir.AgentStep)
			if !ok {
				return
			}
			rs.agentStepIdx[as.ID] = as
			if as.Continues != "" {
				rs.threadTargetSet[as.Continues] = true
			}
		})
	})
}

// stepPathIndex returns the once-built id→static-path index (continues: assembly).
func (rs *RunState) stepPathIndex(wf *ir.Workflow) map[string]string {
	rs.initThreadIndexes(wf)
	return rs.stepPathIdx
}

// agentStepByID returns the once-built id→*AgentStep index (continues: chain walk).
func (rs *RunState) agentStepByID(wf *ir.Workflow) map[string]*ir.AgentStep {
	rs.initThreadIndexes(wf)
	return rs.agentStepIdx
}

// threadTargets returns the once-built set of ids that appear as some step's
// continues: target (the Commit participates precompute).
func (rs *RunState) threadTargets(wf *ir.Workflow) map[string]bool {
	rs.initThreadIndexes(wf)
	return rs.threadTargetSet
}
