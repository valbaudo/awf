package conformance

import (
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
)

// testThread is the T9/T10 dispatcher. Called from RunSuite as t.Run("thread", ...).
func testThread(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("branched", func(t *testing.T) { testThreadBranched(t, factory) })
	t.Run("fanout_prefix_identity", func(t *testing.T) { testThreadFanOut(t, factory) })
}

// testThreadFanOut is T10: a common pre-fork ancestor `seed` lives OUTSIDE the
// parallel; all three branches continues: seed AND share the identical
// system_prompt. The test asserts the byte-identity PRECONDITION for
// shared-prefix server-side caching (E.2.a): each branch's inv.Thread equals
// the single committed seed pair, all three threads are reflect.DeepEqual to
// each other, and all three carry the same system_prompt in with. Per-branch
// content lives only in the tail (with["prompt"]).
//
// Fake call-index model for test/chat (seed + 3 parallel branches, all share one fake):
//
//	[0] seed     → {u:"establish shared context", a:"SEED-A0"}
//	[1..3] branch_{a,b,c} (any order) — each continues: seed;
//	       Thread must equal [{seed pair}] for every branch.
func testThreadFanOut(t *testing.T, factory BackendFactory) {
	t.Helper()
	var chat *fake.Fake
	register := func(reg *agent.Registry) {
		// seed is call 0; the three branches are calls 1..3 in some order
		// (parallel). Each branch's transcript is distinct, but its assembled
		// PREFIX (system + thread) must be identical — that is the assertion.
		chat = fake.New("test/chat").
			Script(0, fake.Result{
				Output:     map[string]any{},
				Transcript: agent.ThreadTurn{User: "establish shared context", Assistant: "SEED-A0"},
			}).
			Script(1, fake.Result{Output: map[string]any{}, Transcript: agent.ThreadTurn{User: "branch tail", Assistant: "B1"}}).
			Script(2, fake.Result{Output: map[string]any{}, Transcript: agent.ThreadTurn{User: "branch tail", Assistant: "B2"}}).
			Script(3, fake.Result{Output: map[string]any{}, Transcript: agent.ThreadTurn{User: "branch tail", Assistant: "B3"}})
		if err := reg.Register(chat); err != nil {
			t.Fatalf("Register chat: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, threadFanOutWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}

	// Collect the three branch invocations (everything except the seed launch).
	// The seed is the call whose With.prompt == "establish shared context".
	calls := chat.Calls()
	if len(calls) != 4 {
		t.Fatalf("Calls len = %d, want 4 (seed + 3 branches)", len(calls))
	}
	var branches []agent.AgentInvocation
	for _, c := range calls {
		if c.With["prompt"] == "establish shared context" {
			continue
		}
		branches = append(branches, c)
	}
	if len(branches) != 3 {
		t.Fatalf("branch invocations = %d, want 3", len(branches))
	}

	// The cached prefix = (system_prompt, thread). Assert byte-identical across
	// all branches; per-branch content only in the tail (with.prompt).
	wantThread := []agent.ThreadTurn{{User: "establish shared context", Assistant: "SEED-A0"}}
	wantSystem := "SHARED-SYSTEM-PROMPT"
	tails := map[string]bool{}
	for i, b := range branches {
		if !reflect.DeepEqual(b.Thread, wantThread) {
			t.Errorf("branch %d thread = %+v, want %+v (every branch threads the shared seed)", i, b.Thread, wantThread)
		}
		if got, _ := b.With["system_prompt"].(string); got != wantSystem {
			t.Errorf("branch %d system_prompt = %q, want %q (shared prefix, E.2.a)", i, got, wantSystem)
		}
		// The differing tail lives in the user prompt, not the prefix.
		tail, _ := b.With["prompt"].(string)
		tails[tail] = true
	}
	// Pairwise byte-identity of the assembled cached region across branches.
	for i := 1; i < len(branches); i++ {
		if !reflect.DeepEqual(branches[0].Thread, branches[i].Thread) {
			t.Errorf("branch thread diverged: branch0=%+v branch%d=%+v (prefix must be byte-identical for shared-prefix caching)", branches[0].Thread, i, branches[i].Thread)
		}
		b0sys, _ := branches[0].With["system_prompt"].(string)
		bisys, _ := branches[i].With["system_prompt"].(string)
		if b0sys != bisys {
			t.Errorf("branch system_prompt diverged: branch0=%q branch%d=%q", b0sys, i, bisys)
		}
	}
	if len(tails) != 3 {
		t.Errorf("expected 3 distinct branch tails (per-branch variation in the user prompt), got %d: %v", len(tails), tails)
	}
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
