package conformance

import (
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	agentfake "github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
)

// testContinuesLoopMap is the T11/T12 conformance bucket: continues: chain
// INSIDE a loop body (intra-iteration) and a map body (intra-item).
//
// The load-bearing invariant: stepRuntimePath resolves a step's continues
// predecessor to the SAME iteration/item the consumer lives in (it inserts
// iter-K / item-K from the consumer's own ctxPath). This means:
//
//   - iter-1's `refine` threads iter-1's `ask` — NOT iter-0 or empty.
//   - iter-2's `refine` threads iter-2's `ask` — NOT iter-1 or iter-0.
//
// Cross-iteration accumulation (iter-2 refine seeing iter-1's ask) is NOT
// in scope for v1 and would be a real addressing bug if it appeared.
func testContinuesLoopMap(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("loop_body_intra_iter", func(t *testing.T) { testContinuesLoopBodyIntraIter(t, factory) })
	t.Run("map_body_intra_item", func(t *testing.T) { testContinuesMapBodyIntraItem(t, factory) })
}

// continuesLoopBodyWorkflow — T11. A loop with max_iters: 2 whose body is
// [ask, refine continues: ask]. Both steps use the awf/llm containerless
// adapter (Containerless: true, Threaded: true). The loop has no `until`
// condition — it runs exactly max_iters times.
//
// Expected runtime paths (loop[0] is the first and only loop node):
//
//	iter-1: loop[0].body.iter-1.ask   call 0
//	        loop[0].body.iter-1.refine call 1  (threads iter-1's ask)
//	iter-2: loop[0].body.iter-2.ask   call 2
//	        loop[0].body.iter-2.refine call 3  (threads iter-2's ask)
const continuesLoopBodyWorkflow = `workflow: conformance-continues-loop-body
version: 1
graph:
  - loop:
      max_iters: 2
      body:
        - id: ask
          uses: awf/llm
          with:
            model: m
            prompt: "ask a question"
        - id: refine
          uses: awf/llm
          continues: ask
          with:
            model: m
            prompt: "refine the answer"
          output_schema:
            type: object
            additionalProperties: false
            required: [draft]
            properties:
              draft: { type: string }
`

// continuesMapBodyWorkflow — T12. A map over a 2-element list with body
// [ask, refine continues: ask]. Both steps use the awf/llm containerless
// adapter. concurrency: 1 serializes items so call indices are deterministic.
//
// The map itself must declare a container (AWF §5.7 requires Map.Container),
// so we declare a lab container but the body steps omit container: — the
// Containerless adapter ignores the handle.
//
// Expected runtime paths (map[0] is the first and only map node):
//
//	item-0: map[0].item-0.ask    call 0
//	        map[0].item-0.refine call 1  (threads item-0's ask)
//	item-1: map[0].item-1.ask    call 2
//	        map[0].item-1.refine call 3  (threads item-1's ask)
var continuesMapBodyWorkflow = `workflow: conformance-continues-map-body
version: 1
input_schema:
  type: object
  required: [items]
  additionalProperties: false
  properties:
    items:
      type: array
      items: { type: string }
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - map:
      over: "{{ input.items }}"
      as: x
      container: lab
      concurrency: 1
      body:
        - id: ask
          uses: awf/llm
          with:
            model: m
            prompt: "ask about {{ x }}"
        - id: refine
          uses: awf/llm
          continues: ask
          with:
            model: m
            prompt: "refine about {{ x }}"
          output_schema:
            type: object
            additionalProperties: false
            required: [draft]
            properties:
              draft: { type: string }
`

// testContinuesLoopBodyIntraIter is T11: a loop with max_iters:2 whose body
// is [ask, refine continues: ask]. The test asserts that in EACH iteration,
// refine's assembled Thread equals [that iteration's ask transcript] — i.e.
// iter-1's refine threads iter-1's ask, iter-2's refine threads iter-2's ask.
//
// This pins the intra-iteration resolution contract: stepRuntimePath inserts
// iter-K from the consumer's own ctxPath, so refine inside iter-2 resolves
// its `continues: ask` to iter-2's ask, NOT iter-1's. If cross-iteration leak
// were present, iter-2's refine would receive [{Q1,A1},{Q2,A2}] (accumulation)
// or [{Q1,A1}] (wrong-iter) instead of [{Q2,A2}] — the assertion catches it.
//
// Fake call-index model (ask and refine share one fake; call index advances):
//
//	[0] ask    iter-1 → {Q1, A1}
//	[1] refine iter-1 → continues ask(iter-1); Thread = [{Q1, A1}]  ← iter-1 key assertion
//	[2] ask    iter-2 → {Q2, A2}
//	[3] refine iter-2 → continues ask(iter-2); Thread = [{Q2, A2}]  ← iter-2 key assertion
func testContinuesLoopBodyIntraIter(t *testing.T, factory BackendFactory) {
	t.Helper()
	var llmFake *agentfake.Fake
	register := func(reg *agent.Registry) {
		llmFake = agentfake.New("awf/llm").
			// Containerless:true — loop body steps declare no container:.
			// Threaded:true — the engine guard rejects a non-Threaded adapter
			// when inv.Thread is non-empty (defense-in-depth). refine uses
			// continues:ask, so the adapter must declare Threaded support.
			WithCaps(agent.Caps{Containerless: true, Threaded: true}).
			Script(0, agentfake.Result{
				Output:     map[string]any{},
				Transcript: agent.ThreadTurn{User: "Q1", Assistant: "A1"},
			}).
			Script(1, agentfake.Result{
				Output:     map[string]any{"draft": "d1"},
				Transcript: agent.ThreadTurn{User: "R1", Assistant: "B1"},
			}).
			Script(2, agentfake.Result{
				Output:     map[string]any{},
				Transcript: agent.ThreadTurn{User: "Q2", Assistant: "A2"},
			}).
			Script(3, agentfake.Result{
				Output:     map[string]any{"draft": "d2"},
				Transcript: agent.ThreadTurn{User: "R2", Assistant: "B2"},
			})
		if err := reg.Register(llmFake); err != nil {
			t.Fatalf("Register llmFake: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, continuesLoopBodyWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}

	calls := llmFake.Calls()
	// 4 calls: ask(iter-1)=0, refine(iter-1)=1, ask(iter-2)=2, refine(iter-2)=3.
	if len(calls) != 4 {
		t.Fatalf("Calls len = %d, want 4 (ask+refine across 2 iters)", len(calls))
	}

	// ask has no continues → empty Thread on both iterations.
	if len(calls[0].Thread) != 0 {
		t.Errorf("ask iter-1 Thread = %+v, want empty (no predecessor)", calls[0].Thread)
	}
	if len(calls[2].Thread) != 0 {
		t.Errorf("ask iter-2 Thread = %+v, want empty (no predecessor)", calls[2].Thread)
	}

	// ITER-1 KEY ASSERTION: refine in iter-1 (call 1) threads iter-1's ask only.
	// If empty → continues resolution failed to find the committed turn.
	// If [{Q2,A2}] or [{Q1,A1},{Q2,A2}] → cross-iteration leak.
	wantThread1 := []agent.ThreadTurn{{User: "Q1", Assistant: "A1"}}
	if !reflect.DeepEqual(calls[1].Thread, wantThread1) {
		t.Errorf("refine iter-1 Thread = %+v, want %+v "+
			"(intra-iter: iter-1's refine must thread iter-1's ask, NOT iter-2 or empty)",
			calls[1].Thread, wantThread1)
	}

	// ITER-2 KEY ASSERTION: refine in iter-2 (call 3) threads iter-2's ask only.
	// If [{Q1,A1}] → wrong-iter (iter-2 resolved to iter-1's ask — addressing bug).
	// If [{Q1,A1},{Q2,A2}] → cross-iteration accumulation (out-of-scope for v1; real bug).
	wantThread2 := []agent.ThreadTurn{{User: "Q2", Assistant: "A2"}}
	if !reflect.DeepEqual(calls[3].Thread, wantThread2) {
		t.Errorf("refine iter-2 Thread = %+v, want %+v "+
			"(intra-iter: iter-2's refine must thread iter-2's ask ONLY — "+
			"NOT iter-1's A1 or both, which would be cross-iteration accumulation or wrong-iter)",
			calls[3].Thread, wantThread2)
	}
}

// testContinuesMapBodyIntraItem is T12: a map over 2 items with body
// [ask, refine continues: ask] and concurrency:1. The test asserts that for
// EACH item, refine's assembled Thread equals [that item's ask transcript] —
// i.e. item-0's refine threads item-0's ask, item-1's refine threads item-1's ask.
//
// This pins the intra-item resolution contract: stepRuntimePath inserts
// item-K from the consumer's own ctxPath, so refine inside item-1 resolves
// its `continues: ask` to item-1's ask, NOT item-0's.
//
// Fake call-index model (concurrency:1 serializes items; ask and refine share
// one fake; call index advances across both items):
//
//	[0] ask    item-0 → {Q1, A1}
//	[1] refine item-0 → continues ask(item-0); Thread = [{Q1, A1}]  ← item-0 key assertion
//	[2] ask    item-1 → {Q2, A2}
//	[3] refine item-1 → continues ask(item-1); Thread = [{Q2, A2}]  ← item-1 key assertion
func testContinuesMapBodyIntraItem(t *testing.T, factory BackendFactory) {
	t.Helper()
	var llmFake *agentfake.Fake
	register := func(reg *agent.Registry) {
		llmFake = agentfake.New("awf/llm").
			// Containerless:true — map body steps declare no container:.
			// Threaded:true — the engine guard rejects a non-Threaded adapter
			// when inv.Thread is non-empty.
			WithCaps(agent.Caps{Containerless: true, Threaded: true}).
			Script(0, agentfake.Result{
				Output:     map[string]any{},
				Transcript: agent.ThreadTurn{User: "Q1", Assistant: "A1"},
			}).
			Script(1, agentfake.Result{
				Output:     map[string]any{"draft": "d1"},
				Transcript: agent.ThreadTurn{User: "R1", Assistant: "B1"},
			}).
			Script(2, agentfake.Result{
				Output:     map[string]any{},
				Transcript: agent.ThreadTurn{User: "Q2", Assistant: "A2"},
			}).
			Script(3, agentfake.Result{
				Output:     map[string]any{"draft": "d2"},
				Transcript: agent.ThreadTurn{User: "R2", Assistant: "B2"},
			})
		if err := reg.Register(llmFake); err != nil {
			t.Fatalf("Register llmFake: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, continuesMapBodyWorkflow, register)
	h.input = map[string]any{"items": []any{"alpha", "beta"}}
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}

	calls := llmFake.Calls()
	// 4 calls: ask(item-0)=0, refine(item-0)=1, ask(item-1)=2, refine(item-1)=3.
	if len(calls) != 4 {
		t.Fatalf("Calls len = %d, want 4 (ask+refine across 2 items)", len(calls))
	}

	// ask has no continues → empty Thread on both items.
	if len(calls[0].Thread) != 0 {
		t.Errorf("ask item-0 Thread = %+v, want empty (no predecessor)", calls[0].Thread)
	}
	if len(calls[2].Thread) != 0 {
		t.Errorf("ask item-1 Thread = %+v, want empty (no predecessor)", calls[2].Thread)
	}

	// ITEM-0 KEY ASSERTION: refine in item-0 (call 1) threads item-0's ask only.
	wantThread0 := []agent.ThreadTurn{{User: "Q1", Assistant: "A1"}}
	if !reflect.DeepEqual(calls[1].Thread, wantThread0) {
		t.Errorf("refine item-0 Thread = %+v, want %+v "+
			"(intra-item: item-0's refine must thread item-0's ask, NOT item-1 or empty)",
			calls[1].Thread, wantThread0)
	}

	// ITEM-1 KEY ASSERTION: refine in item-1 (call 3) threads item-1's ask only.
	// If [{Q1,A1}] → wrong-item (item-1 resolved to item-0's ask — addressing bug).
	// Cross-item refs are rejected at stepRuntimePath with an error, so [{Q1,A1},{Q2,A2}]
	// accumulation is structurally impossible; the single-item thread is the only valid outcome.
	wantThread1 := []agent.ThreadTurn{{User: "Q2", Assistant: "A2"}}
	if !reflect.DeepEqual(calls[3].Thread, wantThread1) {
		t.Errorf("refine item-1 Thread = %+v, want %+v "+
			"(intra-item: item-1's refine must thread item-1's ask ONLY — "+
			"NOT item-0's A1, which would indicate a wrong-item addressing bug)",
			calls[3].Thread, wantThread1)
	}
}
