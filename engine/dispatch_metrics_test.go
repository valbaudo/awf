package engine_test

import (
	"context"
	"testing"

	"github.com/valbaudo/awf/agent"
	agentfake "github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

func TestRunAgentCarriesMetrics(t *testing.T) {
	ctx := context.Background()
	cfake := container.NewFake()
	h, err := cfake.Create(ctx, container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fake := agentfake.New("test/agent").Script(0, agentfake.Result{
		Output: map[string]any{"ok": true},
		Cost:   0.0123,
		Tokens: agent.MetricTokens{Input: 100, Output: 50},
	})
	reg := &agent.Registry{}
	if err := reg.Register(fake); err != nil {
		t.Fatalf("Register: %v", err)
	}
	disp := &engine.LocalDispatcher{Backend: cfake, Handles: map[string]container.Handle{"lab": h}, Resolver: reg}
	intent := engine.NodeIntent{
		Path:           "a1",
		Node:           &ir.AgentStep{ID: "a1", Container: "lab", Uses: "test/agent"},
		ResolvedInputs: engine.ResolvedInputs{Uses: "test/agent"},
	}

	dr, _, err := disp.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want ok", dr.Outcome)
	}
	if dr.Metrics == nil {
		t.Fatal("DispatchResult.Metrics is nil; want the adapter's MetricSet")
	}
	if dr.Metrics.Cost.USD != 0.0123 || dr.Metrics.Cost.Source != agent.CostSourceReported {
		t.Errorf("Metrics.Cost = %+v, want {0.0123 reported}", dr.Metrics.Cost)
	}
	if dr.Metrics.Tokens.Input != 100 || dr.Metrics.Tokens.Output != 50 {
		t.Errorf("Metrics.Tokens = %+v, want {100 50}", dr.Metrics.Tokens)
	}
}
