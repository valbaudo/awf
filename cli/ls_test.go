package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// writeRunLog crafts <stateDir>/runs/<id>/log with the given events and returns
// the run dir. Shared by ls_test / inspect_test / trace_test (all package cli).
func writeRunLog(t *testing.T, stateDir, id string, events ...state.Event) string {
	t.Helper()
	runDir := filepath.Join(stateDir, "runs", id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	lg, err := state.OpenLog(filepath.Join(runDir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	for _, e := range events {
		if err := lg.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := lg.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return runDir
}

func TestLSDerivesStatuses(t *testing.T) {
	stateDir := t.TempDir()
	mustData := func(v any) []byte { b, _ := json.Marshal(v); return b }

	writeRunLog(t, stateDir, "fin",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "fin"})},
		state.Event{Type: engine.EventRunFinished, Data: mustData(engine.RunFinishedData{Outcome: "ok"})},
	)
	writeRunLog(t, stateDir, "fail",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "fail"})},
		state.Event{Type: engine.EventRunFinished, Data: mustData(engine.RunFinishedData{Outcome: "permanent_failure"})},
	)
	// "crash" = started, no terminal, no live lock holder.
	writeRunLog(t, stateDir, "crash",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "crash"})},
		state.Event{Type: engine.EventNodeStarted, Path: "s1", Data: mustData(engine.NodeStartedData{Kind: "agent"})},
	)
	// "live" = same log shape, but we hold the lock for the duration of ls.
	liveDir := writeRunLog(t, stateDir, "live",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "live"})},
		state.Event{Type: engine.EventNodeStarted, Path: "s1", Data: mustData(engine.NodeStartedData{Kind: "agent"})},
	)
	lock, err := acquireRunLock(liveDir)
	if err != nil {
		t.Fatalf("acquireRunLock: %v", err)
	}
	defer lock.Release()

	var out, errb bytes.Buffer
	rc := cliLS([]string{"--state-dir", stateDir, "--output", "json"}, &out, &errb)
	if rc != ExitOK {
		t.Fatalf("ls rc = %d, stderr: %s", rc, errb.String())
	}

	var got []lsRow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal ls json: %v\n%s", err, out.String())
	}
	want := map[string]string{"fin": "finished", "fail": "failed", "crash": "crashed", "live": "running"}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %s", len(got), len(want), out.String())
	}
	for _, r := range got {
		if want[r.RunID] != r.Status {
			t.Errorf("run %q status = %q, want %q", r.RunID, r.Status, want[r.RunID])
		}
	}
}

func TestLSReportsCostAndTokens(t *testing.T) {
	stateDir := t.TempDir()
	mustData := func(v any) []byte { b, _ := json.Marshal(v); return b }
	writeRunLog(t, stateDir, "metered",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "metered", WorkflowID: "wf"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "s1", Data: mustData(engine.NodeCompletedData{
			Outcome: "ok",
			Metrics: &agent.MetricSet{
				Cost:   agent.MetricCost{USD: 0.0123, Source: agent.CostSourceReported},
				Tokens: agent.MetricTokens{Input: 1000, Output: 200},
			},
		})},
		// a second agent step, to prove per-run aggregation sums them
		state.Event{Type: engine.EventNodeCompleted, Path: "s2", Data: mustData(engine.NodeCompletedData{
			Outcome: "ok",
			Metrics: &agent.MetricSet{
				Cost:   agent.MetricCost{USD: 0.0077, Source: agent.CostSourceReported},
				Tokens: agent.MetricTokens{Input: 200, Output: 140},
			},
		})},
		state.Event{Type: engine.EventRunFinished, Data: mustData(engine.RunFinishedData{Outcome: "ok"})},
	)
	var out, errb bytes.Buffer
	if rc := cliLS([]string{"--state-dir", stateDir, "--output", "json"}, &out, &errb); rc != ExitOK {
		t.Fatalf("ls rc = %d, stderr: %s", rc, errb.String())
	}
	var got []lsRow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal ls json: %v\n%s", err, out.String())
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1: %s", len(got), out.String())
	}
	r := got[0]
	if r.CostUSD == nil || *r.CostUSD < 0.0199 || *r.CostUSD > 0.0201 {
		t.Errorf("cost_usd = %v, want ~0.0200 (0.0123+0.0077)", r.CostUSD)
	}
	if r.InputTokens == nil || *r.InputTokens != 1200 {
		t.Errorf("input_tokens = %v, want 1200 (1000+200)", r.InputTokens)
	}
	if r.OutputTokens == nil || *r.OutputTokens != 340 {
		t.Errorf("output_tokens = %v, want 340 (200+140)", r.OutputTokens)
	}
}

func TestLSOmitsCostWhenUnreportedButKeepsTokens(t *testing.T) {
	stateDir := t.TempDir()
	mustData := func(v any) []byte { b, _ := json.Marshal(v); return b }
	// unpriced: an agent step that reports tokens but no cost source (e.g. codex,
	// goose). Tokens are real; a $0.0000 would be a misleading "free", so cost
	// must be absent, not zero.
	writeRunLog(t, stateDir, "unpriced",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "unpriced"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "s1", Data: mustData(engine.NodeCompletedData{
			Outcome: "ok",
			Metrics: &agent.MetricSet{Tokens: agent.MetricTokens{Input: 500, Output: 90}},
		})},
		state.Event{Type: engine.EventRunFinished, Data: mustData(engine.RunFinishedData{Outcome: "ok"})},
	)
	// codeonly: no agent steps at all → neither cost nor tokens.
	writeRunLog(t, stateDir, "codeonly",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "codeonly"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "s1", Data: mustData(engine.NodeCompletedData{Outcome: "ok"})},
		state.Event{Type: engine.EventRunFinished, Data: mustData(engine.RunFinishedData{Outcome: "ok"})},
	)
	var out, errb bytes.Buffer
	if rc := cliLS([]string{"--state-dir", stateDir, "--output", "json"}, &out, &errb); rc != ExitOK {
		t.Fatalf("ls rc = %d, stderr: %s", rc, errb.String())
	}
	var got []lsRow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal ls json: %v\n%s", err, out.String())
	}
	rows := map[string]lsRow{}
	for _, r := range got {
		rows[r.RunID] = r
	}
	if up := rows["unpriced"]; up.CostUSD != nil {
		t.Errorf("unpriced cost_usd = %v, want nil (no cost source reported)", *up.CostUSD)
	}
	if up := rows["unpriced"]; up.InputTokens == nil || *up.InputTokens != 500 {
		t.Errorf("unpriced input_tokens = %v, want 500", up.InputTokens)
	}
	if co := rows["codeonly"]; co.CostUSD != nil || co.InputTokens != nil || co.OutputTokens != nil {
		t.Errorf("codeonly metrics = %+v, want all nil", co)
	}
}

func TestLSTextOutput(t *testing.T) {
	stateDir := t.TempDir()
	mustData := func(v any) []byte { b, _ := json.Marshal(v); return b }
	writeRunLog(t, stateDir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "r1", WorkflowID: "wf"})},
		state.Event{Type: engine.EventRunFinished, Data: mustData(engine.RunFinishedData{Outcome: "ok"})},
	)
	var out, errb bytes.Buffer
	if rc := cliLS([]string{"--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("ls rc = %d", rc)
	}
	if !strings.Contains(out.String(), "r1") || !strings.Contains(out.String(), "finished") {
		t.Errorf("ls text missing run/status:\n%s", out.String())
	}
}
