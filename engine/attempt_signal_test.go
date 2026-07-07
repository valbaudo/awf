package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	agentfake "github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/retry"
	"github.com/valbaudo/awf/state"
)

// TestRunWithRetry_AttemptSignalAdvances verifies the per-attempt Attempt
// counter threads from NodeIntent through the dispatcher into the
// AgentInvocation and advances across retries: attempt 1 fails retryably,
// attempt 2 succeeds, and the attempt-2 invocation carries a strictly greater
// Attempt than the attempt-1 invocation.
func TestRunWithRetry_AttemptSignalAdvances(t *testing.T) {
	t.Parallel()

	fk := agentfake.New("awf/llm").WithCaps(agent.Caps{Containerless: true}).
		Script(0, agentfake.Result{Err: &agent.ErrAgentLaunch{Cause: errors.New("transient 503")}}).
		Script(1, agentfake.Result{Output: map[string]any{"ok": true}})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d := &engine.LocalDispatcher{
		Resolver: &reg,
		Handles:  map[string]container.Handle{},
	}
	intent := engine.NodeIntent{
		Path:           "ask",
		Node:           &ir.AgentStep{ID: "ask", Uses: "awf/llm", Container: ""},
		ResolvedInputs: engine.ResolvedInputs{Uses: "awf/llm", With: ir.RawConfig{"model": "m", "prompt": "hi"}},
	}
	log := state.NewInMemoryLog(clock.System{})
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	policy := retry.Policy{Attempts: 3, Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}

	dr, ch, err := engine.RunWithRetry(context.Background(), d, intent, policy, clk, log)
	for range ch {
	}
	if err != nil {
		t.Fatalf("RunWithRetry: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", dr.Outcome)
	}

	calls := fk.Calls()
	if len(calls) != 2 {
		t.Fatalf("adapter Launch called %d times, want 2", len(calls))
	}
	if calls[1].Attempt <= calls[0].Attempt {
		t.Errorf("attempt-2 Attempt = %d, want strictly greater than attempt-1 Attempt = %d",
			calls[1].Attempt, calls[0].Attempt)
	}
}
