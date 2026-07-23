package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// newGateRig builds engine plumbing for a gate test. Shares scriptedDispatcher
// with try / parallel tests (engine/try_test.go) — the gate-specific addition
// is scriptedResult.outputs, which propagates to DispatchResult.Outputs so
// the gate executor can read a typed verdict from the evaluator step.
//
// The clock is pinned to 2026-01-01 (vs try_test's zero-value default) so
// gate.attempt events get a sane (non-1970) timestamp in the log — only
// matters for log inspection in failing tests; assertions don't depend on it.
func newGateRig(t *testing.T, script map[string]scriptedResult) (*scriptedDispatcher, *state.InMemoryLog, *state.InMemoryBlobs) {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return &scriptedDispatcher{t: t, script: script}, state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
}

// schemaForVerdict returns a JSONSchema accepting the {verified, feedback}
// shape — the Appendix-A verdict shape. The validator (Phase 1.4 AWF1014)
// requires the last evaluator step to declare an output_schema.
func schemaForVerdict() *ir.JSONSchema {
	return &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"verified", "feedback"},
		"properties": map[string]any{
			"verified": map[string]any{"type": "boolean"},
			"feedback": map[string]any{"type": "string"},
		},
	}
}

// TestLastEvaluatorPathMapTerminal: a gate whose evaluate: ends in a *ir.Map
// with a typed reduce (jury-panel Task 2, AWF1014 relaxed) resolves to the
// map's OWN path — bare map[i], no id segment, per ir.PathFor's map addressing
// (mirrors ir/validate_structural.go's nodeHasOutputSchema *Map arm; the two
// predicates must agree on exactly which maps are valid evaluate terminals).
func TestLastEvaluatorPathMapTerminal(t *testing.T) {
	q := ir.Ratio("2")
	g := &ir.Gate{Evaluate: ir.NodeList{
		&ir.CodeStep{ID: "pre", Run: "x", Container: "c"},
		&ir.Map{ID: "jury", Reduce: &ir.Reduce{Quorum: &q, Field: "accept"}},
	}}
	got, err := lastEvaluatorPath(g, "gate[0].evaluate")
	if err != nil {
		t.Fatalf("lastEvaluatorPath: %v", err)
	}
	want := ir.PathFor("gate[0].evaluate", "map", "", 1)
	if got != want {
		t.Errorf("lastEvaluatorPath = %q, want %q", got, want)
	}
}

func TestRunGateSingleAttemptPasses(t *testing.T) {
	until := ir.Expr("{{ evaluate.verified }}")
	g := &ir.Gate{
		Generate: ir.NodeList{
			&ir.CodeStep{ID: "gen1", Run: "echo gen", Container: "c0"},
		},
		Evaluate: ir.NodeList{
			&ir.CodeStep{ID: "eval1", Run: "echo eval", Container: "c0", OutputSchema: schemaForVerdict()},
		},
		Until:       until,
		MaxAttempts: 3,
	}
	disp, lg, blobs := newGateRig(t, map[string]scriptedResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeOK, outputs: map[string]any{"verified": true, "feedback": "all good"}},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("got (%q, %v), want (ok, nil)", oc, err)
	}
	if got := rs.LookupGateAttempts("gate[0]"); len(got) != 1 || got[0].AttemptOutcome != AttemptPassed {
		t.Errorf("GateAttempts = %+v; want one passed attempt", got)
	}
}

func TestRunGateRepairsAndPassesOnAttempt2(t *testing.T) {
	// Attempt 1 returns verified:false; attempt 2 returns verified:true.
	// We script the evaluator to flip its output based on the dispatched ID
	// suffix — the test rig dispatches eval1 twice (one per attempt), so we
	// use a stateful counter.
	until := ir.Expr("{{ evaluate.verified }}")
	g := &ir.Gate{
		Generate: ir.NodeList{
			&ir.CodeStep{ID: "gen1", Run: "echo {{ evaluate.feedback }}", Container: "c0"},
		},
		Evaluate: ir.NodeList{
			&ir.CodeStep{ID: "eval1", Run: "eval", Container: "c0", OutputSchema: schemaForVerdict()},
		},
		Until:       until,
		MaxAttempts: 3,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	evalCount := 0
	disp := &scriptedDispatcher{
		t: t,
		script: map[string]scriptedResult{
			"gen1": {outcome: OutcomeOK},
		},
	}
	// Substitute the eval1 entry per attempt via an adapter:
	disp.script["eval1"] = scriptedResult{} // placeholder — real result chosen below
	wrapper := &perAttemptDispatcher{inner: disp, evalCallback: func() map[string]any {
		evalCount++
		if evalCount == 1 {
			return map[string]any{"verified": false, "feedback": "missing X"}
		}
		return map[string]any{"verified": true, "feedback": "good"}
	}}
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, wrapper, lg, blobs, &clock.Fake{}, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("got (%q, %v), want (ok, nil)", oc, err)
	}
	attempts := rs.LookupGateAttempts("gate[0]")
	if len(attempts) != 2 {
		t.Fatalf("GateAttempts len = %d, want 2", len(attempts))
	}
	if attempts[0].AttemptOutcome != AttemptRejected || attempts[1].AttemptOutcome != AttemptPassed {
		t.Errorf("attempt outcomes = [%q, %q]; want [%q, %q]",
			attempts[0].AttemptOutcome, attempts[1].AttemptOutcome,
			AttemptRejected, AttemptPassed)
	}
}

// perAttemptDispatcher is a wrapper that lets a test return a different
// evaluator output per attempt via a closure. The inner dispatcher's eval1
// script entry is consulted ONLY for {outcome, err}; the Outputs come from
// the callback. (Generator steps go through unchanged.)
type perAttemptDispatcher struct {
	inner        *scriptedDispatcher
	evalCallback func() map[string]any
}

func (d *perAttemptDispatcher) Run(ctx context.Context, intent NodeIntent) (DispatchResult, <-chan container.IOChunk, error) {
	cs, ok := intent.Node.(*ir.CodeStep)
	if !ok {
		d.inner.t.Fatalf("perAttemptDispatcher: non-CodeStep intent %T at path %q", intent.Node, intent.Path)
	}
	if cs.ID == "eval1" {
		zero := 0
		closed := make(chan container.IOChunk)
		close(closed)
		return DispatchResult{Outcome: OutcomeOK, ExitCode: &zero, Outputs: d.evalCallback()}, closed, nil
	}
	return d.inner.Run(ctx, intent)
}

func TestRunGateMaxAttemptsReturnsRejected(t *testing.T) {
	until := ir.Expr("{{ evaluate.verified }}")
	g := &ir.Gate{
		Generate: ir.NodeList{
			&ir.CodeStep{ID: "gen1", Run: "echo gen", Container: "c0"},
		},
		Evaluate: ir.NodeList{
			&ir.CodeStep{ID: "eval1", Run: "eval", Container: "c0", OutputSchema: schemaForVerdict()},
		},
		Until:       until,
		MaxAttempts: 3,
	}
	disp, lg, blobs := newGateRig(t, map[string]scriptedResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeOK, outputs: map[string]any{"verified": false, "feedback": "X"}},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if oc != OutcomeRejected {
		t.Errorf("oc = %q, want %q", oc, OutcomeRejected)
	}
	if err == nil {
		t.Errorf("err = nil, want non-nil (gate rejected after MaxAttempts)")
	}
	if got := rs.LookupGateAttempts("gate[0]"); len(got) != 3 {
		t.Errorf("GateAttempts len = %d, want 3 (every attempt committed before reject)", len(got))
	}
}

func TestRunGateGenerateCrashDoesNotCommitAttempt(t *testing.T) {
	// Generate's only step returns retryable_failure with no retries left.
	// The gate must propagate WITHOUT writing gate.attempt (crash≠verdict).
	until := ir.Expr("{{ evaluate.verified }}")
	g := &ir.Gate{
		Generate: ir.NodeList{
			&ir.CodeStep{ID: "gen1", Run: "fail", Container: "c0", Retry: &ir.RetryPolicy{Attempts: 1}},
		},
		Evaluate: ir.NodeList{
			&ir.CodeStep{ID: "eval1", Run: "eval", Container: "c0", OutputSchema: schemaForVerdict()},
		},
		Until:       until,
		MaxAttempts: 3,
	}
	disp, lg, blobs := newGateRig(t, map[string]scriptedResult{
		"gen1": {outcome: OutcomeRetryableFailure, err: errors.New("gen crashed")},
		// eval1 deliberately not scripted — must NOT be reached.
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if oc == OutcomeOK {
		t.Errorf("oc = ok, want propagation of generate crash")
	}
	if err == nil || !strings.Contains(err.Error(), "gen crashed") {
		t.Errorf("err = %v, want contains \"gen crashed\"", err)
	}
	if got := rs.LookupGateAttempts("gate[0]"); len(got) != 0 {
		t.Errorf("GateAttempts len = %d, want 0 (crash≠verdict — no attempt consumed)", len(got))
	}
	// Verify no gate.attempt event was written.
	events, _ := lg.Fold()
	for _, e := range events {
		if e.Type == EventGateAttempt {
			t.Errorf("unexpected gate.attempt event in log: %+v", e)
		}
	}
}

func TestRunGateEvaluateCrashDoesNotCommitAttempt(t *testing.T) {
	// Generate succeeds; evaluate's first step crashes. Same invariant:
	// no gate.attempt committed.
	until := ir.Expr("{{ evaluate.verified }}")
	g := &ir.Gate{
		Generate: ir.NodeList{
			&ir.CodeStep{ID: "gen1", Run: "echo gen", Container: "c0"},
		},
		Evaluate: ir.NodeList{
			&ir.CodeStep{ID: "eval1", Run: "eval-fail", Container: "c0",
				OutputSchema: schemaForVerdict(), Retry: &ir.RetryPolicy{Attempts: 1}},
		},
		Until:       until,
		MaxAttempts: 3,
	}
	disp, lg, blobs := newGateRig(t, map[string]scriptedResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeRetryableFailure, err: errors.New("eval crashed")},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	_, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "eval crashed") {
		t.Errorf("err = %v, want contains \"eval crashed\"", err)
	}
	if got := rs.LookupGateAttempts("gate[0]"); len(got) != 0 {
		t.Errorf("GateAttempts len = %d, want 0", len(got))
	}
}

func TestRunGateSkipInGenerateEndsGateAsOK(t *testing.T) {
	// Skip in the FIRST generate step → ends WHOLE gate as ok.
	until := ir.Expr("{{ evaluate.verified }}")
	g := &ir.Gate{
		Generate: ir.NodeList{
			&ir.Skip{Reason: "bail from generate"},
		},
		Evaluate: ir.NodeList{
			&ir.CodeStep{ID: "eval1", Run: "eval", Container: "c0", OutputSchema: schemaForVerdict()},
		},
		Until:       until,
		MaxAttempts: 3,
	}
	disp, lg, blobs := newGateRig(t, map[string]scriptedResult{})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("got (%q, %v), want (ok, nil) — skip ends gate as ok per design §D", oc, err)
	}
	events, _ := lg.Fold()
	var skipped bool
	for _, e := range events {
		if e.Type == EventNodeSkipped {
			skipped = true
		}
		if e.Type == EventGateAttempt {
			t.Errorf("gate.attempt committed despite skip-in-generate: %+v", e)
		}
	}
	if !skipped {
		t.Errorf("no node.skipped event in log")
	}
}

// NOTE: TestRunGateSkipInEvaluateEndsGateAsOK was dropped in the critique-pass
// revision. The case it exercised (Skip as the last node of evaluate) is
// AWF1014-invalid (only step kinds with output_schema close the evaluate
// block), so it never reaches the engine in a validated workflow. The
// skip-in-generate test above covers the gate's SkipUnwind target-scope
// detection, which is identical for both subtrees.

func TestRunGateExhaustedRejectedStaysRejected(t *testing.T) {
	// A gate that already spent its full attempt budget (3/3 rejected) and is
	// re-entered on resume stays rejected with NO fresh attempt — the gate never
	// grants a new allotment. Re-running a rejected gate is done by invalidating
	// its subtree with `awf resume --from`, not by the gate itself.
	g := &ir.Gate{
		Generate:    ir.NodeList{&ir.CodeStep{ID: "gen1", Run: "echo gen", Container: "c0"}},
		Evaluate:    ir.NodeList{&ir.CodeStep{ID: "eval1", Run: "eval", Container: "c0", OutputSchema: schemaForVerdict()}},
		Until:       ir.Expr("{{ evaluate.verified }}"),
		MaxAttempts: 3,
	}
	disp, lg, blobs := newGateRig(t, map[string]scriptedResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeOK, outputs: map[string]any{"verified": true, "feedback": "fixed"}},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	for n := 1; n <= 3; n++ {
		rs.RecordGateAttempt("gate[0]", AttemptResult{N: n, AttemptOutcome: AttemptRejected, Verdict: map[string]any{"verified": false}})
	}
	oc, _ := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if oc != OutcomeRejected {
		t.Fatalf("oc = %q, want rejected (budget spent)", oc)
	}
	if got := rs.LookupGateAttempts("gate[0]"); len(got) != 3 {
		t.Fatalf("attempts len = %d, want 3 (no fresh attempt on a spent gate)", len(got))
	}
}

func TestRunGateMidResumeStartsAtNextAttempt(t *testing.T) {
	// Pre-populate RunState.GateAttempts with two prior attempts (both rejected).
	// runGate should start at attempt 3, NOT attempt 1.
	until := ir.Expr("{{ evaluate.verified }}")
	g := &ir.Gate{
		Generate: ir.NodeList{
			&ir.CodeStep{ID: "gen1", Run: "echo gen", Container: "c0"},
		},
		Evaluate: ir.NodeList{
			&ir.CodeStep{ID: "eval1", Run: "eval", Container: "c0", OutputSchema: schemaForVerdict()},
		},
		Until:       until,
		MaxAttempts: 5,
	}
	disp, lg, blobs := newGateRig(t, map[string]scriptedResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeOK, outputs: map[string]any{"verified": true, "feedback": "good"}},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordGateAttempt("gate[0]", AttemptResult{N: 1, AttemptOutcome: AttemptRejected, Verdict: map[string]any{"verified": false, "feedback": "X"}})
	rs.RecordGateAttempt("gate[0]", AttemptResult{N: 2, AttemptOutcome: AttemptRejected, Verdict: map[string]any{"verified": false, "feedback": "Y"}})

	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("got (%q, %v), want (ok, nil)", oc, err)
	}
	got := rs.LookupGateAttempts("gate[0]")
	if len(got) != 3 {
		t.Errorf("attempts len = %d, want 3 (2 pre-resume + 1 fresh)", len(got))
	}
	if got[2].N != 3 {
		t.Errorf("fresh attempt N = %d, want 3 (resume must continue numbering, not restart)", got[2].N)
	}
}

// schemaForDraft is the generate step's typed-output schema for the forwarding
// tests — a single string field the external reference reads.
func schemaForDraft() *ir.JSONSchema {
	return &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"punti"},
		"properties": map[string]any{
			"punti": map[string]any{"type": "string"},
		},
	}
}

// perAttemptGenDispatcher varies BOTH the generator's and the evaluator's typed
// output by attempt number, which perAttemptDispatcher (eval-only) cannot do.
// Attempt 1: gen emits "draft-1", eval rejects. Attempt 2: gen emits "draft-2",
// eval passes. So the ACCEPTED value is "draft-2" and the value a naive
// first-attempt or static lookup would return is "draft-1".
type perAttemptGenDispatcher struct {
	t        *testing.T
	attempts int
}

func (d *perAttemptGenDispatcher) Run(ctx context.Context, intent NodeIntent) (DispatchResult, <-chan container.IOChunk, error) {
	cs, ok := intent.Node.(*ir.CodeStep)
	if !ok {
		d.t.Fatalf("perAttemptGenDispatcher: non-CodeStep intent %T at path %q", intent.Node, intent.Path)
	}
	zero := 0
	closed := make(chan container.IOChunk)
	close(closed)
	switch cs.ID {
	case "gen1":
		d.attempts++
		return DispatchResult{
			Outcome:  OutcomeOK,
			ExitCode: &zero,
			Outputs:  map[string]any{"punti": fmt.Sprintf("draft-%d", d.attempts)},
		}, closed, nil
	case "eval1":
		passed := d.attempts >= 2
		return DispatchResult{
			Outcome:  OutcomeOK,
			ExitCode: &zero,
			Outputs:  map[string]any{"verified": passed, "feedback": "more detail"},
		}, closed, nil
	}
	d.t.Fatalf("perAttemptGenDispatcher: unexpected step %q", cs.ID)
	return DispatchResult{}, closed, nil
}

// forwardingGate is the gate under test: gen1 produces typed output, eval1
// judges it, until reads the verdict.
func forwardingGate() *ir.Gate {
	return &ir.Gate{
		Generate: ir.NodeList{
			&ir.CodeStep{ID: "gen1", Run: "gen {{ evaluate.feedback }}", Container: "c0", OutputSchema: schemaForDraft()},
		},
		Evaluate: ir.NodeList{
			&ir.CodeStep{ID: "eval1", Run: "eval", Container: "c0", OutputSchema: schemaForVerdict()},
		},
		Until:       ir.Expr("{{ evaluate.verified }}"),
		MaxAttempts: 3,
	}
}

// TestGateForwardsAcceptedAttemptNotFirstAttempt is spec test (c). Attempt 1 is
// rejected, attempt 2 is accepted; a reference from outside the gate must read
// attempt 2's value.
func TestGateForwardsAcceptedAttemptNotFirstAttempt(t *testing.T) {
	g := forwardingGate()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)

	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, &perAttemptGenDispatcher{t: t}, lg, blobs, clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("runGate = (%q, %v), want (ok, nil)", oc, err)
	}
	attempts := rs.LookupGateAttempts("gate[0]")
	if len(attempts) != 2 || attempts[0].AttemptOutcome != AttemptRejected || attempts[1].AttemptOutcome != AttemptPassed {
		t.Fatalf("attempts = %+v, want [rejected, passed]", attempts)
	}

	// The reference site is OUTSIDE the gate.
	sc := NewScope(rs, wf, "after_gate")
	v, rerr := sc.Resolve(mustParseRef(t, "step.gen1.punti"))
	if rerr != nil {
		t.Fatalf("step.gen1.punti from outside the gate: %v", rerr)
	}
	if v != "draft-2" {
		t.Errorf("step.gen1.punti = %v, want \"draft-2\" (the ACCEPTED attempt, not attempt-1's \"draft-1\")", v)
	}
}

// TestGateForwardingIsFoldStableAcrossResume is spec test (e). Rebuilding
// RunState from the journal must resolve the identical value — read-time
// resolution depends only on committed state, so resume cannot change it.
func TestGateForwardingIsFoldStableAcrossResume(t *testing.T) {
	g := forwardingGate()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)

	// Fold requires the log's first event to be run.started (engine/fold.go);
	// runGate itself never writes it — only the interpreter's Run() does —
	// so the resume path under test needs it seeded here.
	if err := lg.Append(state.Event{Type: EventRunStarted, Data: mustJSON(RunStartedData{RunID: "run-x", WorkflowDigest: "digest"})}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}

	if oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, &perAttemptGenDispatcher{t: t}, lg, blobs, clk, nil, nil); oc != OutcomeOK || err != nil {
		t.Fatalf("runGate = (%q, %v), want (ok, nil)", oc, err)
	}
	live, err := NewScope(rs, wf, "after_gate").Resolve(mustParseRef(t, "step.gen1.punti"))
	if err != nil {
		t.Fatalf("live resolve: %v", err)
	}

	// Rebuild RunState purely by folding the journal — the resume path.
	events, ferr := lg.Fold()
	if ferr != nil {
		t.Fatalf("lg.Fold: %v", ferr)
	}
	rs2, foldErr := Fold(events, blobs)
	if foldErr != nil {
		t.Fatalf("engine.Fold: %v", foldErr)
	}
	replayed, rerr := NewScope(rs2, wf, "after_gate").Resolve(mustParseRef(t, "step.gen1.punti"))
	if rerr != nil {
		t.Fatalf("post-fold resolve: %v", rerr)
	}
	if replayed != live {
		t.Errorf("post-fold value = %v, live value = %v; gate forwarding must be fold-stable", replayed, live)
	}
	if replayed != "draft-2" {
		t.Errorf("post-fold value = %v, want \"draft-2\"", replayed)
	}
}
