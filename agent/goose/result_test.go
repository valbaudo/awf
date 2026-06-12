package goose_test

import (
	"math"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/goose"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/pricing"
)

// gooseFixtureTable is a known, self-contained rate table so these tests never
// depend on the embedded rates.json. goose reports no cache fields, so only
// input/output rates matter.
func gooseFixtureTable() pricing.Table {
	return pricing.Table{"claude-sonnet": {Currency: "USD", InputPerM: 3, OutputPerM: 15}}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// goose has no cache subset, so Breakdown.Input == tokens.Input directly: 1.0M ×
// $3/M = $3 input, 1.0M × $15/M = $15 output, Total = exactly $18.
func TestBuildResult_DerivesCost_RequestedModel_NoCache(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]", With: ir.RawConfig{"model": "claude-sonnet"}}
	complete := goose.NewCompleteForTest(1_000_000, 1_000_000)
	res, err := goose.BuildResultForTest("free text", complete, inv, gooseFixtureTable())
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Metrics.Model != "claude-sonnet" {
		t.Errorf("Model = %q, want claude-sonnet", res.Metrics.Model)
	}
	c := res.Metrics.Cost
	if c.Source != agent.CostSourceDerived {
		t.Errorf("Cost.Source = %q, want %q", c.Source, agent.CostSourceDerived)
	}
	if c.Currency != "USD" {
		t.Errorf("Cost.Currency = %q, want USD", c.Currency)
	}
	if !approx(c.Input, 3.0) {
		t.Errorf("Cost.Input = %v, want 3.0", c.Input)
	}
	if !approx(c.Output, 15.0) {
		t.Errorf("Cost.Output = %v, want 15.0", c.Output)
	}
	if !approx(c.Total, 18.0) {
		t.Errorf("Cost.Total = %v, want 18.0", c.Total)
	}
	if c.Total != c.Input+c.Output {
		t.Errorf("Total %v != Input+Output %v (exact invariant)", c.Total, c.Input+c.Output)
	}
}

// A model the fixture table does not know → cost ABSENT (Source==""), never $0.
func TestBuildResult_ModelMiss_CostAbsent(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]", With: ir.RawConfig{"model": "unknown-xyz"}}
	complete := goose.NewCompleteForTest(1_000_000, 1_000_000)
	res, err := goose.BuildResultForTest("free text", complete, inv, gooseFixtureTable())
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

// No with:{model} → cost ABSENT (correct; goose's harness emits no model id).
func TestBuildResult_UnsetModel_CostAbsent(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]"} // no With
	complete := goose.NewCompleteForTest(1_000_000, 1_000_000)
	res, err := goose.BuildResultForTest("free text", complete, inv, gooseFixtureTable())
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
