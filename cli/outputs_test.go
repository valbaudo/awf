package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// marshal is a local helper: struct -> json.RawMessage for event Data.
func marshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestOutputsStep(t *testing.T) {
	stateDir := t.TempDir()
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	ref, err := blobs.Put([]byte(`{"verdict":"clean"}`))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	writeRunLog(t, stateDir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: marshal(t, engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "scan", Data: marshal(t, engine.NodeCompletedData{Outcome: "ok", OutputsRef: ref})},
	)
	var out, errb bytes.Buffer
	if rc := cliOutputs([]string{"r1", "--step", "scan", "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("rc = %d (want %d); stderr=%s", rc, ExitOK, errb.String())
	}
	if !strings.Contains(out.String(), `"verdict": "clean"`) {
		t.Fatalf("stdout = %q, want the verdict map", out.String())
	}
}

func TestOutputsStepSucceedsWhenUnrelatedBlobMissing(t *testing.T) {
	// --step is a TARGETED read, not a full engine.Fold. "other"'s OutputsRef
	// points at a blob NOT in the store; reading "scan" must still succeed.
	stateDir := t.TempDir()
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	ref, err := blobs.Put([]byte(`{"verdict":"clean"}`))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	writeRunLog(t, stateDir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: marshal(t, engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "scan", Data: marshal(t, engine.NodeCompletedData{Outcome: "ok", OutputsRef: ref})},
		state.Event{Type: engine.EventNodeCompleted, Path: "other", Data: marshal(t, engine.NodeCompletedData{Outcome: "ok", OutputsRef: "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"})},
	)
	var out, errb bytes.Buffer
	if rc := cliOutputs([]string{"r1", "--step", "scan", "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("rc = %d (want %d); stderr=%s", rc, ExitOK, errb.String())
	}
}

func TestOutputsStepRejectsRuntimePath(t *testing.T) {
	var out, errb bytes.Buffer
	rc := cliOutputs([]string{"r1", "--step", "gate[0].generate", "--state-dir", t.TempDir()}, &out, &errb)
	if rc != ExitUsage {
		t.Fatalf("rc = %d (want %d)", rc, ExitUsage)
	}
	if !strings.Contains(errb.String(), "top-level node ids") {
		t.Fatalf("stderr = %q, want top-level-ids message", errb.String())
	}
}

func TestOutputsMixingFormsIsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	rc := cliOutputs([]string{"r1", "--workflow", "wf.yaml", "--step", "scan", "--state-dir", t.TempDir()}, &out, &errb)
	if rc != ExitUsage {
		t.Fatalf("rc = %d (want %d)", rc, ExitUsage)
	}
}

func TestOutputsReachableViaDispatch(t *testing.T) {
	// Mutation-grade: drives the REAL Run entrypoint, so removing the
	// `case "outputs":` arm from cli.go turns this RED.
	var out, errb bytes.Buffer
	rc := Run([]string{"outputs", "ghost", "--step", "x", "--state-dir", t.TempDir()}, &out, &errb)
	if rc != ExitUsage {
		t.Fatalf("rc = %d (want %d)", rc, ExitUsage)
	}
	if !strings.Contains(errb.String(), "awf outputs:") {
		t.Fatalf("dispatch did not reach the outputs handler; stderr=%q", errb.String())
	}
}
