package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/obs"
	"github.com/valbaudo/awf/state"
)

func traceFixture(t *testing.T, stateDir string) {
	t.Helper()
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }
	exit0 := 0
	writeRunLog(t, stateDir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: d(engine.RunStartedData{RunID: "r1", WorkflowID: "wf"})},
		state.Event{Type: engine.EventNodeStarted, Path: "s1", Data: d(engine.NodeStartedData{Kind: "code"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "s1", Data: d(engine.NodeCompletedData{Outcome: "ok", ExitCode: &exit0})},
		state.Event{Type: engine.EventRunFinished, Data: d(engine.RunFinishedData{Outcome: "ok"})},
	)
}

func TestTraceOutputJSON(t *testing.T) {
	stateDir := t.TempDir()
	traceFixture(t, stateDir)
	var out, errb bytes.Buffer
	if rc := cliTrace([]string{"r1", "--state-dir", stateDir, "--output", "json"}, &out, &errb); rc != ExitOK {
		t.Fatalf("trace rc = %d, stderr: %s", rc, errb.String())
	}
	var spans []obs.Span
	if err := json.Unmarshal(out.Bytes(), &spans); err != nil {
		t.Fatalf("trace json: %v\n%s", err, out.String())
	}
	if len(spans) == 0 {
		t.Fatal("trace --output json produced no spans")
	}
}

func TestTraceStdoutExporter(t *testing.T) {
	stateDir := t.TempDir()
	traceFixture(t, stateDir)
	var out, errb bytes.Buffer
	if rc := cliTrace([]string{"r1", "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("trace rc = %d, stderr: %s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "awf.node.path") {
		t.Errorf("trace stdout exporter missing span output:\n%s", out.String())
	}
}

func TestTraceNoSuchRun(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := cliTrace([]string{"ghost", "--state-dir", t.TempDir()}, &out, &errb); rc != ExitUsage {
		t.Fatalf("trace missing-run rc = %d, want ExitUsage", rc)
	}
}
