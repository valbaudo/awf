package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func TestPrintRunCostSummary(t *testing.T) {
	dir := t.TempDir()
	lg, err := state.OpenLog(filepath.Join(dir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }

	// one agent step with metrics + one code step without.
	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "a1", Data: d(engine.NodeCompletedData{
		Outcome: "ok",
		Usage:   &agent.MetricSet{Cost: agent.MetricCost{Total: 0.04, Source: "reported"}, Tokens: agent.MetricTokens{Input: 100, Output: 50}, Turns: 2},
	})})
	exit0 := 0
	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "c1", Data: d(engine.NodeCompletedData{Outcome: "ok", ExitCode: &exit0})})

	var out bytes.Buffer
	printRunCostSummary(&out, lg)
	got := out.String()
	if !strings.Contains(got, "0.0400") || !strings.Contains(got, "150 tok") || !strings.Contains(got, "1 agent") {
		t.Errorf("summary wrong:\n%s", got)
	}
}

// TestPrintRunCostSummaryDerived asserts that a run whose cost comes from a
// derived adapter surfaces the input/output dollar split in the summary line.
func TestPrintRunCostSummaryDerived(t *testing.T) {
	dir := t.TempDir()
	lg, err := state.OpenLog(filepath.Join(dir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }

	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "a1", Data: d(engine.NodeCompletedData{
		Outcome: "ok",
		Usage: &agent.MetricSet{
			Cost:   agent.MetricCost{Source: agent.CostSourceDerived, Total: 1.2, Input: 0.3, Output: 0.9},
			Tokens: agent.MetricTokens{Input: 200, Output: 100},
			Turns:  3,
		},
	})})

	var out bytes.Buffer
	printRunCostSummary(&out, lg)
	got := out.String()

	if !strings.Contains(got, "in $0.3000") {
		t.Errorf("derived summary missing input split, got:\n%s", got)
	}
	if !strings.Contains(got, "out $0.9000") {
		t.Errorf("derived summary missing output split, got:\n%s", got)
	}
	if !strings.Contains(got, "$1.2000") {
		t.Errorf("derived summary missing total, got:\n%s", got)
	}
}

// TestPrintRunCostSummaryReportedNoSplit asserts that a reported-only run does
// NOT show a dollar split (in $/out $) in the summary line.
func TestPrintRunCostSummaryReportedNoSplit(t *testing.T) {
	dir := t.TempDir()
	lg, err := state.OpenLog(filepath.Join(dir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }

	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "a1", Data: d(engine.NodeCompletedData{
		Outcome: "ok",
		Usage: &agent.MetricSet{
			Cost:   agent.MetricCost{Source: agent.CostSourceReported, Total: 1.2},
			Tokens: agent.MetricTokens{Input: 200, Output: 100},
			Turns:  3,
		},
	})})

	var out bytes.Buffer
	printRunCostSummary(&out, lg)
	got := out.String()

	if strings.Contains(got, "in $") {
		t.Errorf("reported-only summary must NOT show input dollar split, got:\n%s", got)
	}
	if strings.Contains(got, "out $") {
		t.Errorf("reported-only summary must NOT show output dollar split, got:\n%s", got)
	}
	if !strings.Contains(got, "$1.2000") {
		t.Errorf("reported-only summary missing total, got:\n%s", got)
	}
}

func TestPrintRunCostSummaryNoAgentSteps(t *testing.T) {
	dir := t.TempDir()
	lg, _ := state.OpenLog(filepath.Join(dir, "log"), clock.System{})
	t.Cleanup(func() { _ = lg.Close() })
	exit0 := 0
	b, _ := json.Marshal(engine.NodeCompletedData{Outcome: "ok", ExitCode: &exit0})
	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "c1", Data: b})

	var out bytes.Buffer
	printRunCostSummary(&out, lg)
	if out.Len() != 0 {
		t.Errorf("code-step-only run must print no cost summary, got:\n%s", out.String())
	}
}

func TestPrintRunCostSummaryMixedReportedAndDerived(t *testing.T) {
	dir := t.TempDir()
	lg, err := state.OpenLog(filepath.Join(dir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }

	// Derived step: Total == Input + Output.
	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "a1", Data: d(engine.NodeCompletedData{
		Outcome: "ok",
		Usage: &agent.MetricSet{
			Cost: agent.MetricCost{Source: agent.CostSourceDerived, Total: 1.2, Input: 0.3, Output: 0.9},
		},
	})})
	// Reported step: Total only (Claude), Input/Output zero.
	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "a2", Data: d(engine.NodeCompletedData{
		Outcome: "ok",
		Usage: &agent.MetricSet{
			Cost: agent.MetricCost{Source: agent.CostSourceReported, Total: 0.5},
		},
	})})

	var out bytes.Buffer
	printRunCostSummary(&out, lg)
	got := out.String()

	if !strings.Contains(got, "$1.7000") {
		t.Errorf("mixed summary missing total $1.7000, got:\n%s", got)
	}
	if !strings.Contains(got, "in $0.3000 / out $0.9000 + reported $0.5000") {
		t.Errorf("mixed summary breakdown does not reconcile with total, got:\n%s", got)
	}
}

// TestPrintRunCostSummaryUnpricedMarker (F35) asserts that a run with some
// agent steps carrying no cost source at all (Cost.Source == "", e.g. droid
// pre-pricing) surfaces a "+" on the total and a "(N of M steps unpriced)"
// marker — so the total never reads as a complete `$X` when it silently
// undercounts.
func TestPrintRunCostSummaryUnpricedMarker(t *testing.T) {
	dir := t.TempDir()
	lg, err := state.OpenLog(filepath.Join(dir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }

	priced := agent.MetricSet{Cost: agent.MetricCost{Source: agent.CostSourceReported, Total: 0.5}, Tokens: agent.MetricTokens{Input: 10, Output: 10}, Turns: 1}
	unpriced := agent.MetricSet{Tokens: agent.MetricTokens{Input: 10, Output: 10}, Turns: 1} // no Cost.Source at all

	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "p1", Data: d(engine.NodeCompletedData{Outcome: "ok", Usage: &priced})})
	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "p2", Data: d(engine.NodeCompletedData{Outcome: "ok", Usage: &priced})})
	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "p3", Data: d(engine.NodeCompletedData{Outcome: "ok", Usage: &priced})})
	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "u1", Data: d(engine.NodeCompletedData{Outcome: "ok", Usage: &unpriced})})
	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "u2", Data: d(engine.NodeCompletedData{Outcome: "ok", Usage: &unpriced})})

	var out bytes.Buffer
	printRunCostSummary(&out, lg)
	got := out.String()

	if !strings.Contains(got, "$1.5000+") {
		t.Errorf("unpriced summary missing '+' on the total, got:\n%s", got)
	}
	if !strings.Contains(got, "2 of 5 steps unpriced") {
		t.Errorf("unpriced summary missing the unpriced-step marker, got:\n%s", got)
	}
}

// TestPrintRunCostSummaryFullyPricedNoMarker (F35) asserts the fully-priced
// case is unchanged: no trailing "+" and no "unpriced" marker.
func TestPrintRunCostSummaryFullyPricedNoMarker(t *testing.T) {
	dir := t.TempDir()
	lg, err := state.OpenLog(filepath.Join(dir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }

	priced := agent.MetricSet{Cost: agent.MetricCost{Source: agent.CostSourceReported, Total: 0.5}, Tokens: agent.MetricTokens{Input: 10, Output: 10}, Turns: 1}
	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "p1", Data: d(engine.NodeCompletedData{Outcome: "ok", Usage: &priced})})
	_ = lg.Append(state.Event{Type: engine.EventNodeCompleted, Path: "p2", Data: d(engine.NodeCompletedData{Outcome: "ok", Usage: &priced})})

	var out bytes.Buffer
	printRunCostSummary(&out, lg)
	got := out.String()

	if strings.Contains(got, "unpriced") {
		t.Errorf("fully-priced summary must NOT show the unpriced marker, got:\n%s", got)
	}
	if strings.Contains(got, "$1.0000+") {
		t.Errorf("fully-priced summary must NOT show a '+' on the total, got:\n%s", got)
	}
}
