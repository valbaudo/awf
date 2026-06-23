package cli_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/graph"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

// A real `awf run` snapshots the run's full canonical definition into Blobs and records the ref in
// run.started.definition_ref. The blob must reconstruct a definition that projects to the same
// graph as loading the file directly — that is what lets a viewer render the run faithfully after
// the file is edited.
func TestCLIRunWritesResolvableDefinitionSnapshot(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo step2", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`)}, nil)
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)

	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	runner := newTestRunner(t, fake)
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK\nstderr: %s", rc, stderr.String())
	}

	// Pull definition_ref out of run.started.
	logPath := filepath.Join(stateDir, "runs", "test-run-1", "log")
	fl, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = fl.Close() }()
	events, err := fl.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var rsd engine.RunStartedData
	found := false
	for _, e := range events {
		if e.Type == engine.EventRunStarted {
			if err := json.Unmarshal(e.Data, &rsd); err != nil {
				t.Fatalf("unmarshal run.started: %v", err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no run.started event")
	}
	if rsd.DefinitionRef == "" {
		t.Fatal("run.started.definition_ref is empty; expected a snapshot ref")
	}

	// The snapshot blob must reconstruct the same graph as loading the file directly.
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	snap, err := engine.LoadRunStartedDefinitionSnapshot(blobs, rsd.DefinitionRef)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	direct, err := loader.Load("testdata/phase2/seq.yaml")
	if err != nil {
		t.Fatalf("loader.Load: %v", err)
	}
	gotJSON, _ := json.Marshal(graph.BuildStaticLoaded(snap))
	wantJSON, _ := json.Marshal(graph.BuildStaticLoaded(direct))
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("snapshot graph differs from direct-load graph\nsnapshot: %s\ndirect:   %s", gotJSON, wantJSON)
	}
}
