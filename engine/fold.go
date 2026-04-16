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

	// Pre-allocate the three maps with capacity hints proportional to event count.
	// Phase 1.5 logs grow with event count, not payload size, so len(events) is the
	// right upper bound. The heuristics (÷4, ÷8, ÷8) reflect that most events are
	// node.completed; branches and loop iterations are sparser. Sized hints save the
	// geometric resize cycles for the resume-from-large-log path Phase 6 will exercise.
	rs.Completed = make(map[string]NodeResult, len(events)/4)
	rs.Branches = make(map[string]string, len(events)/8)
	rs.LoopIters = make(map[string]int, len(events)/8)

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
			rs.Completed[e.Path] = nr

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

		default:
			// Observational / future event types ignored by Fold (state effect, if
			// any, comes from a sibling event):
			//   - retry.attempt (2.4 — observational)
			//   - node.failed, run.finished (2.5 — observational; resume refusal
			//     uses these but reads them outside Fold)
			//   - node.skipped (3.1 — observational; the target scope's own
			//     completion event drives RunState)
			// obs (Phase 6) projects these via its own dispatch.
		}
	}

	return rs, nil
}
