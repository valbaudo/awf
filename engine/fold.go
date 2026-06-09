package engine

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/state"
)

// Fold replays a committed-event sequence into a RunState. The events come from
// state.Log.Fold(); per Phase 1.5's log contract (state/log.go — OpenLog truncates
// the file at the first short read / CRC mismatch), every record arriving here is
// CRC-valid and JSON-decodable. Blobs is consulted to materialize Input (from
// run.started.InputRef) and per-step Outputs (from node.completed.OutputsRef) into
// typed map[string]any values.
//
// (Spec §A sketches `FoldLog(log state.Log)`; the name was changed to `Fold` with a
// `[]state.Event` argument for testability — callers do
// `events, _ := log.Fold(); rs, _ := engine.Fold(events, blobs)`. Same semantics, no
// extra `state.Log` dependency in unit tests.)
//
// Phase 2 semantics:
//   - The first event (if any) MUST be run.started; it seeds RunID, WorkflowDigest,
//     Input, Epoch=1. A non-run.started first event is corruption or a writer bug.
//   - A second run.started is a fold error — the engine never writes it twice.
//   - Each run.resumed event sets Epoch from its payload.
//   - node.completed populates Completed[path] (the commit record). Outcome is
//     validated via ParseOutcome AND additionally required to be OutcomeOK — only
//     ok-steps commit, per spec §8 + CLAUDE.md "a 'done' record must never reference
//     [an incomplete] state."
//   - branch.taken populates Branches[path] (which side of an if).
//   - loop.iter populates LoopIters[path] (events are seq-ordered, so the latest
//     assignment gives the max — which is correct, since loops only advance forward).
//   - gate.attempt populates GateAttempts[path] (slice 3.3) — verdict_ref dereferenced
//     via Blobs.Get; events arrive in seq-order so append-order is the natural
//     attempt-order. Missing verdict_ref is a hard error (spec §8 commit-atomicity
//     invariant — the writer must Put the verdict blob BEFORE appending gate.attempt).
//   - map.item populates MapItems[path] (slice 3.4) — MapItemRecord{N, Status,
//     ItemValue: nil}. ItemValue is NOT in the wire format (derived from `over` per
//     Design Q3); the runtime fills it via UpdateMapItemValue BEFORE body
//     re-execution on resume. Append-order = arrival-order; items may commit
//     out-of-N-order because the map dispatches concurrently (spec §5.7).
//   - signal.received populates Signals[d.Name] (observational queue) AND
//     SignalReceivedAt[e.Path] (path-keyed half-commit-resume entry — slice
//     3.5 design Q7). REFS ONLY — no Blobs.Get inside Fold; payloads are
//     materialized at use to support non-object payloads from unschema'd
//     signal steps (C6 fix).
//   - run.paused is OBSERVATIONAL — Fold IGNORES it (default arm, same as
//     node.failed). rs.Paused is a runtime-only flag set exclusively by
//     engine.Run's live pollControls goroutine (C7 fix — avoids stale-Paused
//     bug on resume).
//   - run.cancelled sets Cancelled to true (slice 3.5; TERMINAL — cli/resume.go
//     refuses).
//   - Anything else (future event types not yet written by Phase 2 slices) → ignored.
//
// Errors (any fold error → resume cannot proceed safely):
//   - Non-run.started first event / duplicate run.started.
//   - Malformed Data (JSON parse failure on a known event type).
//   - Blobs.Get failure for a referenced blob (broken §8 atomicity invariant —
//     a node.completed referenced a missing artifact).
//   - Unknown Outcome string in node.completed (corruption — ParseOutcome catches it).
//   - Non-`ok` Outcome in node.completed (corruption — only ok-steps commit; the
//     extra check beyond ParseOutcome closes a gap a bare ParseOutcome would miss).
func Fold(events []state.Event, blobs state.Blobs) (*RunState, error) {
	rs := &RunState{}
	if len(events) == 0 {
		return rs, nil
	}
	if events[0].Type != EventRunStarted {
		return nil, fmt.Errorf("engine.Fold: first event must be %s, got %q at seq=%d",
			EventRunStarted, events[0].Type, events[0].Seq)
	}

	// Pre-allocate the four maps with capacity hints proportional to event count.
	// Phase 1.5 logs grow with event count, not payload size, so len(events) is the
	// right upper bound. The heuristics (÷4, ÷8, ÷8, ÷16) reflect that most events
	// are node.completed; branches, loop iterations, and gate attempts are sparser.
	// Sized hints save the geometric resize cycles for the resume-from-large-log path
	// Phase 6 will exercise.
	rs.Completed = make(map[string]NodeResult, len(events)/4)
	rs.Branches = make(map[string]string, len(events)/8)
	rs.LoopIters = make(map[string]int, len(events)/8)
	rs.GateAttempts = make(map[string][]AttemptResult, len(events)/16) // sparse — gates are uncommon
	rs.MapItems = make(map[string][]MapItemRecord, len(events)/16)     // sparse — maps are uncommon
	rs.Signals = make(map[string][]SignalEntry, len(events)/16)        // sparse — signals are uncommon
	rs.SignalReceivedAt = make(map[string]SignalReceivedEntry, len(events)/16)
	rs.SnapshotRefs = make(map[string]string) // slice 7.1 — snapshot:workspace containers only; sparse

	seenRunStarted := false
	for _, e := range events {
		switch e.Type {
		case EventRunStarted:
			if seenRunStarted {
				return nil, fmt.Errorf("engine.Fold: duplicate %s at seq=%d (corruption or writer bug)",
					EventRunStarted, e.Seq)
			}
			seenRunStarted = true
			var d RunStartedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("engine.Fold: parse %s at seq=%d: %w",
					EventRunStarted, e.Seq, err)
			}
			rs.RunID = d.RunID
			rs.WorkflowDigest = d.WorkflowDigest
			rs.Assets = d.Assets
			rs.Epoch = 1
			if d.InputRef != "" {
				raw, err := blobs.Get(d.InputRef)
				if err != nil {
					return nil, fmt.Errorf("engine.Fold: read input ref %q: %w",
						d.InputRef, err)
				}
				var in map[string]any
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, fmt.Errorf("engine.Fold: parse input blob %q: %w",
						d.InputRef, err)
				}
				rs.Input = in
			}

		case EventRunResumed:
			var d RunResumedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("engine.Fold: parse %s at seq=%d: %w",
					EventRunResumed, e.Seq, err)
			}
			rs.Epoch = d.Epoch
			// NB: no Paused-clearing here. Paused is no longer Fold-populated
			// (C7 fix); no clearing needed.

		case EventNodeCompleted:
			var d NodeCompletedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("engine.Fold: parse %s at seq=%d (path=%q): %w",
					EventNodeCompleted, e.Seq, e.Path, err)
			}
			oc, err := ParseOutcome(d.Outcome)
			if err != nil {
				return nil, fmt.Errorf("engine.Fold: %w at path=%q seq=%d", err, e.Path, e.Seq)
			}
			// Spec §8 + CLAUDE.md commit invariant: only ok-steps commit. A node.completed
			// with a non-ok outcome means corruption or a §8 violation by the writer.
			// ParseOutcome alone would accept retryable_failure / permanent_failure here,
			// which is wider than the invariant — this check closes the gap.
			if oc != OutcomeOK {
				return nil, fmt.Errorf("engine.Fold: %s at path=%q seq=%d has outcome %q, only %q commits (spec §8)",
					EventNodeCompleted, e.Path, e.Seq, oc, OutcomeOK)
			}
			nr := NodeResult{
				Outcome:    oc,
				ExitCode:   d.ExitCode,
				OutputsRef: d.OutputsRef,
				Files:      d.Files,
			}
			if d.OutputsRef != "" {
				raw, err := blobs.Get(d.OutputsRef)
				if err != nil {
					return nil, fmt.Errorf("engine.Fold: read outputs ref %q at path=%q: %w",
						d.OutputsRef, e.Path, err)
				}
				var out map[string]any
				if err := json.Unmarshal(raw, &out); err != nil {
					return nil, fmt.Errorf("engine.Fold: parse outputs blob %q at path=%q: %w",
						d.OutputsRef, e.Path, err)
				}
				nr.Outputs = out
			}
			nr.StdoutRef = d.StdoutRef
			if d.StdoutRef != "" {
				raw, err := blobs.Get(d.StdoutRef)
				if err != nil {
					return nil, fmt.Errorf("engine.Fold: read stdout ref %q at path=%q: %w",
						d.StdoutRef, e.Path, err)
				}
				nr.Stdout = raw
			}
			if d.TranscriptRef != "" {
				raw, err := blobs.Get(d.TranscriptRef)
				if err != nil {
					return nil, fmt.Errorf("engine.Fold: read transcript ref %q at path=%q: %w",
						d.TranscriptRef, e.Path, err)
				}
				if err := json.Unmarshal(raw, &nr.Transcript); err != nil {
					return nil, fmt.Errorf("engine.Fold: parse transcript blob %q at path=%q: %w",
						d.TranscriptRef, e.Path, err)
				}
			}
			rs.Completed[e.Path] = nr
			if d.SnapshotRef != "" && d.Container != "" {
				rs.SnapshotRefs[d.Container] = d.SnapshotRef // last write wins = latest commit
			}

		case EventBranchTaken:
			var d BranchTakenData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("engine.Fold: parse %s at seq=%d (path=%q): %w",
					EventBranchTaken, e.Seq, e.Path, err)
			}
			rs.Branches[e.Path] = d.Which

		case EventLoopIter:
			var d LoopIterData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("engine.Fold: parse %s at seq=%d (path=%q): %w",
					EventLoopIter, e.Seq, e.Path, err)
			}
			rs.LoopIters[e.Path] = d.N

		case EventGateAttempt:
			var d GateAttemptData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("engine.Fold: parse %s at seq=%d (path=%q): %w",
					EventGateAttempt, e.Seq, e.Path, err)
			}
			ar := AttemptResult{
				N:              d.N,
				AttemptOutcome: d.AttemptOutcome,
			}
			if d.VerdictRef != "" {
				raw, err := blobs.Get(d.VerdictRef)
				if err != nil {
					return nil, fmt.Errorf("engine.Fold: read verdict_ref %q at path=%q seq=%d: %w",
						d.VerdictRef, e.Path, e.Seq, err)
				}
				var v map[string]any
				if err := json.Unmarshal(raw, &v); err != nil {
					return nil, fmt.Errorf("engine.Fold: parse verdict blob %q at path=%q: %w",
						d.VerdictRef, e.Path, err)
				}
				ar.Verdict = v
			}
			rs.GateAttempts[e.Path] = append(rs.GateAttempts[e.Path], ar)

		case EventMapItem:
			var d MapItemData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("engine.Fold: parse %s at seq=%d (path=%q): %w",
					EventMapItem, e.Seq, e.Path, err)
			}
			// ItemValue stays nil — Design Q3: the runtime re-evaluates `over` on
			// re-entry and fills via UpdateMapItemValue before body re-exec.
			rs.MapItems[e.Path] = append(rs.MapItems[e.Path], MapItemRecord{
				N:           d.N,
				Status:      d.Status,
				ImageDigest: d.ImageDigest,
				Reason:      d.Reason,
				// ItemValue: nil (zero-value) — re-derived from `over` on re-entry.
			})

		case EventMapFrontier:
			// SP5: a prune map commits its WHOLE per-item disposition as one
			// atomic event. Replay every item's status verbatim — the frontier is
			// NEVER re-derived (resume safety, see EventMapFrontier). Same
			// MapItemRecord shape as the map.item arm; ItemValue stays nil
			// (re-derived from `over` on re-entry, Design Q3).
			var d MapFrontierData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("engine.Fold: parse %s at seq=%d (path=%q): %w",
					EventMapFrontier, e.Seq, e.Path, err)
			}
			for _, it := range d.Items {
				rs.MapItems[e.Path] = append(rs.MapItems[e.Path], MapItemRecord{
					N:           it.N,
					Status:      it.Status,
					ImageDigest: it.ImageDigest,
					Reason:      it.Reason,
				})
			}

		case EventSignalReceived:
			var d SignalReceivedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return nil, fmt.Errorf("engine.Fold: parse %s at seq=%d (path=%q): %w",
					EventSignalReceived, e.Seq, e.Path, err)
			}
			// REFS ONLY — no Blobs.Get here (C6 fix). Payloads are
			// materialized at use (signal_step's half-commit-resume path
			// re-derives via Blobs.Get + ValidateAgainstSchema; Phase 6 obs
			// materializes on demand). Avoids the "non-object payload breaks
			// json.Unmarshal into map[string]any" bug for unschema'd signals.
			//
			// Observational per-name queue (Phase 6 obs reads this).
			rs.Signals[d.Name] = append(rs.Signals[d.Name], SignalEntry{
				Seq:        d.Seq,
				PayloadRef: d.PayloadRef,
			})
			// Path-keyed half-commit-resume entry (slice 3.5 design Q7).
			// Only populated for events with a non-empty Path — defense-in-depth
			// against malformed events. signal.received events ALWAYS carry the
			// step's runtime path (runSignalStep sets it on Append), so a missing
			// path here is corruption.
			if e.Path != "" {
				rs.SignalReceivedAt[e.Path] = SignalReceivedEntry{
					Seq:        d.Seq,
					PayloadRef: d.PayloadRef,
				}
			}

		case EventRunCancelled:
			// Terminal — set the flag. cli/resume.go's 4-class refusal catches
			// this first; defense-in-depth here so engine.Run can refuse to
			// start a run that was already cancelled (e.g. if cli/resume.go's
			// refusal check was bypassed by a test or future tool).
			rs.Cancelled = true

		default:
			// Observational / future event types ignored by Fold (state effect, if
			// any, comes from a sibling event):
			//   - retry.attempt (2.4 — observational)
			//   - node.failed, run.finished (2.5 — observational; resume refusal
			//     uses these but reads them outside Fold)
			//   - node.skipped (3.1 — observational; the target scope's own
			//     completion event drives RunState)
			//   - agent.event (5.2 — observational; Phase 6 obs projects as OTel
			//     span events; resume reconstructs RunState from node.completed only)
			// obs (Phase 6) projects these via its own dispatch.
		}
	}

	return rs, nil
}
