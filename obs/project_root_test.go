package obs

import (
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func TestProjectRootSpanAndRollup(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	mk := func(usd float64) engine.NodeCompletedData {
		return engine.NodeCompletedData{Outcome: "ok", Metrics: &agent.MetricSet{Cost: agent.MetricCost{Total: usd, Source: agent.CostSourceReported}}}
	}
	events := []state.Event{
		ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r1", WorkflowID: "cve", WorkflowVersion: 2, WorkflowDigest: "awf-d1:sha256:zz"}),
		// map with two item agent steps, each $0.01 — rollup must be $0.02 (no double-count from the synthesized map[0]/item scopes).
		ev(t, engine.EventNodeStarted, "map[0].item-0.scan", t0.Add(1*time.Second), engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventNodeCompleted, "map[0].item-0.scan", t0.Add(2*time.Second), mk(0.01)),
		ev(t, engine.EventNodeStarted, "map[0].item-1.scan", t0.Add(1*time.Second), engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventNodeCompleted, "map[0].item-1.scan", t0.Add(2*time.Second), mk(0.01)),
		ev(t, engine.EventRunFinished, "", t0.Add(3*time.Second), engine.RunFinishedData{Outcome: "ok"}),
	}
	spans, _ := Project(events, nil)
	root, ok := findSpan(spans, "")
	if !ok {
		t.Fatal("no run-root span")
	}
	if root.Kind != "run" {
		t.Errorf("root kind = %q, want run", root.Kind)
	}
	if root.Attributes[AttrWorkflowID] != "cve" || root.Attributes[AttrWorkflowVersion] != int64(2) {
		t.Errorf("workflow attrs wrong: %+v", root.Attributes)
	}
	if root.Attributes[AttrRunID] != "r1" || root.Attributes[AttrWorkflowDigest] != "awf-d1:sha256:zz" {
		t.Errorf("run/digest attrs wrong: %+v", root.Attributes)
	}
	if root.Attributes[AttrRunEpoch] != int64(1) {
		t.Errorf("run epoch = %v, want 1 (run.started ⇒ epoch 1)", root.Attributes[AttrRunEpoch])
	}
	gotCost, _ := root.Attributes[AttrRunCostUSD].(float64)
	if gotCost < 0.0199 || gotCost > 0.0201 {
		t.Errorf("run cost rollup = %v, want ~0.02 (no double-count)", root.Attributes[AttrRunCostUSD])
	}
	if root.Status != StatusUnset {
		t.Errorf("ok-run root Status = %v, want Unset (OTel Ok is never fabricated, R2)", root.Status)
	}
}

func TestProjectAgentConversationID(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "run-xyz"}),
		ev(t, engine.EventNodeStarted, "triage", t0.Add(1*time.Second), engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventNodeCompleted, "triage", t0.Add(2*time.Second), engine.NodeCompletedData{Outcome: "ok", Metrics: &agent.MetricSet{Cost: agent.MetricCost{Total: 0.01, Source: "reported"}}}),
	}
	spans, _ := Project(events, nil)
	s, _ := findSpan(spans, "triage")
	if s.Attributes[AttrGenAIConversation] != "run-xyz" || s.Attributes[AttrSessionID] != "run-xyz" {
		t.Errorf("agent span missing conversation/session id: %+v", s.Attributes)
	}
}

func TestProjectRootStatusFromOutcome(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r1"}),
		ev(t, engine.EventNodeStarted, "gate[0].attempt-1.generate.g", t0.Add(1*time.Second), engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventNodeFailed, "gate[0].attempt-1.generate.g", t0.Add(2*time.Second), engine.NodeFailedData{Outcome: "permanent_failure", Error: "refused"}),
		ev(t, engine.EventRunFinished, "", t0.Add(3*time.Second), engine.RunFinishedData{Outcome: "permanent_failure"}),
	}
	spans, _ := Project(events, nil)
	root, _ := findSpan(spans, "")
	if root.Status != StatusError {
		t.Errorf("failed-run root Status = %v, want Error (R2)", root.Status)
	}
	// The gate scope enclosing the failed leaf must NOT be marked Ok.
	gate, _ := findSpan(spans, "gate[0]")
	if gate.Status == StatusOK {
		t.Errorf("scope over a failed descendant must not be StatusOK; got %v", gate.Status)
	}
	// The failed leaf still carries the required awf.node.kind (R3).
	leaf, _ := findSpan(spans, "gate[0].attempt-1.generate.g")
	if leaf.Attributes[AttrNodeKind] != "agent" {
		t.Errorf("failed leaf awf.node.kind = %v, want agent (R3)", leaf.Attributes[AttrNodeKind])
	}
}

func TestSumLeafCostsUSDDeterministic(t *testing.T) {
	mk := func(usd float64) *Span { return &Span{Attributes: map[string]any{AttrCostUSD: usd}} }
	// Three non-representable decimals whose float sum is reorder-sensitive.
	byPath := map[string]*Span{"s1": mk(0.013), "s2": mk(0.027), "s3": mk(0.005)}
	got1, ok := sumLeafCostsUSD(byPath)
	got2, _ := sumLeafCostsUSD(byPath)
	if !ok || got1 != got2 {
		t.Fatalf("sumLeafCostsUSD not deterministic: %v vs %v", got1, got2)
	}
	if want := 0.013 + 0.027 + 0.005; got1 != want { // sorted-path order s1,s2,s3
		t.Errorf("sum = %v, want %v (sorted-path order)", got1, want)
	}
}

func TestSpanNamesAreLowCardinality(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	events := []state.Event{
		ev(t, engine.EventNodeStarted, "gate[0].attempt-37.generate.exploit", t0, engine.NodeStartedData{Kind: "agent"}),
		ev(t, engine.EventNodeCompleted, "gate[0].attempt-37.generate.exploit", t0.Add(time.Second), engine.NodeCompletedData{Outcome: "ok"}),
	}
	spans, _ := Project(events, nil)
	for _, s := range spans {
		// R8: a span NAME is a low-cardinality kind/id, never the indexed path
		// token (no "attempt-N"/"iter-N"/"item-N"/"[N]" in the name).
		for _, bad := range []string{"attempt-", "iter-", "item-", "["} {
			if strings.Contains(s.Name, bad) {
				t.Errorf("span %q has high-cardinality Name %q (contains %q)", s.Path, s.Name, bad)
			}
		}
	}
}
