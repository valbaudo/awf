package conformance

import (
	"fmt"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
)

// juryGateWorkflow is the jury-panel end-to-end fixture: a gate whose
// generate: is a single agent step (test/jury-gen) and whose evaluate: is a
// single agent step carrying a `jury:` block — the sugar, NOT a hand-written
// map+quorum (loader.desugarJury lowers it before ir.Validate ever runs; this
// is the proof that the sugar path works end to end against the fake
// backend). Three jurors (over: model gpt-5/o3/sonnet-5) share ONE `uses:`
// ref (test/jury-judge) since they vary only by with:.model (E1) — the
// desugared map defaults to concurrency 1 (loader never sets it), so the 3
// jurors within one attempt dispatch serially, in over: order.
//
// quorum: 2 of 3. `field: accept` names the boolean field (also the sole
// boolean output_schema property, so this pins the explicit form alongside
// loader's jury-sugar.yaml testdata). `until: "{{ evaluate.accept }}"` reads
// the reduce's synthetic verdict — the reduced quorum output, never a
// juror's raw vote.
var juryGateWorkflow = fmt.Sprintf(`workflow: conformance-jury-gate
version: 1
containers:
  lab:
    image: %s
graph:
  - gate:
      generate:
        - id: gen
          container: lab
          uses: test/jury-gen
          with:
            prompt: "draft the artifact"
          output_schema:
            type: object
            additionalProperties: false
            required: [artifact]
            properties:
              artifact: { type: string }
      evaluate:
        - id: judge
          container: lab
          uses: test/jury-judge
          with:
            prompt: review
            model: base
          output_schema:
            type: object
            additionalProperties: false
            required: [accept, critique]
            properties:
              accept: { type: boolean }
              critique: { type: string }
          jury:
            over:
              - model: gpt-5
              - model: o3
              - model: sonnet-5
            quorum: 2
            field: accept
      until: "{{ evaluate.accept }}"
      max_attempts: 2
`, fakeImageDigest)

// juryGatePath / juryEvaluateMapPath mirror engine/gate.go's addressing
// (AttemptPath + ir.PathFor's bare map[i] terminal — engine/gate_test.go's
// TestLastEvaluatorPathMapTerminal pins the same formula) for a gate that is
// graph[0] with a single-node evaluate: list desugared to map[0].
const juryGatePath = "gate[0]"

func juryEvaluateMapPath(attempt int) string {
	return fmt.Sprintf("gate[0].attempt-%d.evaluate.map[0]", attempt)
}

// registerJuryFakes wires the two fake adapters the panel fixture needs:
//
//   - test/jury-gen: the generator, scripted per gate ATTEMPT (index 0 = attempt
//     1, index 1 = attempt 2).
//   - test/jury-judge: the ONE juror adapter shared by all 3 panel seats. Serial
//     concurrency:1 dispatch makes the 6 invocations deterministic by index:
//     0,1,2 = attempt 1's three jurors (in over: order gpt-5,o3,sonnet-5);
//     3,4,5 = attempt 2's three jurors. Attempt 1 votes [true,false,false] —
//     agree=1 < quorum 2 → the reduce commits accept:false and the gate
//     repairs. Attempt 2 votes [true,true,true] — agree=3 ≥ 2 → accept:true,
//     the gate passes. The two dissenting jurors' critique strings
//     (dissent1Substr, dissent2Substr) are the payload
//     testJuryGateVotesReachFeedback looks for in the generator's attempt-2
//     Feedback.
const (
	dissent1Substr = "exploit was superficial and unverifiable"
	dissent2Substr = "missing reproduction steps"
)

func registerJuryFakes(t *testing.T) (gen, judge *fake.Fake) {
	t.Helper()
	gen = fake.New("test/jury-gen").
		Script(0, fake.Result{Output: map[string]any{"artifact": "draft-v1"}}).
		Script(1, fake.Result{Output: map[string]any{"artifact": "draft-v2"}})
	judge = fake.New("test/jury-judge").
		// Attempt 1: 1 accept, 2 dissent — below quorum 2.
		Script(0, fake.Result{Output: map[string]any{"accept": true, "critique": "juror0/attempt1: looks solid"}}).
		Script(1, fake.Result{Output: map[string]any{"accept": false, "critique": "dissent: " + dissent1Substr}}).
		Script(2, fake.Result{Output: map[string]any{"accept": false, "critique": "dissent: " + dissent2Substr}}).
		// Attempt 2: unanimous accept — quorum met.
		Script(3, fake.Result{Output: map[string]any{"accept": true, "critique": "juror0/attempt2: fixed"}}).
		Script(4, fake.Result{Output: map[string]any{"accept": true, "critique": "juror1/attempt2: fixed"}}).
		Script(5, fake.Result{Output: map[string]any{"accept": true, "critique": "juror2/attempt2: fixed"}})
	return gen, judge
}

// testJuryGate is Task 5 of the jury-panel feature: the end-to-end
// conformance proof that a gate whose evaluate: is a jury: block (1) repairs
// when the panel votes below quorum, (2) passes when it reaches quorum, and
// (3) replays the committed verdict on fold without re-polling the panel.
func testJuryGate(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("repair_then_pass", func(t *testing.T) { testJuryGateRepairThenPass(t, factory) })
	t.Run("votes_reach_feedback", func(t *testing.T) { testJuryGateVotesReachFeedback(t, factory) })
	t.Run("fold_replay", func(t *testing.T) { testJuryGateFoldReplay(t, factory) })
	t.Run("resume_passed_gate", func(t *testing.T) { testJuryGateResumePassedGate(t, factory) })
}

// testJuryGateRepairThenPass is the core proof: attempt 1's below-quorum vote
// genuinely drives a second attempt (not a fixture that would pass even if
// repair never happened) — max_attempts:2 with the workflow's until: reading
// evaluate.accept means a gate that (wrongly) read attempt 1's per-juror vote,
// or that ignored the quorum entirely, would either exhaust max_attempts
// (OutcomeRejected) or invoke the judge fake a different number of times than
// 6 — both caught below.
func testJuryGateRepairThenPass(t *testing.T, factory BackendFactory) {
	t.Helper()
	var genFake, judgeFake *fake.Fake
	register := func(reg *agent.Registry) {
		genFake, judgeFake = registerJuryFakes(t)
		if err := reg.Register(genFake); err != nil {
			t.Fatalf("Register gen: %v", err)
		}
		if err := reg.Register(judgeFake); err != nil {
			t.Fatalf("Register judge: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, juryGateWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want %q (jury should reach quorum on attempt 2)", oc, engine.OutcomeOK)
	}

	// The gate ran exactly 2 attempts: 3 jurors invoked per attempt, twice.
	genCalls := genFake.Calls()
	judgeCalls := judgeFake.Calls()
	if len(genCalls) != 2 {
		t.Fatalf("generator Calls len = %d, want 2 (one per gate attempt)", len(genCalls))
	}
	if len(judgeCalls) != 6 {
		t.Fatalf("judge Calls len = %d, want 6 (3 jurors x 2 attempts)", len(judgeCalls))
	}

	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("Fold: %v", ferr)
	}

	// Exactly 2 committed gate.attempt entries: attempt 1 rejected (below
	// quorum), attempt 2 passed (quorum met). A gate that repaired a 3rd time,
	// or that never repaired at all, would fail this shape.
	attempts := rs.LookupGateAttempts(juryGatePath)
	if len(attempts) != 2 {
		t.Fatalf("GateAttempts len = %d, want 2", len(attempts))
	}
	if attempts[0].N != 1 || attempts[0].AttemptOutcome != engine.AttemptRejected {
		t.Errorf("attempt[0] = %+v, want N=1 AttemptRejected (1 of 3 accepts, below quorum 2)", attempts[0])
	}
	if attempts[1].N != 2 || attempts[1].AttemptOutcome != engine.AttemptPassed {
		t.Errorf("attempt[1] = %+v, want N=2 AttemptPassed (3 of 3 accept, quorum met)", attempts[1])
	}

	// Attempt 1's committed verdict at the jury map path: below quorum.
	nr1, ok := rs.LookupCompleted(juryEvaluateMapPath(1))
	if !ok {
		t.Fatalf("no node.completed at %q (attempt 1's jury verdict)", juryEvaluateMapPath(1))
	}
	if nr1.Outputs["accept"] != false {
		t.Errorf("attempt 1 verdict accept = %v, want false", nr1.Outputs["accept"])
	}
	if nr1.Outputs["agree"] != float64(1) {
		t.Errorf("attempt 1 verdict agree = %v, want 1", nr1.Outputs["agree"])
	}

	// Attempt 2's committed verdict: quorum met, accept:true — the gate's
	// PASSING attempt, and the value `until: "{{ evaluate.accept }}"` read true
	// against.
	nr2, ok := rs.LookupCompleted(juryEvaluateMapPath(2))
	if !ok {
		t.Fatalf("no node.completed at %q (attempt 2's jury verdict)", juryEvaluateMapPath(2))
	}
	if nr2.Outputs["accept"] != true {
		t.Errorf("attempt 2 verdict accept = %v, want true", nr2.Outputs["accept"])
	}
	if nr2.Outputs["votes"] != float64(3) {
		t.Errorf("attempt 2 verdict votes = %v, want 3", nr2.Outputs["votes"])
	}
	if nr2.Outputs["agree"] != float64(3) {
		t.Errorf("attempt 2 verdict agree = %v, want 3", nr2.Outputs["agree"])
	}
}

// testJuryGateVotesReachFeedback is the C1 point: the panel's dissent from
// attempt 1 must reach the generator's attempt-2 invocation, not just the
// scalar accept/reject tally. The gate's own `until: evaluate.accept` only
// resolves a SCALAR (AWF templating renders scalars only — an array like
// votes_detail would trip AWF4004 if the workflow tried to template it into
// with:), so this cannot be proven via a rendered With.prompt the way
// gate_agent.go's testGateAgentRepairOnAttempt2 proves scalar feedback
// threading. Instead this inspects AgentInvocation.Feedback directly
// (engine/agent_step.go's slice-5.3 auto-feed: the ENTIRE previous verdict map
// — not run through template substitution) via genFake.Calls(), which is the
// one channel that carries votes_detail unflattened.
func testJuryGateVotesReachFeedback(t *testing.T, factory BackendFactory) {
	t.Helper()
	var genFake *fake.Fake
	register := func(reg *agent.Registry) {
		var judgeFake *fake.Fake
		genFake, judgeFake = registerJuryFakes(t)
		if err := reg.Register(genFake); err != nil {
			t.Fatalf("Register gen: %v", err)
		}
		if err := reg.Register(judgeFake); err != nil {
			t.Fatalf("Register judge: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, juryGateWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := genFake.Calls()
	if len(calls) != 2 {
		t.Fatalf("generator Calls len = %d, want 2", len(calls))
	}
	fb := calls[1].Feedback
	if fb == nil {
		t.Fatalf("generator attempt-2 Feedback is nil; want attempt 1's verdict auto-fed")
	}
	if fb["accept"] != false {
		t.Errorf("attempt-2 Feedback[accept] = %v, want false (attempt 1's tally)", fb["accept"])
	}
	vd, ok := fb["votes_detail"].([]map[string]any)
	if !ok {
		t.Fatalf("attempt-2 Feedback[votes_detail] = %#v (%T), want []map[string]any", fb["votes_detail"], fb["votes_detail"])
	}
	if len(vd) != 3 {
		t.Fatalf("attempt-2 Feedback[votes_detail] len = %d, want 3 ballots", len(vd))
	}
	var sawDissent1, sawDissent2 bool
	for _, ballot := range vd {
		out, ok := ballot["output"].(map[string]any)
		if !ok {
			t.Fatalf("ballot %+v: output = %#v, want map[string]any", ballot, ballot["output"])
		}
		critique, _ := out["critique"].(string)
		if strings.Contains(critique, dissent1Substr) {
			sawDissent1 = true
		}
		if strings.Contains(critique, dissent2Substr) {
			sawDissent2 = true
		}
	}
	if !sawDissent1 || !sawDissent2 {
		t.Errorf("attempt-2 Feedback[votes_detail] did not carry both dissenting jurors' critiques (saw1=%v saw2=%v): %+v",
			sawDissent1, sawDissent2, vd)
	}
}

// testJuryGateFoldReplay proves the panel's verdict is fold-stable: TWO
// independent folds of the same committed journal resolve the IDENTICAL
// accept:true verdict at the jury map path, and neither fold ever touches the
// judge or generator fakes (engine.Fold is a pure function of log+blobs — it
// never calls agent.Adapter.Launch; mirrors the obs bucket's
// byte_identical_replay proof of Fold's determinism).
//
// This is the pure-Fold proof (verdict recoverable + stable across replays).
// testJuryGateResumePassedGate is the companion REAL h.resumeWorkflow()
// re-entry proof, now that runGate short-circuits an already-passed gate to
// OutcomeOK (engine/gate.go) instead of falling through to OutcomeRejected.
func testJuryGateFoldReplay(t *testing.T, factory BackendFactory) {
	t.Helper()
	var genFake, judgeFake *fake.Fake
	register := func(reg *agent.Registry) {
		genFake, judgeFake = registerJuryFakes(t)
		if err := reg.Register(genFake); err != nil {
			t.Fatalf("Register gen: %v", err)
		}
		if err := reg.Register(judgeFake); err != nil {
			t.Fatalf("Register judge: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, juryGateWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	preGenCalls := len(genFake.Calls())
	preJudgeCalls := len(judgeFake.Calls())
	if preGenCalls != 2 || preJudgeCalls != 6 {
		t.Fatalf("pre-fold call counts = (gen=%d, judge=%d), want (2, 6)", preGenCalls, preJudgeCalls)
	}

	// Fold #1: proves the aggregate verdict is recoverable from the journal
	// (never calls Launch — a pure function of log+blobs).
	events := mustFoldEvents(t, h)
	rs1, ferr := engine.Fold(events, h.blobs)
	if ferr != nil {
		t.Fatalf("Fold: %v", ferr)
	}
	nr1, ok := rs1.LookupCompleted(juryEvaluateMapPath(2))
	if !ok {
		t.Fatalf("no node.completed at %q after fold (the committed jury verdict)", juryEvaluateMapPath(2))
	}
	if nr1.Outputs["accept"] != true {
		t.Errorf("folded verdict accept = %v, want true", nr1.Outputs["accept"])
	}
	vd1, ok := nr1.Outputs["votes_detail"].([]any)
	if !ok || len(vd1) != 3 {
		t.Errorf("folded verdict votes_detail = %v, want 3 ballots", nr1.Outputs["votes_detail"])
	}
	if len(genFake.Calls()) != preGenCalls || len(judgeFake.Calls()) != preJudgeCalls {
		t.Fatalf("Fold invoked an adapter: call counts changed to (gen=%d, judge=%d); Fold must be a pure read of log+blobs",
			len(genFake.Calls()), len(judgeFake.Calls()))
	}

	// Fold #2: a SECOND independent fold of the identical log must resolve the
	// SAME verdict (byte-identical-replay style, mirrors the obs bucket) and
	// still never touch either fake — proving the committed panel result is
	// stable across arbitrarily many replays, not just recoverable once.
	rs2, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("second Fold: %v", ferr)
	}
	nr2, ok := rs2.LookupCompleted(juryEvaluateMapPath(2))
	if !ok {
		t.Fatalf("no node.completed at %q on second fold", juryEvaluateMapPath(2))
	}
	if nr2.Outputs["accept"] != nr1.Outputs["accept"] || nr2.Outputs["agree"] != nr1.Outputs["agree"] {
		t.Errorf("second fold verdict = %+v, want identical to first fold %+v", nr2.Outputs, nr1.Outputs)
	}
	if got := len(genFake.Calls()); got != preGenCalls {
		t.Errorf("generator call count changed across a second fold: %d -> %d; Fold must never invoke an adapter", preGenCalls, got)
	}
	if got := len(judgeFake.Calls()); got != preJudgeCalls {
		t.Errorf("judge call count changed across a second fold: %d -> %d; the committed panel verdict must never be re-polled by a fold", preJudgeCalls, got)
	}
}

// testJuryGateResumePassedGate proves the runGate resume short-circuit
// (engine/gate.go): resuming a run whose gate already fully passed (max_attempts:2,
// passed on attempt 2) replays OutcomeOK and re-dispatches NOTHING — neither
// generator nor jury panel. Before the fix, runGate computed
// startN = len(GateAttempts)+1 = 3 > MaxAttempts, skipped the attempt loop, and
// fell through to OutcomeRejected — an already-passed gate mis-reporting rejection
// on the second Run against the completed log.
func testJuryGateResumePassedGate(t *testing.T, factory BackendFactory) {
	t.Helper()
	var genFake, judgeFake *fake.Fake
	register := func(reg *agent.Registry) {
		genFake, judgeFake = registerJuryFakes(t)
		if err := reg.Register(genFake); err != nil {
			t.Fatalf("Register gen: %v", err)
		}
		if err := reg.Register(judgeFake); err != nil {
			t.Fatalf("Register judge: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, juryGateWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want %q", oc, engine.OutcomeOK)
	}
	preGenCalls := len(genFake.Calls())
	preJudgeCalls := len(judgeFake.Calls())
	if preGenCalls != 2 || preJudgeCalls != 6 {
		t.Fatalf("pre-resume call counts = (gen=%d, judge=%d), want (2, 6)", preGenCalls, preJudgeCalls)
	}

	// Resume: engine.Run a second time against the completed log. The gate is
	// already passed, so it must replay OutcomeOK without re-entering the attempt
	// loop.
	oc2, err := h.resumeWorkflow(t)
	if err != nil {
		t.Fatalf("resumeWorkflow: %v", err)
	}
	if oc2 != engine.OutcomeOK {
		t.Fatalf("resumed outcome = %q, want %q (an already-passed gate must replay OK, not re-report rejection)", oc2, engine.OutcomeOK)
	}

	// No re-dispatch: committed generate/evaluate steps are replayed from the
	// journal, never recomputed.
	if got := len(genFake.Calls()); got != preGenCalls {
		t.Errorf("generator call count changed across resume: %d -> %d; a passed gate's generate must not re-dispatch", preGenCalls, got)
	}
	if got := len(judgeFake.Calls()); got != preJudgeCalls {
		t.Errorf("judge call count changed across resume: %d -> %d; a passed gate's panel must not be re-polled", preJudgeCalls, got)
	}

	// The accepted attempt (N=2, passed) and its verdict still resolve from the
	// resumed log — the OutcomeOK replays the committed verdict, it does not
	// discard it.
	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("Fold after resume: %v", ferr)
	}
	attempts := rs.LookupGateAttempts(juryGatePath)
	if len(attempts) != 2 || attempts[1].AttemptOutcome != engine.AttemptPassed {
		t.Fatalf("post-resume GateAttempts = %+v, want 2 with attempt[1] AttemptPassed", attempts)
	}
	nr, ok := rs.LookupCompleted(juryEvaluateMapPath(2))
	if !ok {
		t.Fatalf("no node.completed at %q after resume (accepted verdict must replay)", juryEvaluateMapPath(2))
	}
	if nr.Outputs["accept"] != true {
		t.Errorf("resumed accepted verdict accept = %v, want true", nr.Outputs["accept"])
	}
}
