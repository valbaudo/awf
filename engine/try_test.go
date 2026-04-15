package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// scriptedDispatcher is a tiny test dispatcher. For each Run call it consults
// the script keyed by step id and returns the scripted outcome / error.
// Used by the Try matrix tests.
type scriptedDispatcher struct {
	t      *testing.T
	script map[string]scriptedResult // step id → result
}

type scriptedResult struct {
	outcome Outcome
	err     error // if non-nil, the dispatcher returns this error directly
}

func (d *scriptedDispatcher) Run(ctx context.Context, intent NodeIntent) (DispatchResult, <-chan container.IOChunk, error) {
	cs, ok := intent.Node.(*ir.CodeStep)
	if !ok {
		d.t.Fatalf("scriptedDispatcher: non-CodeStep intent %T at path %q", intent.Node, intent.Path)
	}
	res, ok := d.script[cs.ID]
	if !ok {
		d.t.Fatalf("scriptedDispatcher: no script entry for step %q at path %q", cs.ID, intent.Path)
	}
	closedCh := make(chan container.IOChunk)
	close(closedCh)
	if res.err != nil {
		return DispatchResult{Outcome: res.outcome}, closedCh, res.err
	}
	return DispatchResult{Outcome: res.outcome, ExitCode: intPtr(0)}, closedCh, nil
}

func intPtr(i int) *int { return &i }

// tryTestRig builds engine plumbing for a Try test.
func tryTestRig(t *testing.T, script map[string]scriptedResult) (*scriptedDispatcher, state.Log, state.Blobs) {
	t.Helper()
	clk := &clock.Fake{}
	logger := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	return &scriptedDispatcher{t: t, script: script}, logger, blobs
}

func TestRunTryDoOK(t *testing.T) {
	// try.do = [step-ok]; catch = []; finally = []  → returns ok
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.CodeStep{ID: "ok-step", Run: "echo"},
		},
	}
	disp, logger, blobs := tryTestRig(t, map[string]scriptedResult{
		"ok-step": {outcome: OutcomeOK},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{try}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runTry(context.Background(), try, "try[0]", wf, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("Try{do-ok}: got (%q, %v); want (ok, nil)", oc, err)
	}
}

func TestRunTryDoFailsNoCatch(t *testing.T) {
	// try.do = [step-fails]; catch = nil; finally = nil → Do's error propagates
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.CodeStep{ID: "fail-step", Run: "exit 1"},
		},
	}
	failErr := errors.New("step exhausted retries")
	disp, logger, blobs := tryTestRig(t, map[string]scriptedResult{
		"fail-step": {outcome: OutcomeRetryableFailure, err: failErr},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{try}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runTry(context.Background(), try, "try[0]", wf, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if oc != OutcomeRetryableFailure {
		t.Errorf("Try{do-fails-no-catch}: outcome got %q, want %q", oc, OutcomeRetryableFailure)
	}
	if err == nil || !strings.Contains(err.Error(), "exhausted retries") {
		t.Errorf("Try{do-fails-no-catch}: err = %v, want contains 'exhausted retries'", err)
	}
}

func TestRunTryDoFailsWithCatch(t *testing.T) {
	// try.do = [fail]; catch = [ok-step]; finally = [] → Catch absorbs, returns ok
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.CodeStep{ID: "fail-step", Run: "exit 1"},
		},
		Catch: ir.NodeList{
			&ir.CodeStep{ID: "catch-step", Run: "echo caught"},
		},
	}
	disp, logger, blobs := tryTestRig(t, map[string]scriptedResult{
		"fail-step":  {outcome: OutcomeRetryableFailure, err: errors.New("transient")},
		"catch-step": {outcome: OutcomeOK},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{try}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runTry(context.Background(), try, "try[0]", wf, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("Try{do-fails-with-catch}: got (%q, %v); want (ok, nil)", oc, err)
	}
}

func TestRunTryDoFailsCatchFails(t *testing.T) {
	// try.do = [fail]; catch = [fail-too]; finally = [] → Catch's error propagates
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.CodeStep{ID: "fail-step", Run: "exit 1"},
		},
		Catch: ir.NodeList{
			&ir.CodeStep{ID: "catch-fail", Run: "exit 2"},
		},
	}
	disp, logger, blobs := tryTestRig(t, map[string]scriptedResult{
		"fail-step":  {outcome: OutcomeRetryableFailure, err: errors.New("do failed")},
		"catch-fail": {outcome: OutcomeRetryableFailure, err: errors.New("catch failed too")},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{try}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runTry(context.Background(), try, "try[0]", wf, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if oc != OutcomeRetryableFailure {
		t.Errorf("Try{catch-fails}: outcome got %q, want %q", oc, OutcomeRetryableFailure)
	}
	if err == nil || !strings.Contains(err.Error(), "catch failed too") {
		t.Errorf("Try{catch-fails}: err = %v, want contains 'catch failed too'", err)
	}
}

func TestRunTryFinallyAlwaysRuns(t *testing.T) {
	// try.do = [ok]; catch = []; finally = [ok] → finally runs, returns ok
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.CodeStep{ID: "do-step", Run: "echo"},
		},
		Finally: ir.NodeList{
			&ir.CodeStep{ID: "finally-step", Run: "echo finally"},
		},
	}
	disp, logger, blobs := tryTestRig(t, map[string]scriptedResult{
		"do-step":      {outcome: OutcomeOK},
		"finally-step": {outcome: OutcomeOK},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{try}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runTry(context.Background(), try, "try[0]", wf, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("Try{finally-always-runs}: got (%q, %v); want (ok, nil)", oc, err)
	}
	if _, done := rs.Completed["try[0].finally.finally-step"]; !done {
		t.Errorf("Try{finally-always-runs}: finally step not in Completed: %+v", rs.Completed)
	}
}

func TestRunTryFinallyErrorSupersedes(t *testing.T) {
	// try.do = [ok]; finally = [fail] → finally's error wins
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.CodeStep{ID: "do-step", Run: "echo"},
		},
		Finally: ir.NodeList{
			&ir.CodeStep{ID: "finally-fail", Run: "exit 1"},
		},
	}
	disp, logger, blobs := tryTestRig(t, map[string]scriptedResult{
		"do-step":      {outcome: OutcomeOK},
		"finally-fail": {outcome: OutcomeRetryableFailure, err: errors.New("finally bombed")},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{try}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runTry(context.Background(), try, "try[0]", wf, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if oc != OutcomeRetryableFailure {
		t.Errorf("Try{finally-fails}: outcome got %q, want %q", oc, OutcomeRetryableFailure)
	}
	if err == nil || !strings.Contains(err.Error(), "finally bombed") {
		t.Errorf("Try{finally-fails}: err = %v, want contains 'finally bombed'", err)
	}
}

func TestRunTrySkipInDoSkipsCatchRunsFinally(t *testing.T) {
	// try.do = [skip]; catch = [should-not-run]; finally = [must-run] → ok, catch skipped, finally ran
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.Skip{Reason: "early exit"},
		},
		Catch: ir.NodeList{
			&ir.CodeStep{ID: "should-not-run", Run: "echo should-not-run"},
		},
		Finally: ir.NodeList{
			&ir.CodeStep{ID: "must-run", Run: "echo finally"},
		},
	}
	disp, logger, blobs := tryTestRig(t, map[string]scriptedResult{
		// "should-not-run" deliberately NOT in script — if Catch runs, scriptedDispatcher.t.Fatalf.
		"must-run": {outcome: OutcomeOK},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{try}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runTry(context.Background(), try, "try[0]", wf, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("Try{skip-in-do}: got (%q, %v); want (ok, nil)", oc, err)
	}
	if _, done := rs.Completed["try[0].finally.must-run"]; !done {
		t.Errorf("Try{skip-in-do}: finally step not in Completed: %+v", rs.Completed)
	}
}

func TestRunTryCtxCancelledAfterFinally(t *testing.T) {
	// ctx cancelled before run → Finally still runs; runTry returns ctx.Err()
	ctx, cancel := context.WithCancel(context.Background())
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.CodeStep{ID: "do-step", Run: "echo"},
		},
		Finally: ir.NodeList{
			&ir.CodeStep{ID: "must-run", Run: "echo finally"},
		},
	}
	disp, logger, blobs := tryTestRig(t, map[string]scriptedResult{
		"do-step":  {outcome: OutcomeOK},
		"must-run": {outcome: OutcomeOK},
	})
	cancel() // cancel BEFORE running; the scripted dispatcher doesn't check ctx, but runTry's final check does
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{try}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runTry(ctx, try, "try[0]", wf, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Try{ctx-cancelled}: err = %v, want errors.Is(context.Canceled) true", err)
	}
	if oc != OutcomeRetryableFailure {
		t.Errorf("Try{ctx-cancelled}: outcome got %q, want %q", oc, OutcomeRetryableFailure)
	}
	if _, done := rs.Completed["try[0].finally.must-run"]; !done {
		t.Errorf("Try{ctx-cancelled}: finally must run even on ctx-cancel: Completed = %+v", rs.Completed)
	}
}
