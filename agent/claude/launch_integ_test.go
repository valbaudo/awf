//go:build integ

package claude_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
	"github.com/valbaudo/awf/ir"
)

// hostEnvAllowlist builds a map with the auth env vars currently set on
// the host. Returned to WithEnv at adapter construction.
func hostEnvAllowlist() map[string]string {
	out := map[string]string{}
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if v := os.Getenv(name); v != "" {
			out[name] = v
		}
	}
	return out
}

func newClaudeAdapterForInteg(t *testing.T) (*claude.Adapter, container.Handle, container.Backend) {
	t.Helper()
	be, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	h, err := be.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = be.Destroy(context.Background(), h) })
	a, err := claude.New(claude.WithBackend(be), claude.WithEnv(hostEnvAllowlist()))
	if err != nil {
		t.Fatalf("claude.New: %v", err)
	}
	return a, h, be
}

func TestClaudeAdapterSimpleSchemaOutput(t *testing.T) {
	skipIfNoClaude(t)
	skipIfNoAuthEnv(t)

	a, h, _ := newClaudeAdapterForInteg(t)

	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Uses:     claude.AdapterRef,
		With: ir.RawConfig{
			"prompt": "What is 2 + 2? Respond with the structured output tool.",
		},
		OutputSchema: &ir.JSONSchema{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"answer"},
			"properties": map[string]any{
				"answer": map[string]any{"type": "integer"},
			},
		},
	}
	// γ contract: drain events first, then read outcome.
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range eventCh {
	}
	outcome := <-outcomeCh
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	res := outcome.Result
	if res.Output["answer"] == nil {
		t.Fatalf("Output[answer] missing; full Output=%+v", res.Output)
	}
	if v, ok := res.Output["answer"].(float64); !ok || v != 4 {
		t.Errorf("Output[answer] = %v (%T), want 4", v, res.Output["answer"])
	}
	if res.Metrics.Cost.USD <= 0 {
		t.Errorf("Cost.USD = %v, want > 0 (claude reports total_cost_usd)", res.Metrics.Cost.USD)
	}
}

func TestClaudeAdapterSubscriptionAuthViaBareFalse(t *testing.T) {
	skipIfNoClaude(t)
	skipIfNoAuthEnv(t)

	a, h, _ := newClaudeAdapterForInteg(t)

	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Uses:     claude.AdapterRef,
		With: ir.RawConfig{
			"prompt": "Return ok=true.",
			"bare":   false, // opt out of bare mode — host config pollution acceptable
		},
		OutputSchema: &ir.JSONSchema{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"ok"},
			"properties": map[string]any{
				"ok": map[string]any{"type": "boolean"},
			},
		},
	}
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v (bare:false should accept subscription auth path)", err)
	}
	for range eventCh {
	}
	outcome := <-outcomeCh
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if v, ok := outcome.Result.Output["ok"].(bool); !ok || !v {
		t.Errorf("Output[ok] = %v, want true", outcome.Result.Output["ok"])
	}
}

func TestClaudeAdapterRealtimeStreaming(t *testing.T) {
	skipIfNoClaude(t)
	skipIfNoAuthEnv(t)

	a, h, _ := newClaudeAdapterForInteg(t)

	// Multi-output prompt to maximize multi-event emission.
	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Uses:     claude.AdapterRef,
		With: ir.RawConfig{
			"prompt": "List the first 5 prime numbers; for each, briefly explain in one sentence why it is prime. Take your time and think carefully.",
		},
		OutputSchema: &ir.JSONSchema{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"primes", "explanations"},
			"properties": map[string]any{
				"primes":       map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
				"explanations": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
		},
	}
	start := time.Now()
	// γ contract: Launch returns IMMEDIATELY with events + outcome channels open.
	// Wall-clock measurements taken inside the range loop reflect REAL event
	// arrival times — the realtime UX is end-to-end verifiable.
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	var arrivals []time.Duration
	for range eventCh {
		arrivals = append(arrivals, time.Since(start))
	}
	outcome := <-outcomeCh
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	res := outcome.Result

	if len(arrivals) < 3 {
		t.Fatalf("got %d events; want >= 3 (proves multi-block stream emission)", len(arrivals))
	}
	if arrivals[0] > 5*time.Second {
		t.Errorf("first event at %v; want < 5s (proves Launch returns events promptly)", arrivals[0])
	}
	span := arrivals[len(arrivals)-1] - arrivals[0]
	if span < time.Second {
		t.Errorf("first→last span = %v; want >= 1s (proves progressive emission, not batched at end)", span)
	}
	primes, ok := res.Output["primes"].([]any)
	if !ok {
		t.Fatalf("Output[primes] missing or wrong type: %+v", res.Output)
	}
	if len(primes) != 5 {
		t.Errorf("len(primes) = %d, want 5", len(primes))
	}
}
