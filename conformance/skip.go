package conformance

import (
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// testSkip runs Bucket 6 sub-tests per Phase 3 design §H.
func testSkip(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("at_root", func(t *testing.T) { testSkipAtRoot(t, factory) })
	t.Run("in_loop_body", func(t *testing.T) { testSkipInLoopBody(t, factory) })
	t.Run("in_try_do", func(t *testing.T) { testSkipInTryDo(t, factory) })
}

func testSkipAtRoot(t *testing.T, factory BackendFactory) {
	t.Helper()
	// No programs needed — Skip doesn't invoke the backend. But the harness
	// still creates the container handles, so the factory needs to return a
	// usable backend; pass the raw factory unchanged.
	h := newHarness(t, factory, skipAtRootWorkflow)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("Bucket 6 at_root: engine.Run err = %v, want nil", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Bucket 6 at_root: outcome = %q, want ok", oc)
	}
	events := mustFoldEvents(t, h)
	var foundSkipped bool
	for _, e := range events {
		if e.Type == engine.EventNodeSkipped {
			foundSkipped = true
			break
		}
	}
	if !foundSkipped {
		t.Errorf("Bucket 6 at_root: no node.skipped event in log: %+v", events)
	}
}

func testSkipInLoopBody(t *testing.T, factory BackendFactory) {
	t.Helper()
	h := newHarness(t, factory, skipInLoopBodyWorkflow)
	oc, err := h.runWorkflow(t)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Bucket 6 in_loop_body: (oc, err) = (%q, %v), want (ok, nil)", oc, err)
	}
	events := mustFoldEvents(t, h)
	var iters, skippeds int
	for _, e := range events {
		switch e.Type {
		case engine.EventLoopIter:
			iters++
		case engine.EventNodeSkipped:
			skippeds++
		}
	}
	if iters != 3 {
		t.Errorf("Bucket 6 in_loop_body: loop.iter count = %d, want 3 (each skipped iter records loop.iter — slice 3.1 design question 2)", iters)
	}
	if skippeds != 3 {
		t.Errorf("Bucket 6 in_loop_body: node.skipped count = %d, want 3 (one per skipped iter)", skippeds)
	}
}

func testSkipInTryDo(t *testing.T, factory BackendFactory) {
	t.Helper()
	programmedFactory := preProgramFake(t, factory, []execProgram{
		// must-not-run.sh is DELIBERATELY not programmed — if Catch runs,
		// the fake's ProgramExec-miss returns an error and the test catches it.
		{cmd: "./must-run.sh", res: container.ExecResult{ExitCode: 0}},
	})
	h := newHarness(t, programmedFactory, skipInTryDoWorkflow)
	oc, err := h.runWorkflow(t)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Bucket 6 in_try_do: (oc, err) = (%q, %v), want (ok, nil) — Skip in do should bypass Catch and run Finally", oc, err)
	}
	postEvents := mustFoldEvents(t, h)
	postRS, ferr := engine.Fold(postEvents, h.blobs)
	if ferr != nil {
		t.Fatalf("Fold post-run: %v", ferr)
	}
	if _, mustRun := postRS.Completed["try[0].finally.must-run"]; !mustRun {
		t.Errorf("Bucket 6 in_try_do: finally step must-run not in Completed: %+v", postRS.Completed)
	}
	if _, mustNotRun := postRS.Completed["try[0].catch.must-not-run"]; mustNotRun {
		t.Errorf("Bucket 6 in_try_do: catch step must-not-run IS in Completed — catch should have been skipped (Skip in do bypasses Catch): %+v", postRS.Completed)
	}
}
