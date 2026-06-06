package conformance

import (
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
)

// testGateAgentEvaluatorIndependence is T4: a gated leaf (revise continues:
// critique) passes on attempt 1; the evaluator (judge, no continues) MUST
// receive an empty Thread — fresh context, gate integrity (D8).
//
// The workflow has two plain sequential turns before the gate (draft, critique)
// and a gate whose generate leaf (revise) continues critique. The evaluator
// (judge) uses a SEPARATE fake adapter (test/oracle) and declares NO continues.
// By construction the engine assembles no thread for the judge — it participates
// in no conversation chain. The test asserts this structurally by inspecting
// oracleFake.Calls(): every oracle call must have an empty Thread slice.
//
// Fake call-index model for test/llm (draft, critique, revise share one fake):
//
//	[0] draft     → {u1, a1}
//	[1] critique  → continues draft; Thread = [{u1,a1}]
//	[2] revise    → continues critique; Thread = [{u1,a1},{u2,a2}]
//
// The oracle (test/oracle) gets call [0] → {verified:true, ...}; gate passes.
func testGateAgentEvaluatorIndependence(t *testing.T, factory BackendFactory) {
	t.Helper()
	var oracleFake *fake.Fake
	register := func(reg *agent.Registry) {
		gen := fake.New("test/llm").
			Script(0, fake.Result{Output: map[string]any{}, Transcript: agent.ThreadTurn{User: "u1", Assistant: "a1"}}).            // draft
			Script(1, fake.Result{Output: map[string]any{}, Transcript: agent.ThreadTurn{User: "u2", Assistant: "a2"}}).            // critique
			Script(2, fake.Result{Output: map[string]any{"draft": "d"}, Transcript: agent.ThreadTurn{User: "u3", Assistant: "a3"}}) // revise
		oracleFake = fake.New("test/oracle").
			Script(0, fake.Result{Output: map[string]any{"verified": true, "fooled_by_benign": false, "feedback": ""}})
		if err := reg.Register(gen); err != nil {
			t.Fatalf("Register gen: %v", err)
		}
		if err := reg.Register(oracleFake); err != nil {
			t.Fatalf("Register oracle: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, gatedLeafThreadWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}
	calls := oracleFake.Calls()
	if len(calls) != 1 {
		t.Fatalf("oracle Calls len = %d, want 1", len(calls))
	}
	if len(calls[0].Thread) != 0 {
		t.Errorf("evaluator Thread = %+v, want empty (fresh-context gate integrity, D8)", calls[0].Thread)
	}
}

// testGateAgentThread is the T2 bucket (Phase 5 slice 5.3): a sub-conversation
// INSIDE one gate's generate — generate: [ask, refine continues: ask]. ask runs
// once per attempt; refine continues ask. The gate fails attempt 1 (oracle
// rejects via fooled_by_benign) and passes attempt 2.
//
// The load-bearing assertion: refine's assembled Thread in the PASSING attempt
// contains attempt-2's ask transcript ("A2") and NOT attempt-1's ("A1"). This
// proves stepRuntimePath resolves refine's `continues: ask` to its OWN attempt
// (gate[0].attempt-2.generate.ask), not the rejected attempt-1 ask. Rejected
// attempts still commit their transcripts; they are excluded from the passing
// thread by ADDRESSING (same-attempt resolution), not by skipping the write.
//
// Fake call-index model — both ask and refine declare uses: test/llm, so the
// SAME fake instance handles them and its call index advances across both. The
// gate executor runs generate steps in document order, then evaluate, per
// attempt (engine/gate.go). judge uses test/oracle (a separate fake). So the
// test/llm fake's calls are:
//
//	[0] ask    attempt 1  → {Q1, A1}
//	[1] refine attempt 1  → continues ask(att1); Thread = [{Q1, A1}]
//	[2] ask    attempt 2  → {Q2, A2}
//	[3] refine attempt 2  → continues ask(att2); Thread = [{Q2, A2}]   <- passing
func testGateAgentThread(t *testing.T, factory BackendFactory) {
	t.Helper()
	var llmFake *fake.Fake
	register := func(reg *agent.Registry) {
		// ask and refine SHARE uses: test/llm → one fake, interleaved indices.
		// ask is scripted with a distinct transcript per attempt (A1 then A2);
		// refine's own output carries the typed draft and a transcript that is
		// never asserted (refine is the consumer, not a predecessor here).
		llmFake = fake.New("test/llm").
			Script(0, fake.Result{Output: map[string]any{}, Transcript: agent.ThreadTurn{User: "Q1", Assistant: "A1"}}).
			Script(1, fake.Result{Output: map[string]any{"draft": "d1"}, Transcript: agent.ThreadTurn{User: "R1", Assistant: "B1"}}).
			Script(2, fake.Result{Output: map[string]any{}, Transcript: agent.ThreadTurn{User: "Q2", Assistant: "A2"}}).
			Script(3, fake.Result{Output: map[string]any{"draft": "d2"}, Transcript: agent.ThreadTurn{User: "R2", Assistant: "B2"}})
		oracle := fake.NewBenignOracle() // call 0 → rejected (fooled_by_benign); call 1 → verified
		if err := reg.Register(llmFake); err != nil {
			t.Fatalf("Register llmFake: %v", err)
		}
		if err := reg.Register(oracle); err != nil {
			t.Fatalf("Register oracle: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, gateAgentThreadSubConversationWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q (gate passes on attempt 2)", oc, engine.OutcomeOK)
	}

	calls := llmFake.Calls()
	// 4 calls: ask(att1)=0, refine(att1)=1, ask(att2)=2, refine(att2)=3.
	if len(calls) != 4 {
		t.Fatalf("test/llm Calls len = %d, want 4 (ask+refine across 2 attempts)", len(calls))
	}

	// ask carries no continues → empty Thread on both attempts.
	if len(calls[0].Thread) != 0 {
		t.Errorf("ask attempt-1 Thread = %+v, want empty (no predecessor)", calls[0].Thread)
	}
	if len(calls[2].Thread) != 0 {
		t.Errorf("ask attempt-2 Thread = %+v, want empty (no predecessor)", calls[2].Thread)
	}

	// refine on the REJECTED attempt 1 (call 1) threads attempt-1's ask only.
	wantThread1 := []agent.ThreadTurn{{User: "Q1", Assistant: "A1"}}
	if !reflect.DeepEqual(calls[1].Thread, wantThread1) {
		t.Errorf("refine attempt-1 Thread = %+v, want %+v", calls[1].Thread, wantThread1)
	}

	// THE KEY ASSERTION: refine on the PASSING attempt 2 (call 3) threads
	// attempt-2's ask transcript ("A2") and MUST NOT contain attempt-1's ("A1").
	// If A1 leaks in here, that is a real cross-attempt addressing bug — do not
	// weaken this assertion.
	wantThread2 := []agent.ThreadTurn{{User: "Q2", Assistant: "A2"}}
	if !reflect.DeepEqual(calls[3].Thread, wantThread2) {
		t.Errorf("refine attempt-2 Thread = %+v, want %+v (must NOT contain rejected attempt-1's A1)", calls[3].Thread, wantThread2)
	}
}
