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
		Metrics: &agent.MetricSet{Cost: agent.MetricCost{Total: 0.04, Source: "reported"}, Tokens: agent.MetricTokens{Input: 100, Output: 50}, Turns: 2},
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
