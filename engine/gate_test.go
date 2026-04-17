package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// gateTestDispatcher is a minimal Dispatcher used by gate handler tests.
// Fresh type so engine/{try,parallel}_test.go stay untouched per CLAUDE.md
// rule 3. script maps CodeStep.ID → {outcome, optional err, optional outputs}.
type gateTestDispatcher struct {
	t      *testing.T
	script map[string]gateTestResult
	calls  []string // ordered list of dispatched step IDs (for the independence sub-test)
}

type gateTestResult struct {
	outcome Outcome
	err     error
	outputs map[string]any
}

func (d *gateTestDispatcher) Run(ctx context.Context, intent NodeIntent) (DispatchResult, <-chan container.IOChunk, error) {
	cs, ok := intent.Node.(*ir.CodeStep)
	if !ok {
		d.t.Fatalf("gateTestDispatcher: non-CodeStep intent %T at path %q", intent.Node, intent.Path)
	}
	d.calls = append(d.calls, cs.ID)
	res, ok := d.script[cs.ID]
	if !ok {
		d.t.Fatalf("gateTestDispatcher: no script entry for step %q at path %q", cs.ID, intent.Path)
	}
	closed := make(chan container.IOChunk)
	close(closed)
	if res.err != nil {
		return DispatchResult{Outcome: res.outcome}, closed, res.err
	}
	zero := 0
	return DispatchResult{Outcome: res.outcome, ExitCode: &zero, Outputs: res.outputs}, closed, nil
}

func newGateRig(t *testing.T, script map[string]gateTestResult) (*gateTestDispatcher, *state.InMemoryLog, *state.InMemoryBlobs) {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return &gateTestDispatcher{t: t, script: script}, state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
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
	disp, lg, blobs := newGateRig(t, map[string]gateTestResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeOK, outputs: map[string]any{"verified": true, "feedback": "all good"}},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil)
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
	disp := &gateTestDispatcher{
		t: t,
		script: map[string]gateTestResult{
			"gen1": {outcome: OutcomeOK},
		},
	}
	// Substitute the eval1 entry per attempt via an adapter:
	disp.script["eval1"] = gateTestResult{} // placeholder — real result chosen below
	wrapper := &perAttemptDispatcher{inner: disp, evalCallback: func() map[string]any {
		evalCount++
		if evalCount == 1 {
			return map[string]any{"verified": false, "feedback": "missing X"}
		}
		return map[string]any{"verified": true, "feedback": "good"}
	}}
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, wrapper, lg, blobs, &clock.Fake{}, nil)
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
	inner        *gateTestDispatcher
	evalCallback func() map[string]any
}

func (d *perAttemptDispatcher) Run(ctx context.Context, intent NodeIntent) (DispatchResult, <-chan container.IOChunk, error) {
	cs, ok := intent.Node.(*ir.CodeStep)
	if !ok {
		d.inner.t.Fatalf("perAttemptDispatcher: non-CodeStep intent %T at path %q", intent.Node, intent.Path)
	}
	d.inner.calls = append(d.inner.calls, cs.ID)
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
	disp, lg, blobs := newGateRig(t, map[string]gateTestResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeOK, outputs: map[string]any{"verified": false, "feedback": "X"}},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil)
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
	disp, lg, blobs := newGateRig(t, map[string]gateTestResult{
		"gen1": {outcome: OutcomeRetryableFailure, err: errors.New("gen crashed")},
		// eval1 deliberately not scripted — must NOT be reached.
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil)
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
	disp, lg, blobs := newGateRig(t, map[string]gateTestResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeRetryableFailure, err: errors.New("eval crashed")},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	_, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil)
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
	disp, lg, blobs := newGateRig(t, map[string]gateTestResult{})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil)
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
	disp, lg, blobs := newGateRig(t, map[string]gateTestResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeOK, outputs: map[string]any{"verified": true, "feedback": "good"}},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordGateAttempt("gate[0]", AttemptResult{N: 1, AttemptOutcome: AttemptRejected, Verdict: map[string]any{"verified": false, "feedback": "X"}})
	rs.RecordGateAttempt("gate[0]", AttemptResult{N: 2, AttemptOutcome: AttemptRejected, Verdict: map[string]any{"verified": false, "feedback": "Y"}})

	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil)
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
