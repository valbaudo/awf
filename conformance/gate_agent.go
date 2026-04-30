package conformance

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
)

// testGateAgent is Bucket 13 (Phase 5 slice 5.2). Validates the gate's
// generate→evaluate→repair state machine works with agent-based steps —
// the engine path is unchanged from Phase 3.3, but with agent.Adapter
// instead of CodeStep dispatch.
//
//   - pass_on_attempt_1: oracle returns verified:true on call 0 → gate passes.
//   - repair_on_attempt_2_threads_feedback: oracle returns rejected on call 0,
//     verified on call 1; generator's With on call 1 contains the templated
//     feedback string from the prior verdict.
//   - max_attempts_rejected: oracle always returns rejected → gate exhausts
//     max_attempts and returns OutcomeRejected.
func testGateAgent(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("pass_on_attempt_1", func(t *testing.T) { testGateAgentPassOnAttempt1(t, factory) })
	t.Run("repair_on_attempt_2_threads_feedback", func(t *testing.T) { testGateAgentRepairOnAttempt2(t, factory) })
	t.Run("max_attempts_rejected", func(t *testing.T) { testGateAgentMaxAttemptsRejected(t, factory) })
}

func testGateAgentPassOnAttempt1(t *testing.T, factory BackendFactory) {
	t.Helper()
	register := func(reg *agent.Registry) {
		gen := fake.New("test/gen").Script(0, fake.Result{Output: map[string]any{"exploit": "real"}})
		oracle := fake.New("test/oracle").Script(0, fake.Result{Output: map[string]any{"verified": true, "fooled_by_benign": false, "feedback": ""}})
		if err := reg.Register(gen); err != nil {
			t.Fatalf("Register gen: %v", err)
		}
		if err := reg.Register(oracle); err != nil {
			t.Fatalf("Register oracle: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, gateAgentPassOnAttempt1Workflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}
}

func testGateAgentRepairOnAttempt2(t *testing.T, factory BackendFactory) {
	t.Helper()
	var capturedGen *fake.Fake
	register := func(reg *agent.Registry) {
		// Generator returns the same output each time; assertion below
		// inspects fake.Calls() to verify the templated With.prompt on
		// attempt 2 contains the prior verdict's feedback substring.
		capturedGen = fake.New("test/gen").
			Script(0, fake.Result{Output: map[string]any{"exploit": "v0"}}).
			Script(1, fake.Result{Output: map[string]any{"exploit": "v1"}})
		oracle := fake.NewBenignOracle() // attempt 0 → rejected; attempt 1 → verified
		if err := reg.Register(capturedGen); err != nil {
			t.Fatalf("Register capturedGen: %v", err)
		}
		if err := reg.Register(oracle); err != nil {
			t.Fatalf("Register oracle: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, gateAgentRepairOnAttempt2Workflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q (gate should pass on attempt 2)", oc, engine.OutcomeOK)
	}
	calls := capturedGen.Calls()
	if len(calls) != 2 {
		t.Fatalf("generator Calls len = %d, want 2 (one per attempt)", len(calls))
	}
	// Attempt 2's prompt MUST contain the feedback substring from attempt 1's
	// verdict ("exploit was fake — only matched a version string", per
	// fake.NewBenignOracle's script).
	prompt2, _ := calls[1].With["prompt"].(string)
	t.Logf("attempt 2 prompt: %q", prompt2)
	if !strings.Contains(prompt2, "exploit was fake") {
		t.Errorf("attempt 2 prompt did not include attempt 1 feedback: %q", prompt2)
	}
}

func testGateAgentMaxAttemptsRejected(t *testing.T, factory BackendFactory) {
	t.Helper()
	register := func(reg *agent.Registry) {
		gen := fake.New("test/gen").
			Script(0, fake.Result{Output: map[string]any{"exploit": "v0"}}).
			Script(1, fake.Result{Output: map[string]any{"exploit": "v1"}})
		oracle := fake.New("test/oracle").
			Script(0, fake.Result{Output: map[string]any{"verified": false, "fooled_by_benign": true, "feedback": "fail"}}).
			Script(1, fake.Result{Output: map[string]any{"verified": false, "fooled_by_benign": true, "feedback": "fail again"}})
		if err := reg.Register(gen); err != nil {
			t.Fatalf("Register gen: %v", err)
		}
		if err := reg.Register(oracle); err != nil {
			t.Fatalf("Register oracle: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, gateAgentMaxAttemptsRejectedWorkflow, register)
	oc, _ := h.runWorkflow(t)
	if oc != engine.OutcomeRejected {
		t.Fatalf("Outcome = %q, want %q (max_attempts:2 exhausted)", oc, engine.OutcomeRejected)
	}
}
