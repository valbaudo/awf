package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// testBucket14aSimpleSchema (Bucket 14a per Phase 5 design decision 12).
// Drives the factory's Adapter against a trivial prompt + integer-
// returning schema. Asserts typed Output correctness.
//
// NO cost-billing assertion: Metrics.Cost.USD can be 0 legitimately
// (cache hits, Cost.Source="unavailable"). The bucket locks typed-
// output round-trip — billing is observability, not contract.
//
// Realtime UX (events arrive progressively) is locked by slice 5.3's
// TestClaudeAdapterRealtimeStreaming, not here. 14a uses synchronous
// drain (`for range events {}` BEFORE `<-outcomeCh`) which is fine
// for the 2+2 happy path: ~3 events fit in slice 5.3's 16-event
// buffer, so the adapter writer doesn't block. A multi-tool-call
// workflow would need concurrent drain.
func testBucket14aSimpleSchema(t *testing.T, factory AgentBackendFactory) {
	t.Helper()
	env := factory(t)

	// 120s ctx: cold claude startup + structuring-call retries +
	// network blip on CI. 60s would be tight on a loaded runner.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	h, err := env.Backend.Create(ctx, env.Spec)
	if err != nil {
		t.Fatalf("Backend.Create: %v", err)
	}
	t.Cleanup(func() { _ = env.Backend.Destroy(context.Background(), h) })

	schema := ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"answer"},
		"properties": map[string]any{
			"answer": map[string]any{
				"type":        "integer",
				"description": "The numeric answer",
			},
		},
	}

	inv := agent.AgentInvocation{
		NodePath:     "/test/bucket14a",
		Uses:         env.Adapter.Ref(),
		With:         ir.RawConfig{"prompt": "What is 2+2? Answer with a JSON object containing the integer answer."},
		OutputSchema: &schema,
	}

	events, outcomeCh, err := env.Adapter.Launch(ctx, h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range events {
	}
	oc, ok := <-outcomeCh
	if !ok {
		t.Fatal("outcome channel closed without emitting")
	}
	if _, more := <-outcomeCh; more {
		t.Error("outcome channel emitted a second value; want exactly one")
	}
	if oc.Err != nil {
		t.Fatalf("Launch outcome err: %v", oc.Err)
	}

	if oc.Result.Output == nil {
		t.Fatalf("Output is nil; want a map with 'answer' key")
	}
	got, ok := oc.Result.Output["answer"]
	if !ok {
		t.Fatalf("Output missing 'answer' key: %+v", oc.Result.Output)
	}
	// encoding/json decodes JSON numbers to float64 — no int case.
	v, ok := got.(float64)
	if !ok {
		t.Fatalf("answer typed as %T (value %v); want float64", got, got)
	}
	if v != 4 {
		t.Errorf("answer = %v; want 4", v)
	}
}
