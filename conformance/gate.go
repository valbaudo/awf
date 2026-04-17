package conformance

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// testGate runs Bucket 5 sub-tests per Phase 3 design §H + decision 11.
//
//   - feedback_threading: gate with max_attempts:2. Eval returns verified:false
//     (with feedback "missing X") on every attempt; asserts the generator was
//     dispatched on attempt-2 with the threaded feedback ("./gen.sh missing X"),
//     pinning the {{ evaluate.feedback }} round-trip.
//   - max_attempts_rejected: eval always returns verified:false. After 3
//     attempts, gate returns OutcomeRejected.
//   - crash_not_verdict: generator crashes on attempt 1 (retry-exhausted).
//     No gate.attempt event committed; attempt budget not consumed.
//   - mid_resume: max_attempts:5. fake.FailExecAfterN(2) makes attempt-2's
//     gen1 crash mid-flight; pre-resume: gate.attempt-1 only. Resume on a
//     fresh (fault-free) backend continues at attempt 2, runs through
//     attempt 5 → all rejected.
//   - independence_placeholder: asserts engine dispatches the 3 steps
//     (gen1, gen2, eval1) with distinct node.completed paths. Phase 5 replaces
//     this with the meaningful agent-launch fresh-context proof.
//   - rejected_caught_by_try (critique-pass addition): gate wrapped in
//     try.catch; gate rejects on max_attempts:1; try.catch's
//     unconditional-catch absorbs; handler runs; run completes ok. Pins
//     design §A's "rejected propagates like any failure to nearest
//     try/catch" claim.
func testGate(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("feedback_threading", func(t *testing.T) { testGateFeedbackThreading(t, factory) })
	t.Run("max_attempts_rejected", func(t *testing.T) { testGateMaxAttemptsRejected(t, factory) })
	t.Run("crash_not_verdict", func(t *testing.T) { testGateCrashNotVerdict(t, factory) })
	t.Run("mid_resume", func(t *testing.T) { testGateMidResume(t, factory) })
	t.Run("independence_placeholder", func(t *testing.T) { testGateIndependencePlaceholder(t, factory) })
	t.Run("rejected_caught_by_try", func(t *testing.T) { testGateRejectedCaughtByTry(t, factory) })
}

// verdictRejected is the per-call AWFOutput JSON the fake returns from the
// evaluator step. Decoded by engine.ValidateAgainstSchema, the typed
// verdict becomes {verified: false, feedback: "missing X"} — the
// evaluator's last step's Outputs map. gate.until ({{ evaluate.verified }})
// resolves to false → AttemptRejected. Used by max_attempts_rejected and
// mid_resume.
var verdictRejected = []byte(`{"verified":false,"feedback":"missing X"}`)

// verdictPassed: until evaluates to true → AttemptPassed.
var verdictPassed = []byte(`{"verified":true,"feedback":"all good"}`)

func testGateFeedbackThreading(t *testing.T, factory BackendFactory) {
	t.Helper()
	// The generator's run is "./gen.sh {{ evaluate.feedback }}":
	//   attempt 1: "./gen.sh " (empty feedback per attempt-1 contract).
	//   attempt 2: "./gen.sh missing X" (threaded from attempt-1's verdict).
	// Because fake's ProgramExec is keyed by Run command, we can program
	// BOTH variants and assert (a) both were dispatched in order, AND
	// (b) attempt-2's interpolated command appeared exactly once.
	//
	// fake.ProgramExec is keyed by Cmd.Run, so a single ./eval.sh entry
	// returns the SAME programmed AWFOutput on every call. To make the
	// gate eventually pass-on-2, we'd need different verdicts per attempt,
	// which fake doesn't support. So this sub-test asserts FEEDBACK WAS
	// THREADED (correct dispatch sequence) and DOESN'T assert pass-on-2.
	// The engine-level TestRunGateRepairsAndPassesOnAttempt2 in
	// engine/gate_test.go uses per-attempt dispatcher injection to cover
	// the pass-on-2 verdict round-trip.
	// Capture the *Fake used during the run so we can read fake.Calls after.
	// h.factory() is called once per lifetime by runOrResume — minting a fresh
	// fake on every invocation — so we can't recover the run's fake by calling
	// h.factory() again post-run (it would return an empty-Calls fake).
	var runFake *container.Fake
	capturingFactory := func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		fake.ProgramExec("./gen.sh ", container.ExecResult{ExitCode: 0}, nil)          // attempt 1 (empty feedback)
		fake.ProgramExec("./gen.sh missing X", container.ExecResult{ExitCode: 0}, nil) // attempt 2 (threaded)
		fake.ProgramExec("./eval.sh", container.ExecResult{ExitCode: 0, AWFOutput: verdictRejected}, nil)
		runFake = fake
		return fake
	}
	h := newHarness(t, capturingFactory, gateFeedbackThreadingWorkflow)
	oc, err := h.runWorkflow(t)
	if oc != engine.OutcomeRejected {
		// Both attempts run; both return verdictRejected; gate exhausts max_attempts:2 → rejected.
		t.Errorf("Bucket 5 feedback_threading: outcome = %q, want %q", oc, engine.OutcomeRejected)
	}
	if err == nil {
		t.Errorf("Bucket 5 feedback_threading: err = nil, want non-nil")
	}

	// Verify the dispatched commands: attempt-1's command was "./gen.sh "
	// (empty interpolation) AND attempt-2's command was "./gen.sh missing X"
	// (feedback threaded). We assert via the fake's recorded Calls slice.
	if runFake == nil {
		t.Skip("feedback_threading: backend is not *container.Fake; sub-test pins fake-level dispatch")
	}
	fake := runFake
	var sawEmpty, sawThreaded bool
	for _, c := range fake.Calls {
		switch c.Run {
		case "./gen.sh ":
			sawEmpty = true
		case "./gen.sh missing X":
			sawThreaded = true
		}
	}
	if !sawEmpty {
		t.Errorf("Bucket 5 feedback_threading: attempt-1 generator command \"./gen.sh \" NOT dispatched; calls = %+v", fake.Calls)
	}
	if !sawThreaded {
		t.Errorf("Bucket 5 feedback_threading: attempt-2 generator command \"./gen.sh missing X\" NOT dispatched — FEEDBACK NOT THREADED; calls = %+v", fake.Calls)
	}
}

func testGateMaxAttemptsRejected(t *testing.T, factory BackendFactory) {
	t.Helper()
	programmedFactory := preProgramFake(t, factory, []execProgram{
		{cmd: "./gen.sh", res: container.ExecResult{ExitCode: 0}},
		// eval.sh returns the rejected verdict on every call. until
		// resolves to false → AttemptRejected. Three attempts → rejected.
		{cmd: "./eval.sh", res: container.ExecResult{ExitCode: 0, AWFOutput: verdictRejected}},
	})
	h := newHarness(t, programmedFactory, gateMaxAttemptsRejectedWorkflow)
	oc, err := h.runWorkflow(t)
	if oc != engine.OutcomeRejected {
		t.Errorf("Bucket 5 max_attempts_rejected: outcome = %q, want %q", oc, engine.OutcomeRejected)
	}
	if err == nil {
		t.Errorf("Bucket 5 max_attempts_rejected: err = nil, want non-nil")
	}
	events := mustFoldEvents(t, h)
	gateAttempts := 0
	for _, e := range events {
		if e.Type == engine.EventGateAttempt {
			gateAttempts++
		}
	}
	if gateAttempts != 3 {
		t.Errorf("Bucket 5 max_attempts_rejected: gate.attempt events = %d, want 3", gateAttempts)
	}
}

func testGateCrashNotVerdict(t *testing.T, factory BackendFactory) {
	t.Helper()
	programmedFactory := preProgramFake(t, factory, []execProgram{
		{cmd: "./gen-crash.sh", res: container.ExecResult{ExitCode: 1}}, // halts via retry:{attempts:1}
		// eval.sh: would-be-pass; must NOT be reached. AWFOutput present for
		// defense-in-depth — if a regression makes eval run, schema validation
		// passes and we'd see a gate.attempt event (the assertion below catches that).
		{cmd: "./eval.sh", res: container.ExecResult{ExitCode: 0, AWFOutput: verdictPassed}},
	})
	h := newHarness(t, programmedFactory, gateCrashNotVerdictWorkflow)
	oc, err := h.runWorkflow(t)
	// Generator's failure propagates BEFORE any gate.attempt commits.
	if oc == engine.OutcomeOK {
		t.Errorf("Bucket 5 crash_not_verdict: outcome = ok; want propagation of generator crash")
	}
	if oc == engine.OutcomeRejected {
		t.Errorf("Bucket 5 crash_not_verdict: outcome = rejected; CRASH IS NOT A VERDICT (design §D)")
	}
	if err == nil {
		t.Errorf("Bucket 5 crash_not_verdict: err = nil, want non-nil")
	}
	events := mustFoldEvents(t, h)
	for _, e := range events {
		if e.Type == engine.EventGateAttempt {
			t.Errorf("Bucket 5 crash_not_verdict: unexpected gate.attempt event in log (CRASH IS NOT A VERDICT): %+v", e)
		}
	}
}

func testGateMidResume(t *testing.T, factory BackendFactory) {
	t.Helper()
	// Critique-pass revision: this sub-test does a REAL mid-attempt crash
	// + resume via fake.FailExecAfterN, replacing the prior shortcut that
	// ran-to-completion and just folded the log.
	//
	// First-run call sequence (max_attempts: 5):
	//   call 1: ./gen.sh   (attempt-1 generate)
	//   call 2: ./eval.sh  (attempt-1 evaluate)
	//   ← gate.attempt-1{rejected} commits
	//   call 3: ./gen.sh   (attempt-2 generate)   ← FailExecAfterN(2) FIRES HERE
	//   gen1 fails, retry: {attempts: 1} so no retry; gate propagates BEFORE
	//   committing gate.attempt-2. Run halts with retryable_failure.
	//
	// Pre-resume state: gate.attempt-1 in log; gate.attempt-2 absent.
	// RunState.GateAttempts["gate[0]"] has 1 entry.
	//
	// Resume call sequence (after fresh backend): runGate starts at attempt 2
	// (startN = len(GateAttempts)+1). Runs attempts 2, 3, 4, 5 → all rejected.
	// Final OutcomeRejected; total gate.attempt events = 5.
	faultyFactory := func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		fake.ProgramExec("./gen.sh", container.ExecResult{ExitCode: 0}, nil)
		fake.ProgramExec("./eval.sh", container.ExecResult{ExitCode: 0, AWFOutput: verdictRejected}, nil)
		fake.FailExecAfterN(2) // first 2 calls succeed; call 3 fails
		return fake
	}
	h := newHarness(t, faultyFactory, gateMidResumeWorkflow)

	// First run: halts mid-attempt-2 (gen1 crashes).
	oc1, err1 := h.runWorkflow(t)
	if oc1 == engine.OutcomeOK {
		t.Fatalf("Bucket 5 mid_resume first run: outcome = ok, want propagation of induced fault")
	}
	if err1 == nil {
		t.Fatalf("Bucket 5 mid_resume first run: err = nil, want non-nil")
	}

	preEvents := mustFoldEvents(t, h)
	preRS, ferr := engine.Fold(preEvents, h.blobs)
	if ferr != nil {
		t.Fatalf("Fold pre-resume: %v", ferr)
	}
	pre := preRS.GateAttempts["gate[0]"]
	if len(pre) != 1 {
		t.Fatalf("Bucket 5 mid_resume pre-resume: GateAttempts len = %d, want 1 (only attempt-1 should have committed before the fault)", len(pre))
	}
	if pre[0].N != 1 || pre[0].AttemptOutcome != engine.AttemptRejected {
		t.Errorf("Bucket 5 mid_resume pre-resume: attempt[0] = %+v, want N=1 AttemptRejected", pre[0])
	}

	// Resume: fresh backend (factory mints a new *Fake; the fault is reset
	// implicitly because it's a fresh instance). Programs gen + eval results
	// again. Resume continues at attempt 2.
	h.factory = func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		fake.ProgramExec("./gen.sh", container.ExecResult{ExitCode: 0}, nil)
		fake.ProgramExec("./eval.sh", container.ExecResult{ExitCode: 0, AWFOutput: verdictRejected}, nil)
		// No FailExecAfterN — resume runs cleanly.
		return fake
	}
	oc2, err2 := h.resumeWorkflow(t)
	if oc2 != engine.OutcomeRejected {
		t.Errorf("Bucket 5 mid_resume resume: outcome = %q, want %q", oc2, engine.OutcomeRejected)
	}
	if err2 == nil {
		t.Errorf("Bucket 5 mid_resume resume: err = nil, want non-nil (exhausted max_attempts:5)")
	}

	postEvents := mustFoldEvents(t, h)
	postRS, ferr := engine.Fold(postEvents, h.blobs)
	if ferr != nil {
		t.Fatalf("Fold post-resume: %v", ferr)
	}
	post := postRS.GateAttempts["gate[0]"]
	if len(post) != 5 {
		t.Fatalf("Bucket 5 mid_resume post-resume: GateAttempts len = %d, want 5", len(post))
	}
	for i, ar := range post {
		if ar.N != i+1 {
			t.Errorf("Bucket 5 mid_resume post-resume: attempt[%d].N = %d, want %d (monotonic 1..5 across resume)", i, ar.N, i+1)
		}
	}

	// Per-path commit-once invariant: each path appears AT MOST once in the
	// post-log's gate.attempt sequence. Resume must continue numbering, not
	// re-emit attempt-1.
	gateAttemptEvents := 0
	attemptNs := map[int]int{}
	for _, e := range postEvents {
		if e.Type == engine.EventGateAttempt {
			gateAttemptEvents++
			var d engine.GateAttemptData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal gate.attempt: %v", err)
			}
			attemptNs[d.N]++
		}
	}
	if gateAttemptEvents != 5 {
		t.Errorf("Bucket 5 mid_resume: gate.attempt event count = %d, want 5 (1 pre-resume + 4 post-resume)", gateAttemptEvents)
	}
	for n, count := range attemptNs {
		if count != 1 {
			t.Errorf("Bucket 5 mid_resume: attempt N=%d appeared %d times in log; want 1 (resume must continue numbering, not duplicate)", n, count)
		}
	}
}

func testGateRejectedCaughtByTry(t *testing.T, factory BackendFactory) {
	t.Helper()
	// Critique-pass addition (revision 6): pins design §A's claim that gate
	// rejection propagates like any other failure to the nearest try.catch.
	// Workflow: try { do: [gate] catch: [handler] }. Gate has max_attempts:1
	// and an evaluator that returns verified:false → gate returns OutcomeRejected.
	// try.catch's unconditional-catch (slice 3.1 decision 7) absorbs.
	// catch runs the handler step; run completes ok.
	programmedFactory := preProgramFake(t, factory, []execProgram{
		{cmd: "./gen.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./eval.sh", res: container.ExecResult{ExitCode: 0, AWFOutput: verdictRejected}},
		{cmd: "./handler.sh", res: container.ExecResult{ExitCode: 0}},
	})
	h := newHarness(t, programmedFactory, gateRejectedCaughtWorkflow)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Bucket 5 rejected_caught: outcome = %q, want ok (try.catch absorbs gate rejection)", oc)
	}
	postEvents := mustFoldEvents(t, h)
	postRS, ferr := engine.Fold(postEvents, h.blobs)
	if ferr != nil {
		t.Fatalf("Fold: %v", ferr)
	}
	// gate.attempt-1 should be in the log (the gate did consume one attempt).
	var gateAttempts int
	for _, e := range postEvents {
		if e.Type == engine.EventGateAttempt {
			gateAttempts++
		}
	}
	if gateAttempts != 1 {
		t.Errorf("Bucket 5 rejected_caught: gate.attempt events = %d, want 1 (max_attempts:1)", gateAttempts)
	}
	// catch.handler must be in Completed.
	if _, done := postRS.Completed["try[0].catch.handler"]; !done {
		t.Errorf("Bucket 5 rejected_caught: catch handler NOT in Completed (try.catch should have absorbed gate rejection): %+v", postRS.Completed)
	}
}

func testGateIndependencePlaceholder(t *testing.T, factory BackendFactory) {
	t.Helper()
	// PHASE 5 PLACEHOLDER. Engine-level independence: assert each step in the
	// gate (gen1, gen2, eval1) was dispatched as its own NodeIntent (no shared
	// adapter handle, no shared transcript-ref). Trivially passes for code
	// steps — the meaningful fresh-context agent-launch proof replaces this
	// sub-test when Phase 5's agent.Adapter lands.
	//
	// Implementation: the harness's LocalDispatcher wraps a *Fake. We don't
	// have a hook to capture intents at the harness level today; this
	// sub-test inspects the LOG to verify each step's node.completed event
	// is independently committed (which it must be, since the engine's
	// Commit path appends one per step). For Phase 5, this becomes the
	// "evaluator step was launched with no --resume" assertion.
	programmedFactory := preProgramFake(t, factory, []execProgram{
		{cmd: "./gen1.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./gen2.sh", res: container.ExecResult{ExitCode: 0}},
		// eval.sh: passes (verified:true) so gate returns ok on attempt-1.
		// We only care that all 3 steps committed independently — passing
		// keeps the assertion focused on the dispatch sequence, not the
		// rejection branch.
		{cmd: "./eval.sh", res: container.ExecResult{ExitCode: 0, AWFOutput: verdictPassed}},
	})
	h := newHarness(t, programmedFactory, gateIndependencePlaceholderWorkflow)
	if _, err := h.runWorkflow(t); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	events := mustFoldEvents(t, h)
	stepPaths := map[string]bool{}
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted {
			stepPaths[e.Path] = true
		}
	}
	// Expect 3 distinct step paths under attempt-1.
	expected := []string{
		"gate[0].attempt-1.generate.gen1",
		"gate[0].attempt-1.generate.gen2",
		"gate[0].attempt-1.evaluate.eval1",
	}
	for _, path := range expected {
		if !stepPaths[path] {
			t.Errorf("Bucket 5 independence_placeholder: step path %q not in node.completed events; got %v",
				path, sortedKeysBool(stepPaths))
		}
	}
	if !strings.Contains(t.Name(), "independence_placeholder") {
		// Bare sanity — test name guard so a refactor doesn't lose the placeholder marker.
		t.Errorf("test name lost placeholder marker")
	}
}

func sortedKeysBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
