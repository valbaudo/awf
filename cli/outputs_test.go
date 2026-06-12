package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
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

const outputsWF = `workflow: outputs-test
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
output_schema:
  type: object
  additionalProperties: false
  required: [summary]
  properties:
    summary: { type: string }
outputs:
  summary: "{{ step.summarize.summary }}"
graph:
  - id: summarize
    container: lab
    run: "true"
    output_schema:
      type: object
      additionalProperties: false
      required: [summary]
      properties:
        summary: { type: string }
`

// seedOutputsRun writes outputsWF to disk, loads it for the digest, and builds a
// run whose run.started digest matches and whose "summarize" step committed the
// given output. Returns (wfPath, stateDir).
func seedOutputsRun(t *testing.T, summarizeOutput string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	wfPath := filepath.Join(dir, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(outputsWF), 0o644); err != nil {
		t.Fatalf("write wf: %v", err)
	}
	ld, err := loader.Load(wfPath)
	if err != nil {
		t.Fatalf("loader.Load: %v", err)
	}
	// Fail fast on a bad fixture (matches cli/resume_test.go) so a malformed
	// outputsWF surfaces here, not later as a confusing ExitRunFailed.
	if diags := ir.Validate(ld); ir.HasErrors(diags) {
		t.Fatalf("fixture invalid: %v", diags)
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	stateDir := t.TempDir()
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	ref, err := blobs.Put([]byte(summarizeOutput))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	writeRunLog(t, stateDir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: marshal(t, engine.RunStartedData{RunID: "r1", WorkflowDigest: digest})},
		state.Event{Type: engine.EventNodeCompleted, Path: "summarize", Data: marshal(t, engine.NodeCompletedData{Outcome: "ok", OutputsRef: ref})},
	)
	return wfPath, stateDir
}

func TestOutputsContractHappyPath(t *testing.T) {
	wfPath, stateDir := seedOutputsRun(t, `{"summary":"hello"}`)
	var out, errb bytes.Buffer
	if rc := cliOutputs([]string{"r1", "--workflow", wfPath, "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("rc = %d (want %d); stderr=%s", rc, ExitOK, errb.String())
	}
	if !strings.Contains(out.String(), `"summary": "hello"`) {
		t.Fatalf("stdout = %q, want summary=hello", out.String())
	}
}

func TestOutputsContractDigestMismatch(t *testing.T) {
	wfPath, stateDir := seedOutputsRun(t, `{"summary":"hello"}`)
	writeRunLog(t, stateDir, "r2",
		state.Event{Type: engine.EventRunStarted, Data: marshal(t, engine.RunStartedData{RunID: "r2", WorkflowDigest: "WRONG"})},
	)
	var out, errb bytes.Buffer
	if rc := cliOutputs([]string{"r2", "--workflow", wfPath, "--state-dir", stateDir}, &out, &errb); rc != ExitUsage {
		t.Fatalf("rc = %d (want %d); stderr=%s", rc, ExitUsage, errb.String())
	}
	if !strings.Contains(errb.String(), "digest mismatch") {
		t.Fatalf("stderr = %q, want digest mismatch", errb.String())
	}
}

func TestOutputsContractUncommittedRefIsReadFailure(t *testing.T) {
	dir := t.TempDir()
	wfPath := filepath.Join(dir, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(outputsWF), 0o644); err != nil {
		t.Fatalf("write wf: %v", err)
	}
	ld, _ := loader.Load(wfPath)
	digest, _ := ld.ComputeDigest()
	stateDir := t.TempDir()
	writeRunLog(t, stateDir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: marshal(t, engine.RunStartedData{RunID: "r1", WorkflowDigest: digest})},
	)
	var out, errb bytes.Buffer
	if rc := cliOutputs([]string{"r1", "--workflow", wfPath, "--state-dir", stateDir}, &out, &errb); rc != ExitRunFailed {
		t.Fatalf("rc = %d (want %d); stderr=%s", rc, ExitRunFailed, errb.String())
	}
}
