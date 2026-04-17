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

// scriptedDispatcher is a tiny test dispatcher shared by the Try (slice 3.1)
// and Parallel (slice 3.2) handler tests. For each Run call it consults the
// script keyed by step id and returns the scripted outcome / error.
//
// If a result's ctxAware flag is set, Run pre-checks ctx.Err() and returns
// (RetryableFailure, ctx.Err()) without consulting the script — mirrors what
// container.Fake does. The ctx-unaware path is the default (Phase 2 / slice
// 3.1 patterns); ctx-aware is required to exercise sibling-cancellation
// semantics (slice 3.2 parallel + the slice 3.1 try-finally-on-ctx-cancel
// regression test).
type scriptedDispatcher struct {
	t      *testing.T
	script map[string]scriptedResult // step id → result
}

type scriptedResult struct {
	outcome  Outcome
	err      error          // if non-nil, the dispatcher returns this error directly
	ctxAware bool           // if true, return (RetryableFailure, ctx.Err()) when ctx is cancelled
	outputs  map[string]any // if non-nil, propagated to DispatchResult.Outputs (gate tests need this)
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
	if res.ctxAware {
		if err := ctx.Err(); err != nil {
			closed := make(chan container.IOChunk)
			close(closed)
			return DispatchResult{Outcome: OutcomeRetryableFailure}, closed, err
		}
	}
	closedCh := make(chan container.IOChunk)
	close(closedCh)
	if res.err != nil {
		return DispatchResult{Outcome: res.outcome}, closedCh, res.err
	}
	return DispatchResult{Outcome: res.outcome, ExitCode: intPtr(0), Outputs: res.outputs}, closedCh, nil
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
	// try.do = [skip]; catch = [should-not-run]; finally = [must-run]
	// Spec §5.6: try is NOT a skip-target scope. SkipUnwind propagates THROUGH
	// the try (with Finally running on the way), so runTry returns
	// (OutcomeOK, &SkipUnwind{}) — not (OutcomeOK, nil).
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
	if oc != OutcomeOK {
		t.Errorf("Try{skip-in-do}: outcome got %q, want %q (skip is terminal-ok)", oc, OutcomeOK)
	}
	// Spec §5.6: try is NOT a skip-target scope. SkipUnwind must propagate
	// through the try (with Finally running on the way) so the next enclosing
	// scope (or Run) catches it.
	var su *SkipUnwind
	if !errors.As(err, &su) {
		t.Errorf("Try{skip-in-do}: err = %v, want errors.As(*SkipUnwind) true (skip propagates THROUGH try per spec §5.6)", err)
	}
	if _, done := rs.Completed["try[0].finally.must-run"]; !done {
		t.Errorf("Try{skip-in-do}: finally step not in Completed: %+v", rs.Completed)
	}
}

func TestRunTrySkipInDoPropagatesToNextEnclosingScope(t *testing.T) {
	// Spec §5.6: skip terminates the NEAREST enclosing loop/gate/parallel/run.
	// Try is a passthrough — skip runs try's Finally on the way out, then
	// continues propagating. Verify by wrapping the try in a workflow Graph
	// and confirming a sibling AFTER the try does NOT run (Run absorbs the
	// skip at workflow-root → run completes ok, sibling never reached).
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.Skip{Reason: "early exit"},
		},
		Finally: ir.NodeList{
			&ir.CodeStep{ID: "must-run", Run: "echo finally"},
		},
	}
	siblingAfter := &ir.CodeStep{ID: "must-not-run-after-try", Run: "echo after"}
	disp, logger, blobs := tryTestRig(t, map[string]scriptedResult{
		"must-run": {outcome: OutcomeOK},
		// must-not-run-after-try deliberately NOT scripted — if it runs, scriptedDispatcher.t.Fatalf.
	})
	wf := &ir.Workflow{
		ID:      "x",
		Version: 1,
		Graph:   ir.NodeList{try, siblingAfter},
	}
	def := &ir.LoadedDefinition{Workflow: wf}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := Run(context.Background(), def, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if err != nil || oc != OutcomeOK {
		t.Fatalf("Run with skip-in-try.do + sibling-after-try: (oc, err) = (%q, %v), want (ok, nil) — skip should propagate through try to workflow root, terminating siblings", oc, err)
	}
	if _, done := rs.Completed["try[0].finally.must-run"]; !done {
		t.Errorf("finally step not in Completed: %+v", rs.Completed)
	}
	if _, done := rs.Completed["must-not-run-after-try"]; done {
		t.Errorf("sibling after try ran; spec §5.6 says skip propagates past try to workflow root: %+v", rs.Completed)
	}
}

func TestRunTrySkipAppendFailsStillRunsFinally(t *testing.T) {
	// The pendingErr machinery (Task 4 fixup 42482b3) ensures Finally runs
	// even when appendNodeSkipped fails. Without it, a transient log-append
	// failure during a skip would silently skip Finally cleanup, breaking
	// the "ALWAYS run Finally" invariant from design §B step 3.
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.Skip{Reason: "skip do"},
		},
		Finally: ir.NodeList{
			&ir.CodeStep{ID: "must-run", Run: "echo finally"},
		},
	}
	disp, _, blobs := tryTestRig(t, map[string]scriptedResult{
		"must-run": {outcome: OutcomeOK},
	})
	clk := &clock.Fake{}
	logger := state.NewInMemoryLog(clk)
	// Fail the first Append: that's the node.skipped event runTry tries to
	// emit when SkipUnwind escapes Do. The pendingErr machinery must capture
	// this and STILL run Finally before returning.
	logger.FailAppendAfterN(0)
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{try}}
	rs := NewRunState("run-x", "digest", nil)
	_, err := runTry(context.Background(), try, "try[0]", wf, rs, disp, logger, blobs, clk, nil)
	if err == nil {
		t.Error("expected non-nil err from appendNodeSkipped failure, got nil")
	}
	// The critical invariant: Finally MUST have run even though appendNodeSkipped
	// failed (in fact, even though the runTry surface returns an internal error).
	if _, done := rs.Completed["try[0].finally.must-run"]; !done {
		t.Errorf("Finally MUST run even on appendNodeSkipped failure (\"ALWAYS run Finally\" invariant): Completed = %+v", rs.Completed)
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

func TestRunSkipAtRootEndsRunOK(t *testing.T) {
	// Workflow with a single root-scope Skip node:
	//   graph: [- skip: "early exit"]
	wf := &ir.Workflow{
		ID:      "x",
		Version: 1,
		Graph: ir.NodeList{
			&ir.Skip{Reason: "early exit"},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}
	rs := NewRunState("run-x", "digest", nil)
	// Skip doesn't invoke the dispatcher — pass a nil-safe sentinel.
	disp := &scriptedDispatcher{t: t, script: map[string]scriptedResult{}}
	logger := state.NewInMemoryLog(&clock.Fake{})
	blobs := state.NewInMemoryBlobs()

	oc, err := Run(context.Background(), def, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if err != nil {
		t.Errorf("Run with root Skip: err = %v, want nil", err)
	}
	if oc != OutcomeOK {
		t.Errorf("Run with root Skip: outcome = %q, want %q", oc, OutcomeOK)
	}
	// Verify the node.skipped event was appended.
	events, _ := logger.Fold()
	var found bool
	for _, e := range events {
		if e.Type == EventNodeSkipped {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Run with root Skip: no node.skipped event in log: %+v", events)
	}
}

func TestSkipInsideLoopEndsIterationLoopContinues(t *testing.T) {
	// loop { max_iters: 3, body: [skip] } — each iter ends via skip, loop runs
	// all 3 iters, 3 loop.iter events recorded (one per skipped iter).
	maxIters := 3
	wf := &ir.Workflow{
		ID:      "x",
		Version: 1,
		Graph: ir.NodeList{
			&ir.Loop{
				MaxIters: &maxIters,
				Body: ir.NodeList{
					&ir.Skip{Reason: "skip iter"},
				},
			},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}
	rs := NewRunState("run-x", "digest", nil)
	disp := &scriptedDispatcher{t: t, script: map[string]scriptedResult{}}
	logger := state.NewInMemoryLog(&clock.Fake{})
	blobs := state.NewInMemoryBlobs()

	oc, err := Run(context.Background(), def, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if err != nil {
		t.Errorf("Run with skip-in-loop: err = %v, want nil", err)
	}
	if oc != OutcomeOK {
		t.Errorf("Run with skip-in-loop: outcome = %q, want %q", oc, OutcomeOK)
	}
	// Verify 3 loop.iter events (each skipped iter still records loop.iter).
	events, _ := logger.Fold()
	var loopIters, nodeSkippeds int
	for _, e := range events {
		if e.Type == EventLoopIter {
			loopIters++
		}
		if e.Type == EventNodeSkipped {
			nodeSkippeds++
		}
	}
	if loopIters != 3 {
		t.Errorf("Run with skip-in-loop: loop.iter count = %d, want 3 (one per skipped iter)", loopIters)
	}
	if nodeSkippeds != 3 {
		t.Errorf("Run with skip-in-loop: node.skipped count = %d, want 3 (one per skipped iter)", nodeSkippeds)
	}
	if rs.LoopIters["loop[0]"] != 3 {
		t.Errorf("RunState.LoopIters[loop[0]] = %d, want 3", rs.LoopIters["loop[0]"])
	}
}

func TestRunTryFinallyRunsEvenWhenCtxCancelledMidDo(t *testing.T) {
	// Phase 3 design §B step 3: "ALWAYS run Finally (even on ctx-cancel)."
	// Pre-fix: runTry passed the cancelled ctx straight through to Finally;
	// a ctx-aware dispatcher (like container.Fake) would short-circuit each
	// finally step via its pre-Run ctx.Err() check, so finally never
	// committed. Post-fix: runTry passes context.WithoutCancel(ctx) to
	// Finally, so dispatch proceeds normally; the step 5 ctx-check at the
	// bottom of runTry still propagates the cancellation to the caller.
	//
	// Slice 3.2's Bucket 4b parallel_cancellation conformance test is the
	// primary regression guard (it uses the real container.Fake and was
	// flaky pre-fix). This engine-level test pins the contract so a future
	// refactor of engine/try.go can't silently revert it without a red bar
	// here.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel; simulates sibling-failure cancelling gctx mid-do
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.CodeStep{ID: "do-step", Run: "echo"},
		},
		Finally: ir.NodeList{
			&ir.CodeStep{ID: "must-run-finally", Run: "echo"},
		},
	}
	// ctxAware: true so the dispatcher mirrors container.Fake's pre-Run
	// ctx.Err() short-circuit. Pre-fix, runTry would pass the cancelled
	// ctx to Finally → must-run-finally's dispatch short-circuits, never
	// commits. Post-fix, runTry passes WithoutCancel(ctx) → ctx.Err() in
	// the dispatcher returns nil → step runs and commits.
	disp, logger, blobs := tryTestRig(t, map[string]scriptedResult{
		"do-step":          {outcome: OutcomeOK, ctxAware: true},
		"must-run-finally": {outcome: OutcomeOK, ctxAware: true},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{try}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runTry(ctx, try, "try[0]", wf, rs, disp, logger, blobs, &clock.Fake{}, nil)
	// Step 5 ctx-check: cancellation must still propagate to caller.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(context.Canceled) true (step 5 ctx-check must still propagate cancellation)", err)
	}
	if oc != OutcomeRetryableFailure {
		t.Errorf("outcome = %q, want %q (cancelled run)", oc, OutcomeRetryableFailure)
	}
	// CRITICAL: finally MUST have committed despite ctx-cancel. Pre-fix
	// this was the failing assertion (ctx-aware dispatch in Finally
	// short-circuited before reaching the commit path).
	if _, done := rs.LookupCompleted("try[0].finally.must-run-finally"); !done {
		t.Errorf("Finally must run even on ctx-cancel (Phase 3 design §B step 3): must-run-finally not in Completed = %+v", rs.Completed)
	}
}

func TestRunTryCtxCancelledSupersedeDoError(t *testing.T) {
	// ctx cancelled mid-do AND Do also errored → ctx.Err() must win.
	// Needed for slice 3.2's parallel handler that distinguishes
	// sibling-cancelled branches via errors.Is(err, context.Canceled).
	ctx, cancel := context.WithCancel(context.Background())
	try := &ir.Try{
		Do: ir.NodeList{
			&ir.CodeStep{ID: "do-fails", Run: "exit 1"},
		},
		Finally: ir.NodeList{
			&ir.CodeStep{ID: "finally-ok", Run: "echo finally"},
		},
	}
	disp, logger, blobs := tryTestRig(t, map[string]scriptedResult{
		"do-fails":   {outcome: OutcomeRetryableFailure, err: errors.New("do failed independently")},
		"finally-ok": {outcome: OutcomeOK},
	})
	cancel() // cancel BEFORE running
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{try}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runTry(ctx, try, "try[0]", wf, rs, disp, logger, blobs, &clock.Fake{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Try{ctx-cancelled-AND-do-fails}: err = %v, want errors.Is(context.Canceled) true (cancellation supersedes Do's error so slice 3.2's parallel handler can detect sibling cancellation)", err)
	}
	if oc != OutcomeRetryableFailure {
		t.Errorf("outcome got %q, want %q", oc, OutcomeRetryableFailure)
	}
	if _, done := rs.Completed["try[0].finally.finally-ok"]; !done {
		t.Errorf("finally must run: Completed = %+v", rs.Completed)
	}
}
