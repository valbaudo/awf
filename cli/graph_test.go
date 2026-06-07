package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/graph"
	"github.com/valbaudo/awf/state"
)

const graphFixture = "testdata/render-probe.yaml"

// TestGraphStatic: no --run emits the static graph (exit 0), with the agent step's
// opaque with: passed through and no run_overlay.
func TestGraphStatic(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := cliGraph([]string{graphFixture}, &out, &errb); rc != ExitOK {
		t.Fatalf("graph rc = %d, stderr: %s", rc, errb.String())
	}
	var proj graph.Projection
	if err := json.Unmarshal(out.Bytes(), &proj); err != nil {
		t.Fatalf("decode projection: %v\n%s", err, out.String())
	}
	if proj.SchemaVersion != graph.SchemaVersion || proj.Workflow != "render-probe" {
		t.Errorf("bad header: %+v", proj)
	}
	if proj.RunOverlay != nil {
		t.Errorf("static graph must not carry run_overlay, got %+v", proj.RunOverlay)
	}
	var probe *graph.Node
	for i := range proj.Nodes {
		if proj.Nodes[i].Path == "probe" {
			probe = &proj.Nodes[i]
		}
	}
	if probe == nil {
		t.Fatalf("no 'probe' node in %+v", proj.Nodes)
	}
	if probe.Kind != "agent" || probe.ID != "probe" {
		t.Errorf("probe node = %+v, want kind agent id probe", *probe)
	}
	if probe.With["prompt"] != "go" {
		t.Errorf("with: not passed through opaque, got %+v", probe.With)
	}
}

// TestGraphWithRun: --run <id> attaches a flat run_overlay with per-path state.
func TestGraphWithRun(t *testing.T) {
	stateDir := t.TempDir()
	d := func(v any) []byte { b, _ := json.Marshal(v); return b }
	writeRunLog(t, stateDir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: d(engine.RunStartedData{RunID: "r1"})},
		state.Event{Type: engine.EventNodeStarted, Path: "probe", Data: d(engine.NodeStartedData{Kind: "agent"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "probe", Data: d(engine.NodeCompletedData{Outcome: "ok"})},
		state.Event{Type: engine.EventRunFinished, Data: d(engine.RunFinishedData{Outcome: "ok"})},
	)

	var out, errb bytes.Buffer
	if rc := cliGraph([]string{graphFixture, "--run", "r1", "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("graph --run rc = %d, stderr: %s", rc, errb.String())
	}
	var proj graph.Projection
	if err := json.Unmarshal(out.Bytes(), &proj); err != nil {
		t.Fatalf("decode projection: %v\n%s", err, out.String())
	}
	st, ok := proj.RunOverlay["probe"]
	if !ok {
		t.Fatalf("run_overlay missing 'probe': %+v", proj.RunOverlay)
	}
	if st.State != "completed" || st.Outcome != "ok" {
		t.Errorf("probe overlay = %+v, want completed/ok", st)
	}
}

func TestGraphMissingRun(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := cliGraph([]string{graphFixture, "--run", "ghost", "--state-dir", t.TempDir()}, &out, &errb); rc != ExitUsage {
		t.Fatalf("graph missing-run rc = %d, want ExitUsage; stderr: %s", rc, errb.String())
	}
}

func TestGraphBadPath(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := cliGraph([]string{"testdata/does-not-exist.yaml"}, &out, &errb); rc != ExitUsage {
		t.Fatalf("graph bad-path rc = %d, want ExitUsage", rc)
	}
}

func TestGraphUnknownOutput(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := cliGraph([]string{graphFixture, "--output", "yaml"}, &out, &errb); rc != ExitUsage {
		t.Fatalf("graph unknown-output rc = %d, want ExitUsage", rc)
	}
}

func TestGraphNoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := cliGraph(nil, &out, &errb); rc != ExitUsage {
		t.Fatalf("graph no-args rc = %d, want ExitUsage", rc)
	}
}
