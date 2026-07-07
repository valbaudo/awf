package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/retry"
	"github.com/valbaudo/awf/state"
)

// TestRunWithRetryJournalsResumeHint verifies that a FAILED session attempt
// carrying DispatchResult.ResumeHint (the stalled live session's key) makes
// RunWithRetry append a resume.hint event beside retry.attempt, and that Fold
// folds it into RunState.ResumeHints — WITHOUT treating it as a committed
// artifact (no node.completed, no Blobs ref) and WITHOUT conflating it with the
// content-addressed SessionRefs.
func TestRunWithRetryJournalsResumeHint(t *testing.T) {
	t.Parallel()
	const hintKey = "sess-key-abc"
	exit := 0
	dsp := &stubDispatcher{results: []stubResult{
		// Attempt 1: a stalled session turn needing cross-process replay.
		{dr: engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure, Err: errors.New("live replay required"), ResumeHint: hintKey}},
		// Attempt 2: recovers.
		{dr: engine.DispatchResult{Outcome: engine.OutcomeOK, ExitCode: &exit}},
	}}

	log := state.NewInMemoryLog(clock.System{})
	// Fold requires run.started as the first event.
	rsData, err := json.Marshal(engine.RunStartedData{RunID: "run-1", WorkflowDigest: "wd"})
	if err != nil {
		t.Fatalf("marshal run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: rsData}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	policy := retry.Policy{Attempts: 3, Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}

	dr, _, err := engine.RunWithRetry(context.Background(), dsp, defaultIntent(), policy, clk, log)
	if err != nil {
		t.Fatalf("RunWithRetry: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", dr.Outcome)
	}

	events, _ := log.Fold()

	// (a) The raw resume.hint event landed with the node path and the key.
	var hints []engine.ResumeHintData
	var hintPaths []string
	for _, e := range events {
		if e.Type == engine.EventResumeHint {
			var d engine.ResumeHintData
			if uerr := json.Unmarshal(e.Data, &d); uerr != nil {
				t.Fatalf("unmarshal resume.hint: %v", uerr)
			}
			hints = append(hints, d)
			hintPaths = append(hintPaths, e.Path)
		}
	}
	if len(hints) != 1 {
		t.Fatalf("resume.hint event count = %d, want 1; full = %+v", len(hints), hints)
	}
	if hints[0].Key != hintKey {
		t.Errorf("resume.hint Key = %q, want %q", hints[0].Key, hintKey)
	}
	if hintPaths[0] != "x" {
		t.Errorf("resume.hint Path = %q, want %q", hintPaths[0], "x")
	}

	// (b) Fold populates RunState.ResumeHints. InMemoryBlobs is empty — if Fold
	// tried to dereference the hint as a Blobs ref, Get would fail, proving it is
	// NOT a content-addressed artifact.
	rs, err := engine.Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if got := rs.ResumeHints["x"]; got != hintKey {
		t.Errorf("rs.ResumeHints[\"x\"] = %q, want %q", got, hintKey)
	}

	// (c) Not conflated with SessionRefs (content-addressed) and not committed.
	if got, ok := rs.SessionRefs["x"]; ok {
		t.Errorf("rs.SessionRefs[\"x\"] = %q, want absent (hint must not leak into SessionRefs)", got)
	}
	for path, ref := range rs.SessionRefs {
		if ref == hintKey {
			t.Errorf("hint key leaked into SessionRefs[%q]", path)
		}
	}
	if _, ok := rs.Completed["x"]; ok {
		t.Error("rs.Completed[\"x\"] present: RunWithRetry must not commit a node for a resume hint")
	}
}
