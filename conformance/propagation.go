package conformance

import (
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// testPropagation runs Bucket 4 sub-tests per Phase 3 design §H:
//   - caught:  try.catch absorbs a retry-exhausted failure; finally runs;
//     downstream step runs; run completes ok.
//   - uncaught: same workflow without try → run halts with retryable_failure
//     propagated.
//
// Slice 3.2 will add a third sub-test (parallel sibling cancellation) under
// the same bucket.
func testPropagation(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("caught", func(t *testing.T) { testPropagationCaught(t, factory) })
	t.Run("uncaught", func(t *testing.T) { testPropagationUncaught(t, factory) })
}

func testPropagationCaught(t *testing.T, factory BackendFactory) {
	t.Helper()
	programmedFactory := preProgramFake(t, factory, []execProgram{
		{cmd: "./step1.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./step2-failing.sh", res: container.ExecResult{ExitCode: 1}}, // halts via retry: {attempts: 1}
		{cmd: "./catch.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./finally.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./step3.sh", res: container.ExecResult{ExitCode: 0}},
	})
	h := newHarness(t, programmedFactory, propagationCaughtWorkflow)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Bucket 4a caught: outcome = %q, want ok (try.catch should have absorbed step2's failure)", oc)
	}
	// Verify the post-run RunState via a fold of the log.
	postEvents := mustFoldEvents(t, h)
	postRS, err := engine.Fold(postEvents, h.blobs)
	if err != nil {
		t.Fatalf("Fold post-run: %v", err)
	}
	// step2-failing must NOT be in Completed — it never committed (spec §8:
	// only ok-steps commit).
	if _, done := postRS.Completed["try[1].do.step2-failing"]; done {
		t.Errorf("Bucket 4a caught: step2-failing in Completed; should not be — only ok-steps commit (spec §8)")
	}
	// catch-step, finally-step, step3 must all be in Completed.
	for _, path := range []string{"try[1].catch.catch-step", "try[1].finally.finally-step", "step3"} {
		if _, done := postRS.Completed[path]; !done {
			t.Errorf("Bucket 4a caught: %q not in Completed (catch/finally/step3 should all have run): %+v", path, postRS.Completed)
		}
	}
}

func testPropagationUncaught(t *testing.T, factory BackendFactory) {
	t.Helper()
	programmedFactory := preProgramFake(t, factory, []execProgram{
		{cmd: "./step1.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./step2-failing.sh", res: container.ExecResult{ExitCode: 1}},
		// step3.sh deliberately NOT programmed — if the interpreter reaches
		// it after the propagation, the fake's ProgramExec-miss error will
		// fire and the test catches it.
	})
	h := newHarness(t, programmedFactory, propagationUncaughtWorkflow)
	oc, err := h.runWorkflow(t)
	if oc != engine.OutcomeRetryableFailure {
		t.Errorf("Bucket 4a uncaught: outcome = %q, want %q", oc, engine.OutcomeRetryableFailure)
	}
	if err == nil {
		t.Errorf("Bucket 4a uncaught: err = nil, want non-nil propagation")
	}
	// step3 must NOT be in Completed (run halted before reaching it).
	postEvents := mustFoldEvents(t, h)
	postRS, ferr := engine.Fold(postEvents, h.blobs)
	if ferr != nil {
		t.Fatalf("Fold post-run: %v", ferr)
	}
	if _, done := postRS.Completed["step3"]; done {
		t.Errorf("Bucket 4a uncaught: step3 in Completed; run should have halted before reaching it: %+v", postRS.Completed)
	}
}
