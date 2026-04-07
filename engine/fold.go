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
// Phase 2 semantics:
//   - The first event (if any) MUST be run.started; it seeds RunID, WorkflowDigest,
//     Input, Epoch=1. A non-run.started first event is corruption or a writer bug.
//   - A second run.started is a fold error — the engine never writes it twice.
//   - Each run.resumed event sets Epoch from its payload.
//   - node.completed populates Completed[path] (the commit record). Outcome is
//     validated via ParseOutcome — unknown values are rejected.
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
//   - Unknown Outcome string in node.completed (corruption — engine only emits
//     the three known values; ParseOutcome catches it).
func Fold(events []state.Event, blobs state.Blobs) (RunState, error) {
	var rs RunState
	if len(events) == 0 {
		return rs, nil
	}
	if events[0].Type != EventRunStarted {
		return RunState{}, fmt.Errorf("engine.Fold: first event must be %s, got %q at seq=%d",
			EventRunStarted, events[0].Type, events[0].Seq)
	}

	seenRunStarted := false
	for _, e := range events {
		switch e.Type {
		case EventRunStarted:
			if seenRunStarted {
				return RunState{}, fmt.Errorf("engine.Fold: duplicate %s at seq=%d (corruption or writer bug)",
					EventRunStarted, e.Seq)
			}
			seenRunStarted = true
			var d RunStartedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return RunState{}, fmt.Errorf("engine.Fold: parse %s at seq=%d: %w",
					EventRunStarted, e.Seq, err)
			}
			rs.RunID = d.RunID
			rs.WorkflowDigest = d.WorkflowDigest
			rs.Epoch = 1
			if d.InputRef != "" {
				raw, err := blobs.Get(d.InputRef)
				if err != nil {
					return RunState{}, fmt.Errorf("engine.Fold: read input ref %q: %w",
						d.InputRef, err)
				}
				var in map[string]any
				if err := json.Unmarshal(raw, &in); err != nil {
					return RunState{}, fmt.Errorf("engine.Fold: parse input blob %q: %w",
						d.InputRef, err)
				}
				rs.Input = in
			}

		case EventRunResumed:
			var d RunResumedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return RunState{}, fmt.Errorf("engine.Fold: parse %s at seq=%d: %w",
					EventRunResumed, e.Seq, err)
			}
			rs.Epoch = d.Epoch

		case EventNodeCompleted:
			var d NodeCompletedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return RunState{}, fmt.Errorf("engine.Fold: parse %s at seq=%d (path=%q): %w",
					EventNodeCompleted, e.Seq, e.Path, err)
			}
			oc, err := ParseOutcome(d.Outcome)
			if err != nil {
				return RunState{}, fmt.Errorf("engine.Fold: %w at path=%q seq=%d", err, e.Path, e.Seq)
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
					return RunState{}, fmt.Errorf("engine.Fold: read outputs ref %q at path=%q: %w",
						d.OutputsRef, e.Path, err)
				}
				var out map[string]any
				if err := json.Unmarshal(raw, &out); err != nil {
					return RunState{}, fmt.Errorf("engine.Fold: parse outputs blob %q at path=%q: %w",
						d.OutputsRef, e.Path, err)
				}
				nr.Outputs = out
			}
			if rs.Completed == nil {
				rs.Completed = make(map[string]NodeResult)
			}
			rs.Completed[e.Path] = nr

		case EventBranchTaken:
			var d BranchTakenData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return RunState{}, fmt.Errorf("engine.Fold: parse %s at seq=%d (path=%q): %w",
					EventBranchTaken, e.Seq, e.Path, err)
			}
			if rs.Branches == nil {
				rs.Branches = make(map[string]string)
			}
			rs.Branches[e.Path] = d.Which

		case EventLoopIter:
			var d LoopIterData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return RunState{}, fmt.Errorf("engine.Fold: parse %s at seq=%d (path=%q): %w",
					EventLoopIter, e.Seq, e.Path, err)
			}
			if rs.LoopIters == nil {
				rs.LoopIters = make(map[string]int)
			}
			rs.LoopIters[e.Path] = d.N

		default:
			// Future event types not in 2.1's dispatch (retry.attempt, node.started /
			// node.failed / run.finished from 2.4/2.5; signal.received / map.item / ...
			// from later phases) — ignored. obs (Phase 6) projects them via its own
			// dispatch.
		}
	}

	return rs, nil
}
