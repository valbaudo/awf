package ui

import (
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// When a run carries a definition snapshot (run.started.definition_ref), the UI renders it against
// that snapshot's structure — the version the run actually executed against — not the workflow file
// currently loaded by `awf ui`. The server's current workflow is demoWorkflow (build + gate); the
// run below executed an older single-step structure, so the projection must show the old step and
// NOT the current file's nodes.
func TestGraphRendersAgainstRunSnapshot(t *testing.T) {
	dir := t.TempDir()
	oldLD := &ir.LoadedDefinition{Workflow: &ir.Workflow{
		ID:    "demo",
		Graph: ir.NodeList{&ir.CodeStep{ID: "oldstep", Run: "echo old"}},
	}}
	blobs, err := state.OpenBlobs(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	ref, err := engine.StoreRunStartedDefinitionSnapshot(blobs, oldLD)
	if err != nil {
		t.Fatalf("store snapshot: %v", err)
	}

	writeRunLog(t, dir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{
			RunID: "r1", WorkflowDigest: "old-digest", WorkflowID: "demo", DefinitionRef: ref,
		})},
		state.Event{Type: engine.EventNodeStarted, Path: "oldstep", Data: mustData(engine.NodeStartedData{Kind: "code"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "oldstep", Data: mustData(engine.NodeCompletedData{Outcome: "ok"})},
	)

	ts := newTestServer(t, dir)
	p := getProjection(t, ts.URL+"/api/graph?run=r1")

	var paths []string
	for _, n := range p.Nodes {
		paths = append(paths, n.Path)
	}
	if !containsPath(paths, "oldstep") {
		t.Errorf("snapshot render missing 'oldstep'; paths=%v", paths)
	}
	if containsPath(paths, "build") {
		t.Errorf("snapshot render leaked current-file node 'build' (should use the run's own structure); paths=%v", paths)
	}
}

// Without a snapshot (pre-snapshot run), the UI falls back to the currently loaded file, exactly as
// before — so the current file's nodes (build) appear.
func TestGraphFallsBackWhenNoSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeRunLog(t, dir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{
			RunID: "r1", WorkflowDigest: testDigest, WorkflowID: "demo", // no DefinitionRef
		})},
		state.Event{Type: engine.EventNodeStarted, Path: "build", Data: mustData(engine.NodeStartedData{Kind: "code"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "build", Data: mustData(engine.NodeCompletedData{Outcome: "ok"})},
	)
	ts := newTestServer(t, dir)
	p := getProjection(t, ts.URL+"/api/graph?run=r1")

	found := false
	for _, n := range p.Nodes {
		if n.Path == "build" {
			found = true
		}
	}
	if !found {
		t.Error("fallback render missing current-file node 'build'")
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}
