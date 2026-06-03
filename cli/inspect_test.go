package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/obs"
	"github.com/valbaudo/awf/state"
)

func TestInspectTextTreeFoldByStatus(t *testing.T) {
	stateDir := t.TempDir()
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }
	exit0 := 0
	writeRunLog(t, stateDir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: d(engine.RunStartedData{RunID: "r1", WorkflowID: "wf"})},
		state.Event{Type: engine.EventNodeStarted, Path: "ok1", Data: d(engine.NodeStartedData{Kind: "code"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "ok1", Data: d(engine.NodeCompletedData{Outcome: "ok", ExitCode: &exit0})},
		state.Event{Type: engine.EventNodeStarted, Path: "bad1", Data: d(engine.NodeStartedData{Kind: "code"})},
		state.Event{Type: engine.EventNodeFailed, Path: "bad1", Data: d(engine.NodeFailedData{Outcome: "permanent_failure", Error: "boom"})},
		state.Event{Type: engine.EventRunFinished, Data: d(engine.RunFinishedData{Outcome: "permanent_failure"})},
	)

	var out, errb bytes.Buffer
	if rc := cliInspect([]string{"r1", "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("inspect rc = %d, stderr: %s", rc, errb.String())
	}
	text := out.String()
	for _, needle := range []string{"r1", "ok1", "bad1", "failed", "permanent_failure"} {
		if !strings.Contains(text, needle) {
			t.Errorf("inspect text missing %q:\n%s", needle, text)
		}
	}
}

func TestInspectJSONOutput(t *testing.T) {
	stateDir := t.TempDir()
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }
	exit0 := 0
	writeRunLog(t, stateDir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: d(engine.RunStartedData{RunID: "r1"})},
		state.Event{Type: engine.EventNodeStarted, Path: "s1", Data: d(engine.NodeStartedData{Kind: "code"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "s1", Data: d(engine.NodeCompletedData{Outcome: "ok", ExitCode: &exit0})},
		state.Event{Type: engine.EventRunFinished, Data: d(engine.RunFinishedData{Outcome: "ok"})},
	)
	var out, errb bytes.Buffer
	if rc := cliInspect([]string{"r1", "--state-dir", stateDir, "--output", "json"}, &out, &errb); rc != ExitOK {
		t.Fatalf("inspect rc = %d", rc)
	}
	var spans []obs.Span
	if err := json.Unmarshal(out.Bytes(), &spans); err != nil {
		t.Fatalf("inspect json: %v\n%s", err, out.String())
	}
	if len(spans) == 0 {
		t.Fatal("inspect --output json produced no spans")
	}
}

func TestInspectNoSuchRun(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := cliInspect([]string{"ghost", "--state-dir", t.TempDir()}, &out, &errb); rc != ExitUsage {
		t.Fatalf("inspect missing-run rc = %d, want ExitUsage", rc)
	}
}

// Task 4.1 — pending agent span with tool-call events shows elapsed + tool-call count.
func TestInspectPendingAgentToolCalls(t *testing.T) {
	stateDir := t.TempDir()
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }
	writeRunLog(t, stateDir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: d(engine.RunStartedData{RunID: "r1", WorkflowID: "wf"})},
		state.Event{Type: engine.EventNodeStarted, Path: "scan", Data: d(engine.NodeStartedData{Kind: "agent"})},
		state.Event{Type: engine.EventAgentEvent, Path: "scan", Data: d(engine.AgentEventData{Kind: "tool_use", Size: 10})},
		state.Event{Type: engine.EventAgentEvent, Path: "scan", Data: d(engine.AgentEventData{Kind: "tool_use", Size: 10})},
		// no terminal event → pending agent span
	)

	var out, errb bytes.Buffer
	if rc := cliInspect([]string{"r1", "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("inspect rc = %d, stderr: %s", rc, errb.String())
	}
	text := out.String()
	if !strings.Contains(text, "tool call") {
		t.Errorf("inspect text missing 'tool call'; got:\n%s", text)
	}
	// The pending-agent suffix also renders the elapsed duration (e.g. "0s)").
	if !strings.Contains(text, "s)") {
		t.Errorf("inspect text missing elapsed duration suffix (expected 's)'); got:\n%s", text)
	}
}

// TestInspectPendingAgentNoToolCallsNoSuffix — a pending agent span with zero
// agent.event entries must NOT render a tool-call count suffix.
func TestInspectPendingAgentNoToolCallsNoSuffix(t *testing.T) {
	stateDir := t.TempDir()
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }
	writeRunLog(t, stateDir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: d(engine.RunStartedData{RunID: "r1", WorkflowID: "wf"})},
		state.Event{Type: engine.EventNodeStarted, Path: "scan", Data: d(engine.NodeStartedData{Kind: "agent"})},
		// no agent.event entries, no terminal event → pending agent span with zero tool calls
	)

	var out, errb bytes.Buffer
	if rc := cliInspect([]string{"r1", "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("inspect rc = %d, stderr: %s", rc, errb.String())
	}
	text := out.String()
	if strings.Contains(text, "tool call") {
		t.Errorf("inspect text must not contain 'tool call' for zero tool-call span; got:\n%s", text)
	}
}

// Task 4.2 — --tokens shows per-step input/output token counts.
func TestInspectTokensFlag(t *testing.T) {
	stateDir := t.TempDir()
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }
	writeRunLog(t, stateDir, "gen-run",
		state.Event{Type: engine.EventRunStarted, Data: d(engine.RunStartedData{RunID: "gen-run", WorkflowID: "wf"})},
		state.Event{Type: engine.EventNodeStarted, Path: "gen", Data: d(engine.NodeStartedData{Kind: "agent"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "gen", Data: d(engine.NodeCompletedData{
			Outcome: "ok",
			Metrics: &agent.MetricSet{
				Cost:   agent.MetricCost{USD: 0.01, Source: agent.CostSourceReported},
				Tokens: agent.MetricTokens{Input: 45000, Output: 8000},
				Turns:  2,
			},
		})},
		state.Event{Type: engine.EventRunFinished, Data: d(engine.RunFinishedData{Outcome: "ok"})},
	)

	var out, errb bytes.Buffer
	if rc := cliInspect([]string{"gen-run", "--state-dir", stateDir, "--tokens"}, &out, &errb); rc != ExitOK {
		t.Fatalf("inspect rc = %d, stderr: %s", rc, errb.String())
	}
	text := out.String()
	if !strings.Contains(text, "45000 in") {
		t.Errorf("inspect --tokens missing '45000 in'; got:\n%s", text)
	}
	if !strings.Contains(text, "8000 out") {
		t.Errorf("inspect --tokens missing '8000 out'; got:\n%s", text)
	}
}
