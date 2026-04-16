package conformance

import (
	"sort"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// testPropagation runs Bucket 4 sub-tests per Phase 3 design §H:
//   - caught:  try.catch absorbs a retry-exhausted failure; finally runs;
//     downstream step runs; run completes ok.
//   - uncaught: same workflow without try → run halts with retryable_failure
//     propagated.
//   - parallel_cancellation: try.catch wraps a 3-branch parallel; one
//     branch fails (retry-exhausted); siblings' finally blocks run; outer
//     catch absorbs; run completes ok.
//   - parallel_resume_consistency: mid-parallel crash via deterministic
//     ProgramExec failure; resume folds the log, skips committed branches,
//     re-runs only the uncommitted branch + after-step. Asserts per-path
//     node.completed count == 1 across both runs.
func testPropagation(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("caught", func(t *testing.T) { testPropagationCaught(t, factory) })
	t.Run("uncaught", func(t *testing.T) { testPropagationUncaught(t, factory) })
	t.Run("parallel_cancellation", func(t *testing.T) { testPropagationParallelCancellation(t, factory) })
	t.Run("parallel_resume_consistency", func(t *testing.T) { testPropagationParallelResume(t, factory) })
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

func testPropagationParallelCancellation(t *testing.T, factory BackendFactory) {
	t.Helper()
	programmedFactory := preProgramFake(t, factory, []execProgram{
		{cmd: "./b0-failing.sh", res: container.ExecResult{ExitCode: 1}},
		// b1-do / b2-do: program both as ok. Under fake semantics, all branches
		// run their full programmed result before fan-in. What matters is the
		// finally MARKERS still run regardless of whether the branches' do
		// completed before / after b0 failed.
		{cmd: "./b1-do.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./b1-finally.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./b2-do.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./b2-finally.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./outer-catch.sh", res: container.ExecResult{ExitCode: 0}},
	})
	h := newHarness(t, programmedFactory, parallelCancellationWorkflow)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Bucket 4b parallel_cancellation: outcome = %q, want ok (try.catch absorbs branch 0's propagation)", oc)
	}
	events := mustFoldEvents(t, h)
	postRS, err := engine.Fold(events, h.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	// Diagnostic: log the Completed paths so a path-scheme mismatch
	// surfaces immediately on `-v` runs (assertions below were verified
	// by inspection against ir.PathFor; this Logf catches an accidental
	// rename in a Phase 4+ slice).
	t.Logf("Completed paths after parallel_cancellation: %v", keysOf(postRS.Completed))

	if _, done := postRS.Completed["try[0].do.parallel[0].b0-failing"]; done {
		t.Errorf("Bucket 4b: b0-failing in Completed; only ok-steps commit (spec §8)")
	}
	for _, path := range []string{
		"try[0].do.parallel[0].try[1].finally.b1-finally",
		"try[0].do.parallel[0].try[2].finally.b2-finally",
		"try[0].catch.outer-catch",
	} {
		if _, done := postRS.Completed[path]; !done {
			t.Errorf("Bucket 4b: %q not in Completed (sibling finally + outer catch must run): %v",
				path, keysOf(postRS.Completed))
		}
	}
}

// keysOf returns the sorted keys of m for stable test output.
func keysOf(m map[string]engine.NodeResult) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func testPropagationParallelResume(t *testing.T, factory BackendFactory) {
	t.Helper()
	// First run: pb2.sh is programmed to return exit code 1 deterministically
	// (NOT FailExecAfterN, which would fire on an arbitrary branch under
	// parallel's non-deterministic scheduling). pb0 + pb1 succeed; pb2
	// halts with retryable_failure (retry: attempts=1). pb0 + pb1 commit;
	// pb2 does not; after never runs.
	faultyFactory := func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		fake.ProgramExec("./pb0.sh", container.ExecResult{ExitCode: 0}, nil)
		fake.ProgramExec("./pb1.sh", container.ExecResult{ExitCode: 0}, nil)
		fake.ProgramExec("./pb2.sh", container.ExecResult{ExitCode: 1}, nil) // deterministic failing branch
		fake.ProgramExec("./after.sh", container.ExecResult{ExitCode: 0}, nil)
		return fake
	}
	h := newHarness(t, faultyFactory, parallelResumeWorkflow)
	oc, err := h.runWorkflow(t)
	if oc == engine.OutcomeOK {
		t.Fatalf("first run: outcome = ok, want failure (pb2 should have halted)")
	}
	_ = err

	preEvents := mustFoldEvents(t, h)
	preRS, err := engine.Fold(preEvents, h.blobs)
	if err != nil {
		t.Fatalf("Fold pre-resume: %v", err)
	}
	t.Logf("Pre-resume Completed: %v", keysOf(preRS.Completed))

	// Deterministic pre-conditions: pb0 + pb1 in Completed; pb2 + after not.
	for _, id := range []string{"parallel[0].pb0", "parallel[0].pb1"} {
		if _, done := preRS.Completed[id]; !done {
			t.Errorf("pre-resume: %q should be in Completed (succeeded before pb2 halted): %v",
				id, keysOf(preRS.Completed))
		}
	}
	for _, id := range []string{"parallel[0].pb2", "after"} {
		if _, done := preRS.Completed[id]; done {
			t.Errorf("pre-resume: %q should NOT be in Completed (halt should have prevented commit): %v",
				id, keysOf(preRS.Completed))
		}
	}

	// Resume: re-program pb2 to succeed, re-run. Resume should skip pb0+pb1
	// (already in Completed via Fold-replay) and re-execute only pb2 + after.
	h.factory = func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		// Resume only re-runs pb2 (uncommitted) and after. pb0+pb1 are
		// replayed from the log without dispatch. ProgramExec entries for
		// pb0/pb1 are unnecessary but cheap insurance against an
		// incorrectly-routed re-dispatch (the next assertion catches that).
		fake.ProgramExec("./pb0.sh", container.ExecResult{ExitCode: 0}, nil)
		fake.ProgramExec("./pb1.sh", container.ExecResult{ExitCode: 0}, nil)
		fake.ProgramExec("./pb2.sh", container.ExecResult{ExitCode: 0}, nil) // now succeeds
		fake.ProgramExec("./after.sh", container.ExecResult{ExitCode: 0}, nil)
		return fake
	}
	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Fatalf("resume: %v", err2)
	}
	if oc2 != engine.OutcomeOK {
		t.Errorf("resume outcome = %q, want ok", oc2)
	}

	postEvents := mustFoldEvents(t, h)
	postRS, err := engine.Fold(postEvents, h.blobs)
	if err != nil {
		t.Fatalf("Fold post-resume: %v", err)
	}
	t.Logf("Post-resume Completed: %v", keysOf(postRS.Completed))

	// All four nodes must be in Completed after resume.
	for _, id := range []string{"parallel[0].pb0", "parallel[0].pb1", "parallel[0].pb2", "after"} {
		if _, done := postRS.Completed[id]; !done {
			t.Errorf("post-resume: %q not in Completed: %v", id, keysOf(postRS.Completed))
		}
	}

	// Per-path commit-once invariant: each path has EXACTLY ONE
	// node.completed event across both runs. Resume must REPLAY committed
	// steps (per spec §8) — not re-execute and re-commit them.
	commits := map[string]int{}
	for _, e := range postEvents {
		if e.Type == engine.EventNodeCompleted {
			commits[e.Path]++
		}
	}
	for path, count := range commits {
		if count != 1 {
			t.Errorf("path %q has %d node.completed events; resume must replay (not re-execute) committed steps (spec §8)", path, count)
		}
	}
}
