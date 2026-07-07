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

// TestRunWithRetry_LiveReplayRecoveryPolicy (R3) locks the policy-driven retry
// loop disposition of agent.ErrLiveReplayRequired: under recovery:continue a
// replay-required from one attempt is DEMOTED to a retryable_failure so the loop
// runs the next attempt (where a PersistentSession adapter resumes its session,
// R5) instead of hard-halting for a cross-process resume; under recovery:restart
// it REMAINS a hard halt.
//
// A generic PersistentSession fake raises agent.ErrLiveReplayRequired on attempt
// 1 and succeeds on attempt 2, so the loop's branch is exercised without the
// codexlive-internal ActiveTurn dance.
func TestRunWithRetry_LiveReplayRecoveryPolicy(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, recovery string) (engine.DispatchResult, int, error) {
		t.Helper()
		fk := agentfake.New("openai/codex-live").
			WithCaps(agent.Caps{Containerless: true, PersistentSession: true}).
			Script(0, agentfake.Result{Err: agent.ErrLiveReplayRequired}).
			Script(1, agentfake.Result{Output: map[string]any{"ok": true}})
		var reg agent.Registry
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register: %v", err)
		}
		d := &engine.LocalDispatcher{Resolver: &reg, Handles: map[string]container.Handle{}}
		intent := engine.NodeIntent{
			Path:           "ask",
			Node:           &ir.AgentStep{ID: "ask", Uses: "openai/codex-live", Container: ""},
			ResolvedInputs: engine.ResolvedInputs{Uses: "openai/codex-live", With: ir.RawConfig{"prompt": "hi"}},
		}
		log := state.NewInMemoryLog(clock.System{})
		clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		policy := retry.Policy{Attempts: 3, Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second, Recovery: recovery}
		dr, ch, err := engine.RunWithRetry(context.Background(), d, intent, policy, clk, log)
		if ch != nil {
			for range ch {
			}
		}
		return dr, len(fk.Calls()), err
	}

	t.Run("continue retries to success (no hard halt)", func(t *testing.T) {
		dr, calls, err := run(t, "continue")
		if err != nil {
			t.Fatalf("err = %v, want nil (replay-required demoted, retried to success)", err)
		}
		if dr.Outcome != engine.OutcomeOK {
			t.Fatalf("Outcome = %v, want ok", dr.Outcome)
		}
		if calls != 2 {
			t.Fatalf("adapter calls = %d, want 2 (attempt 1 replay-required, attempt 2 ok)", calls)
		}
	})

	t.Run("restart hard-halts on replay-required", func(t *testing.T) {
		_, calls, err := run(t, "restart")
		if !errors.Is(err, agent.ErrLiveReplayRequired) {
			t.Fatalf("err = %v, want ErrLiveReplayRequired hard-halt", err)
		}
		if calls != 1 {
			t.Fatalf("adapter calls = %d, want 1 (hard-halt on attempt 1)", calls)
		}
	})
}
