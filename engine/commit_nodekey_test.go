package engine_test

// TestNodeKey* — WS-6b RED→GREEN tests for per-node node_key at commit + fold.
//
// Coverage:
//  1. A deterministic code step (with input_files) gets a non-empty NodeKey.
//  2. A code step with no input_files also gets a non-empty NodeKey.
//  3. engine.Commit called without Node (the non-code case) produces empty NodeKey.
//  4. A legacy node.completed event (no node_key field) folds to NodeKey=="" (backward compat).

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// TestNodeKeyCodeStep_WithInputFile runs a code step that has one input_files
// entry, then folds the log and asserts NodeKey == independently computed key.
func TestNodeKeyCodeStep_WithInputFile(t *testing.T) {
	t.Parallel()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	disp := &engine.LocalDispatcher{
		Backend: fake,
		Handles: map[string]container.Handle{"lab": h},
	}
	log := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()

	// Put an input file blob so we can reference it as input.files.data.
	inputContent := []byte("input blob content")
	inputCASRef, err := blobs.Put(inputContent)
	if err != nil {
		t.Fatalf("Put input blob: %v", err)
	}

	if err := log.Append(state.Event{
		Type: engine.EventRunStarted,
		Data: nodeKeyMustJSON(t, engine.RunStartedData{
			RunID:          "r1",
			WorkflowDigest: "awf-d1:sha256:abc",
			InputFiles:     map[string]string{"data": inputCASRef},
		}),
	}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "awf-d1:sha256:abc", nil)

	// The code step under test.
	cs := &ir.CodeStep{
		ID:        "build",
		Container: "lab",
		Run:       "./build.sh",
		InputFiles: map[string]string{
			"/work/data.txt": "input.files.data",
		},
	}
	wf := &ir.Workflow{
		ID:         "w",
		Version:    1,
		Containers: map[string]ir.Container{"lab": {}},
		Graph:      ir.NodeList{cs},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	fake.ProgramExec("./build.sh", container.ExecResult{ExitCode: 0, Stdout: []byte("built\n")}, nil)

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{
		InputFiles: map[string]string{"data": inputCASRef},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}

	// Fold the log and check NodeResult.NodeKey.
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	folded, err := engine.Fold(events, blobs)
	if err != nil {
		t.Fatalf("engine.Fold: %v", err)
	}

	nr, ok := folded.Completed["build"]
	if !ok {
		t.Fatalf("no completed entry for build; completed=%v", folded.Completed)
	}
	if nr.NodeKey == "" {
		t.Fatal("NodeKey is empty for deterministic code step — expected non-empty")
	}

	// Independently compute the expected key.
	subtreeDigest, err := ir.NodeSubtreeDigest(cs)
	if err != nil {
		t.Fatalf("NodeSubtreeDigest: %v", err)
	}
	inputRefs := []string{inputCASRef}
	sort.Strings(inputRefs)
	wantKey := engine.ComputeNodeKey(subtreeDigest, inputRefs, nil)

	if nr.NodeKey != wantKey {
		t.Errorf("NodeKey = %q\n  want %q\n  (subtreeDigest=%q, inputRefs=%v)",
			nr.NodeKey, wantKey, subtreeDigest, inputRefs)
	}

	// Also confirm the node.completed event JSON embeds node_key.
	for _, e := range events {
		if e.Type != engine.EventNodeCompleted {
			continue
		}
		var d engine.NodeCompletedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("unmarshal node.completed: %v", err)
		}
		if d.NodeKey == "" {
			t.Error("NodeCompletedData.NodeKey is empty in the committed event")
		}
		if d.NodeKey != wantKey {
			t.Errorf("NodeCompletedData.NodeKey = %q, want %q", d.NodeKey, wantKey)
		}
	}
}

// TestNodeKeyCodeStep_NoInputFiles verifies a code step with no input_files
// still gets a non-empty NodeKey (key = H(subtree ‖ ∅ ‖ ∅)).
func TestNodeKeyCodeStep_NoInputFiles(t *testing.T) {
	t.Parallel()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	disp := &engine.LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"lab": h}}
	log := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: []byte(`{"run_id":"r1","workflow_digest":"d1"}`)}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d1", nil)

	cs := &ir.CodeStep{ID: "scan", Container: "lab", Run: "./scan.sh"}
	wf := &ir.Workflow{
		ID:         "w",
		Version:    1,
		Containers: map[string]ir.Container{"lab": {}},
		Graph:      ir.NodeList{cs},
	}
	def := &ir.LoadedDefinition{Workflow: wf}
	fake.ProgramExec("./scan.sh", container.ExecResult{ExitCode: 0}, nil)

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}

	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	folded, err := engine.Fold(events, blobs)
	if err != nil {
		t.Fatalf("engine.Fold: %v", err)
	}

	nr, ok := folded.Completed["scan"]
	if !ok {
		t.Fatalf("no completed entry for scan; completed=%v", folded.Completed)
	}
	if nr.NodeKey == "" {
		t.Fatal("NodeKey is empty for no-input code step — expected non-empty")
	}

	// Verify against independently computed key (nil inputRefs).
	subtreeDigest, err := ir.NodeSubtreeDigest(cs)
	if err != nil {
		t.Fatalf("NodeSubtreeDigest: %v", err)
	}
	wantKey := engine.ComputeNodeKey(subtreeDigest, nil, nil)
	if nr.NodeKey != wantKey {
		t.Errorf("NodeKey = %q, want %q (no-input case)", nr.NodeKey, wantKey)
	}
}

// TestNodeKeyNonCodeCommit_IsEmpty verifies that a Commit call where
// DispatchResult.Node is nil (the pattern for all non-code call sites:
// agent_step, react, reduce) produces an empty NodeKey.
func TestNodeKeyNonCodeCommit_IsEmpty(t *testing.T) {
	t.Parallel()
	log := state.NewInMemoryLog(clock.System{})
	blobs := state.NewInMemoryBlobs()
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: []byte(`{"run_id":"r1","workflow_digest":"d"}`)}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}
	// DispatchResult with no Node set — mirrors agent_step/react/reduce call sites.
	dr := engine.DispatchResult{
		Outcome: engine.OutcomeOK,
		Outputs: map[string]any{"k": "v"},
	}
	nr, err := engine.Commit(log, blobs, "agent-step", dr, false)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if nr.NodeKey != "" {
		t.Errorf("NodeKey = %q, want empty for non-code Commit call", nr.NodeKey)
	}

	// Also assert the JSON event omits node_key (omitempty).
	events, _ := log.Fold()
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted {
			if jsonContains(e.Data, "node_key") {
				t.Errorf("non-code node.completed must omit node_key; got %s", e.Data)
			}
		}
	}
}

// TestNodeKeyLegacyLog_FoldsClean verifies that a hand-crafted node.completed
// event WITHOUT a node_key field folds to NodeKey=="" (backward compatibility).
func TestNodeKeyLegacyLog_FoldsClean(t *testing.T) {
	t.Parallel()
	blobs := state.NewInMemoryBlobs()
	// Construct events as if written by a pre-WS6b engine: run.started + node.completed without node_key.
	events := []state.Event{
		{
			Seq: 1, Type: engine.EventRunStarted, Path: "",
			Data: []byte(`{"run_id":"r1","workflow_digest":"d"}`),
		},
		{
			Seq: 2, Type: engine.EventNodeCompleted, Path: "step1",
			Data: []byte(`{"outcome":"ok"}`),
		},
	}
	folded, err := engine.Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	nr, ok := folded.Completed["step1"]
	if !ok {
		t.Fatalf("step1 not in Completed")
	}
	if nr.NodeKey != "" {
		t.Errorf("legacy log NodeKey = %q, want empty (backward-compatible)", nr.NodeKey)
	}
}

func nodeKeyMustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("nodeKeyMustJSON: %v", err)
	}
	return b
}

func jsonContains(data []byte, key string) bool {
	needle := `"` + key + `"`
	for i := 0; i+len(needle) <= len(data); i++ {
		if string(data[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}
