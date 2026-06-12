package codex_test

import (
	"math"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/codex"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/pricing"
)

// approx compares two USD figures tolerant of IEEE-754 noise. The per-token rate
// arithmetic (tokens/1e6*rate, in the pricing pkg) makes 1.6+0.1 land at
// 1.7000000000000002, so an exact literal compare on a derived sub-value is
// brittle; the load-bearing exactness (Total == Input+Output) is checked
// separately with ==, since both sides are the identical float.
func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// fixtureTable is a known, self-contained rate table so these tests never depend
// on the embedded rates.json. gpt-5-codex: input $2/M, output $10/M, cache-read
// $0.50/M (no cache-write).
func fixtureTable() pricing.Table {
	return pricing.Table{"gpt-5-codex": {Currency: "USD", InputPerM: 2, OutputPerM: 10, CacheReadPerM: 0.5}}
}

// codex's input_tokens INCLUDES cached, so buildResult must subtract the cached
// subset before pricing: uncached = 1.0M − 0.2M = 0.8M priced at $2/M = $1.6,
// cached 0.2M priced at the cache-read $0.5/M = $0.1 → Input $1.7. Output 1.0M ×
// $10/M = $10.0. Total = exactly $11.7.
func TestBuildResult_DerivesCost_RequestedModel_CacheNormalized(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]", With: ir.RawConfig{"model": "gpt-5-codex"}}
	usage := codex.NewUsageForTest(1_000_000, 200_000, 1_000_000) // input(incl cached), cached, output
	res, err := codex.BuildResultForTest("free text", usage, inv, fixtureTable())
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Metrics.Model != "gpt-5-codex" {
		t.Errorf("Model = %q, want gpt-5-codex", res.Metrics.Model)
	}
	c := res.Metrics.Cost
	if c.Source != agent.CostSourceDerived {
		t.Errorf("Cost.Source = %q, want %q", c.Source, agent.CostSourceDerived)
	}
	if c.Currency != "USD" {
		t.Errorf("Cost.Currency = %q, want USD", c.Currency)
	}
	if !approx(c.Input, 1.7) {
		t.Errorf("Cost.Input = %v, want 1.7 (0.8M·$2 + 0.2M·$0.5)", c.Input)
	}
	if !approx(c.Output, 10.0) {
		t.Errorf("Cost.Output = %v, want 10.0", c.Output)
	}
	if !approx(c.Total, 11.7) {
		t.Errorf("Cost.Total = %v, want 11.7", c.Total)
	}
	if c.Total != c.Input+c.Output {
		t.Errorf("Total %v != Input+Output %v (exact invariant)", c.Total, c.Input+c.Output)
	}
}

// A model the fixture table does not know → cost ABSENT (Source==""), never $0.
func TestBuildResult_ModelMiss_CostAbsent(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]", With: ir.RawConfig{"model": "unknown-xyz"}}
	usage := codex.NewUsageForTest(1_000_000, 0, 1_000_000)
	res, err := codex.BuildResultForTest("free text", usage, inv, fixtureTable())
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Metrics.Model != "unknown-xyz" {
		t.Errorf("Model = %q, want unknown-xyz (still recorded)", res.Metrics.Model)
	}
	if res.Metrics.Cost.Source != "" {
		t.Errorf("Cost.Source = %q, want \"\" (absent on miss)", res.Metrics.Cost.Source)
	}
	if res.Metrics.Cost.Total != 0 {
		t.Errorf("Cost.Total = %v, want 0 (absent, not $0)", res.Metrics.Cost.Total)
	}
}

// No with:{model} at all → cost ABSENT (correct, not a bug — the harness emits no
// model id).
func TestBuildResult_UnsetModel_CostAbsent(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]"} // no With
	usage := codex.NewUsageForTest(1_000_000, 0, 1_000_000)
	res, err := codex.BuildResultForTest("free text", usage, inv, fixtureTable())
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Metrics.Model != "" {
		t.Errorf("Model = %q, want \"\" (unset)", res.Metrics.Model)
	}
	if res.Metrics.Cost.Source != "" {
		t.Errorf("Cost.Source = %q, want \"\" (absent when model unset)", res.Metrics.Cost.Source)
	}
}
