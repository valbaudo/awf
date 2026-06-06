package conformance

import (
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
)

// testThread is the T9 dispatcher. Called from RunSuite as t.Run("thread", ...).
func testThread(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("branched", func(t *testing.T) { testThreadBranched(t, factory) })
}

// testThreadBranched is T9: draft → critique → if/else where BOTH forks declare
// continues: critique (a dominating common ancestor). cond is static-true so the
// `then` fork (revise_then) is taken deterministically. The else fork must never
// run. The taken fork's assembled Thread must equal [draft-pair, critique-pair] —
// proving the engine resolves the pre-fork ancestor correctly through an if-node.
//
// Fake call-index model for test/chat (all three running steps share one fake):
//
//	[0] draft      → {u:"draft a plan",       a:"DRAFT-A0"}
//	[1] critique   → continues draft; Thread=[{draft pair}] (not asserted here)
//	[2] revise_then → continues critique; Thread=[{draft pair},{critique pair}]  ← KEY assertion
//
// The else fork (revise_else) must NOT appear in Calls().
func testThreadBranched(t *testing.T, factory BackendFactory) {
	t.Helper()
	var chat *fake.Fake
	register := func(reg *agent.Registry) {
		// Distinct strings on BOTH halves per turn (B.2): vacuous otherwise.
		chat = fake.New("test/chat").
			Script(0, fake.Result{
				Output:     map[string]any{},
				Transcript: agent.ThreadTurn{User: "draft a plan", Assistant: "DRAFT-A0"},
			}).
			Script(1, fake.Result{
				Output:     map[string]any{},
				Transcript: agent.ThreadTurn{User: "critique the draft", Assistant: "CRITIQUE-A1"},
			}).
			Script(2, fake.Result{
				Output:     map[string]any{},
				Transcript: agent.ThreadTurn{User: "revise per the critique (then-fork)", Assistant: "REVISE-A2"},
			})
		if err := reg.Register(chat); err != nil {
			t.Fatalf("Register chat: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, threadBranchedWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}
	calls := chat.Calls()
	if len(calls) != 3 {
		t.Fatalf("Calls len = %d, want 3 (draft, critique, taken-fork revise)", len(calls))
	}
	// The taken (then) fork is call index 2. Its thread must be the two
	// committed verbatim pairs in root→current order.
	want := []agent.ThreadTurn{
		{User: "draft a plan", Assistant: "DRAFT-A0"},
		{User: "critique the draft", Assistant: "CRITIQUE-A1"},
	}
	if !reflect.DeepEqual(calls[2].Thread, want) {
		t.Errorf("taken-fork thread = %+v, want %+v", calls[2].Thread, want)
	}
	// The else fork was never taken → never launched.
	for i, c := range calls {
		if c.With["prompt"] == "revise per the critique (else-fork)" {
			t.Errorf("else-fork was launched at call %d; cond is static-true, only the then-fork should run", i)
		}
	}
}
