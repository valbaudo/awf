package conformance

import (
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
)

func testGateEvaluatorContextEvidenceResume(t *testing.T, factory BackendFactory) {
	t.Helper()
	firstFake := fake.New("test/llm").
		WithCaps(agent.Caps{NativeSchema: true, Threaded: true, ContextEvidence: true}).
		Script(0, fake.Result{Output: map[string]any{}, Transcript: agent.ThreadTurn{User: "u1", Assistant: "a1"}}).
		Script(1, fake.Result{Output: map[string]any{}, Transcript: agent.ThreadTurn{User: "u2", Assistant: "a2"}}).
		Script(2, fake.Result{Output: map[string]any{"draft": "d"}, Transcript: agent.ThreadTurn{User: "u3", Assistant: "a3"}})

	register := func(reg *agent.Registry) {
		if err := reg.Register(firstFake); err != nil {
			t.Fatalf("Register firstFake: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, gatedEvaluatorContextEvidenceWorkflow, register)

	firstOutcome, firstErr := h.runWorkflow(t)
	if firstErr == nil {
		t.Fatalf("first runWorkflow err = nil, want missing judge script error")
	}
	if firstOutcome == engine.OutcomeOK {
		t.Fatalf("first Outcome = %q, want non-ok because judge was uncommitted", firstOutcome)
	}
	if len(firstFake.Calls()) != 4 {
		t.Fatalf("first fake Calls len = %d, want 4 including failed judge launch", len(firstFake.Calls()))
	}

	resumeFake := fake.New("test/llm").
		WithCaps(agent.Caps{NativeSchema: true, Threaded: true, ContextEvidence: true}).
		Script(0, fake.Result{Output: map[string]any{"verified": true, "fooled_by_benign": false, "feedback": ""}, Transcript: agent.ThreadTurn{User: "judge", Assistant: "approved"}})
	h.agentRegistry = &agent.Registry{}
	if err := h.agentRegistry.Register(resumeFake); err != nil {
		t.Fatalf("Register resumeFake: %v", err)
	}

	oc, err := h.resumeWorkflow(t)
	if err != nil {
		t.Fatalf("resume runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("resume Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := resumeFake.Calls()
	if len(calls) != 1 {
		t.Fatalf("resume fake Calls len = %d, want 1; committed source and generate steps should replay", len(calls))
	}
	want := []agent.ThreadTurn{{User: "u1", Assistant: "a1"}, {User: "u2", Assistant: "a2"}}
	if len(calls[0].Thread) != 0 {
		t.Fatalf("resume judge Thread = %+v, want empty", calls[0].Thread)
	}
	if calls[0].Feedback != nil {
		t.Fatalf("resume judge Feedback = %v, want nil", calls[0].Feedback)
	}
	if !reflect.DeepEqual(calls[0].ContextEvidence, want) {
		t.Fatalf("resume judge ContextEvidence = %+v, want %+v", calls[0].ContextEvidence, want)
	}
}
