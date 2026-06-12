# `awf outputs` Implementation Plan (P1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only `awf outputs <run-id>` CLI command that returns a completed run's typed outputs as JSON — the `outputs:` contract (digest-checked) or one top-level node's output via `--step` — so an external orchestrator can retrieve a run's result without parsing AWF's internal blob layout.

**Architecture:** Two forms. The `outputs:` form folds the log to a `*engine.RunState`, re-loads + digest-checks the workflow file (like `awf resume`), and evaluates `wf.Outputs` via a newly-extracted shared `engine.EvaluateExports` (factored out of the existing `evaluateWorkflowExports` so the sub-workflow-call path and this top-level path are one implementation). The `--step` form does a *targeted* read (scan events → one `node.completed` → one blob) with no full fold. Read-only: no new event, no write-path change. Plus a non-fatal validate warning when a top-level output binds a conditionally-scoped step, and a man-page entry.

**Tech Stack:** Go; existing AWF packages `engine`, `ir`, `loader`, `state`, `cli`. TDD with `go test`; green bar is `make lint test`.

**Source spec:** `docs/superpowers/specs/2026-06-12-awf-correctable-gaps-design.md` §2 (P1).

---

## File Structure

| File | Responsibility | Create/Modify |
|---|---|---|
| `engine/workflow_exports.go` | Extract exported `EvaluateExports(rs, wf, ctxPath, input, blobs)`; `evaluateWorkflowExports` becomes a thin call-path wrapper | Modify |
| `engine/workflow_exports_test.go` | Add a top-level `EvaluateExports` test; existing tests must stay green | Modify |
| `cli/outputs.go` | The `awf outputs` command: flag parse, `--step` targeted read, `outputs:` form, JSON emit | Create |
| `cli/outputs_test.go` | CLI tests: `--step`, unrelated-blob-missing robustness, `outputs:` happy path, digest mismatch, no-`outputs:`, uncommitted-ref → exit 1, form-mixing usage | Create |
| `cli/cli.go` | Dispatch `case "outputs":` + add to usage listing | Modify |
| `ir/validate_refs.go` | Non-fatal conditional-scope warning in `validateWorkflowExports` + two helpers | Modify |
| `ir/validate_refs_test.go` | Unit tests for the two helpers | Modify (or create if absent) |
| `man/awf.1.md` | `awf outputs` command-reference entry | Modify |
| `docs/research/awf-as-agent-building-substrate.md` | Doc hygiene (A1/A2/A6 shipped). **Main working tree only — see Task 6** | Modify (separate) |

**Scope note:** This plan covers **P1 only**. P2 (native resume) and P3 (tool-loop keystone) are roadmap entries in the spec and get their own plans. Artifact (file) read-back (`--file`) is explicitly **deferred** (spec §2.10); do not implement it here.

---

## Task 1: Extract `engine.EvaluateExports` (shared exporter)

**Files:**
- Modify: `engine/workflow_exports.go:18-77`
- Test: `engine/workflow_exports_test.go`

**Why:** The `outputs:` form (Task 3) must evaluate a workflow's exports against a *top-level* folded `RunState`. The existing `evaluateWorkflowExports` is unexported and child-call-specific — it calls `childRunStateForCall` (prefix-strips the parent's keys), which is **wrong** for a top-level run (whose keys are already bare ids). Factor the eval body into an exported function that takes an already-correct `(rs, ctxPath, input)`.

- [ ] **Step 1: Write the failing test** (append to `engine/workflow_exports_test.go`)

```go
func TestEvaluateExportsTopLevel(t *testing.T) {
	// Top-level run: the producer is keyed at its BARE id "summarize" (no prefix),
	// ctxPath is "", input is nil. This is the shape cli/outputs.go uses.
	wf := awfChildWorkflowWithOutput("root", "summary")
	rs := NewRunState("run-1", "digest-1", nil)
	rs.RecordCompleted("summarize", NodeResult{
		Outcome: OutcomeOK,
		Outputs: map[string]any{"summary": "top-level"},
	})

	got, err := EvaluateExports(rs, wf, "", nil, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("EvaluateExports: %v", err)
	}
	if got.Outputs["summary"] != "top-level" {
		t.Fatalf("Outputs[summary] = %v, want top-level", got.Outputs["summary"])
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestEvaluateExportsTopLevel -v`
Expected: FAIL — `undefined: EvaluateExports`.

- [ ] **Step 3: Refactor `engine/workflow_exports.go`** — replace the existing `evaluateWorkflowExports` function (lines 18-77) with these two functions. The body from `var out WorkflowExportResult` through `return out, nil` is moved verbatim into `EvaluateExports`; only the scope-construction line changes (it now uses the passed `rs`/`ctxPath`/`input` instead of building a child).

```go
// EvaluateExports evaluates a workflow's outputs:/output_schema/output_files
// against an ALREADY-CORRECT (rs, ctxPath, input). The caller constructs rs:
// a sub-workflow call prefix-strips via childRunStateForCall and passes the
// parent path; a top-level run (awf outputs) passes the folded RunState
// directly with ctxPath="" and input=nil. Shared so both paths are one
// implementation (mirrors the engine's top-level-vs-call split in
// interpreter_context.go).
func EvaluateExports(rs *RunState, wf *ir.Workflow, ctxPath string, input map[string]any, blobs state.Blobs) (WorkflowExportResult, error) {
	scope := NewScopeWithInput(rs, wf, ctxPath, input)

	var out WorkflowExportResult
	if wf.OutputSchema != nil {
		out.Outputs = map[string]any{}
	}
	if len(wf.Outputs) > 0 {
		out.Outputs = make(map[string]any, len(wf.Outputs))
		keys := make([]string, 0, len(wf.Outputs))
		for key := range wf.Outputs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value, err := template.EvalTemplateValue(wf.Outputs[key], scope)
			if err != nil {
				return WorkflowExportResult{}, fmt.Errorf("evaluate workflow output %q: %w", key, err)
			}
			out.Outputs[key] = value
		}
	}
	if wf.OutputSchema != nil {
		if err := ValidateOutputMap(out.Outputs, wf.OutputSchema); err != nil {
			return WorkflowExportResult{}, fmt.Errorf("workflow output_schema validation: %w", err)
		}
	}

	if len(wf.ArtifactExports) > 0 {
		out.Files = make(map[string]string, len(wf.ArtifactExports))
		keys := make([]string, 0, len(wf.ArtifactExports))
		for key := range wf.ArtifactExports {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			raw := wf.ArtifactExports[key]
			if strings.Contains(raw, "{{") || strings.Contains(raw, "}}") {
				return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s must be a static step.<id>.files.<name> reference, not a template", key)
			}
			id, name, ok := template.ParseArtifactRef(raw)
			if !ok {
				return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s=%s: expected step.<id>.files.<name>", key, raw)
			}
			ref, err := resolveNamedArtifactRef(scope, wf, id, name)
			if err != nil {
				return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s: %w", key, err)
			}
			if blobs != nil {
				if _, err := blobs.Get(ref); err != nil {
					return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s ref %q is missing from blobs: %w", key, ref, err)
				}
			}
			out.Files[key] = ref
		}
	}

	return out, nil
}

// evaluateWorkflowExports is the sub-workflow-CALL path: build the child
// RunState (prefix-strip the parent's keys) then delegate to the shared
// EvaluateExports. Call-specific construction stays here.
func evaluateWorkflowExports(parent *RunState, wf *ir.Workflow, callPath string, input map[string]any, blobs state.Blobs) (WorkflowExportResult, error) {
	child := childRunStateForCall(parent, callPath, input)
	return EvaluateExports(child, wf, ir.CallWorkflowParentPath(callPath), input, blobs)
}
```

- [ ] **Step 4: Run the new test and the existing ones (regression guard)**

Run: `go test ./engine/ -run 'TestEvaluateExports|TestEvaluateWorkflowOutputs|TestEvaluateWorkflowOutputFiles|TestResolveCallArtifactRef' -v`
Expected: PASS — the new `TestEvaluateExportsTopLevel` AND every existing `evaluateWorkflowExports` test (the call-path behavior must be unchanged).

- [ ] **Step 5: Commit**

```bash
git add engine/workflow_exports.go engine/workflow_exports_test.go
git commit -m "refactor(engine): extract shared EvaluateExports from evaluateWorkflowExports"
```

---

## Task 2: `cli/outputs.go` — `--step` targeted read + dispatch

**Files:**
- Create: `cli/outputs.go`
- Modify: `cli/cli.go` (dispatch `case "outputs":` near `case "trace":`, and the usage listing)
- Test: `cli/outputs_test.go`

**Why:** Ship `awf outputs <run-id> --step <node-id>` end-to-end first — it needs no workflow file and no refactor dependency. The read is *targeted* (one event, one blob), NOT a full `engine.Fold`, so it works even when an unrelated step's blob is missing.

- [ ] **Step 1: Write the failing tests** (create `cli/outputs_test.go`)

```go
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
	// Locks the R2 contract: --step does a TARGETED read, not a full engine.Fold.
	// "other"'s OutputsRef points at a blob that is NOT in the store; reading
	// "scan" must still succeed (a full fold would error on the missing blob).
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
	rc := cliOutputs([]string{"r1", "wf.yaml", "--step", "scan", "--state-dir", t.TempDir()}, &out, &errb)
	if rc != ExitUsage {
		t.Fatalf("rc = %d (want %d)", rc, ExitUsage)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cli/ -run TestOutputs -v`
Expected: FAIL — `undefined: cliOutputs`.

- [ ] **Step 3: Create `cli/outputs.go`** with the command, flag parsing, the `--step` targeted read, and the JSON emitter. (The `outputs:` form `outputsContract` is added in Task 3 — for now reference a stub that returns a usage error so the file compiles.)

```go
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

func printOutputsUsage(w io.Writer) {
	fprintln(w, "usage: awf outputs <run-id> [<workflow-path>] [--step <node-id>] [--state-dir <dir>]")
	fprintln(w, "")
	fprintln(w, "  read a completed run's typed outputs as JSON.")
	fprintln(w, "  <workflow-path>    evaluate the workflow's outputs: contract (digest-checked")
	fprintln(w, "                     against the run, like `awf resume`)")
	fprintln(w, "  --step <node-id>   instead, emit one top-level node's typed output (no")
	fprintln(w, "                     workflow file needed)")
	fprintln(w, "  --state-dir <dir>  base directory for runs/ and blobs/ (default: ./.awf)")
	fprintln(w, "")
	fprintln(w, "  Exit reflects the READ, not the run: 0 ok, 2 bad invocation, 1 read failed.")
	fprintln(w, "  Check `awf ls` or the original `awf run` exit code for run success.")
}

func cliOutputs(args []string, stdout, stderr io.Writer) int {
	fs0 := flag.NewFlagSet("outputs", flag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	stateDir := fs0.String("state-dir", ".awf", "base directory for runs/ and blobs/")
	step := fs0.String("step", "", "emit one top-level node's typed output")
	if err := fs0.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printOutputsUsage(stdout)
			return ExitOK
		}
		fprintf(stderr, "awf outputs: %v\n", err)
		printOutputsUsage(stderr)
		return ExitUsage
	}
	if fs0.NArg() < 1 {
		printOutputsUsage(stderr)
		return ExitUsage
	}
	runID := fs0.Arg(0)
	wfPath := ""
	if fs0.NArg() >= 2 {
		wfPath = fs0.Arg(1)
	}

	// Form check: --step is log-only; the outputs: form needs <workflow-path>.
	if *step != "" && wfPath != "" {
		fprintf(stderr, "awf outputs: use either --step (log-only) or the workflow-export form, not both\n")
		return ExitUsage
	}
	if *step == "" && wfPath == "" {
		fprintf(stderr, "awf outputs: provide a <workflow-path> (to read the outputs: contract) or --step <node-id>\n")
		return ExitUsage
	}

	logPath := filepath.Join(*stateDir, "runs", runID, "log")
	events, err := state.FoldFile(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fprintf(stderr, "awf outputs: no run with id %q at %q\n", runID, logPath)
		} else {
			fprintf(stderr, "awf outputs: fold log %q: %v\n", logPath, err)
		}
		return ExitUsage
	}
	blobs, err := state.OpenBlobs(filepath.Join(*stateDir, "blobs"))
	if err != nil {
		fprintf(stderr, "awf outputs: open blobs: %v\n", err)
		return ExitUsage
	}

	if *step != "" {
		return outputsStep(events, blobs, *step, stdout, stderr)
	}
	return outputsContract(events, blobs, runID, wfPath, stdout, stderr)
}

// outputsStep emits one top-level node's typed output via a TARGETED read —
// scan events for the node.completed, read its single OutputsRef blob. Not a
// full engine.Fold (which errors if any UNRELATED committed blob is missing).
func outputsStep(events []state.Event, blobs state.Blobs, nodeID string, stdout, stderr io.Writer) int {
	if isRuntimeSuffixedPath(nodeID) {
		fprintf(stderr, "awf outputs: %q is a runtime-internal path; P1 reads top-level node ids. Use `awf inspect`/`awf trace` for gate/map-internal outputs.\n", nodeID)
		return ExitUsage
	}
	// Last node.completed for this id wins (a resumed run may re-commit it).
	ref := ""
	found := false
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted && e.Path == nodeID {
			var d engine.NodeCompletedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				fprintf(stderr, "awf outputs: decode node.completed at %q: %v\n", nodeID, err)
				return ExitRunFailed
			}
			ref = d.OutputsRef
			found = true
		}
	}
	if !found {
		fprintf(stderr, "awf outputs: no committed step at %q\n", nodeID)
		return ExitRunFailed
	}
	if ref == "" {
		fprintf(stderr, "awf outputs: step %q has no typed output (no output_schema)\n", nodeID)
		return ExitRunFailed
	}
	raw, err := blobs.Get(ref)
	if err != nil {
		fprintf(stderr, "awf outputs: read output blob for %q: %v\n", nodeID, err)
		return ExitRunFailed
	}
	var outMap map[string]any
	if err := json.Unmarshal(raw, &outMap); err != nil {
		fprintf(stderr, "awf outputs: parse output blob for %q: %v\n", nodeID, err)
		return ExitRunFailed
	}
	return emitJSON(stdout, stderr, outMap)
}

// isRuntimeSuffixedPath rejects gate/map-internal runtime paths (spec §2.3):
// only top-level node ids are addressable in P1.
func isRuntimeSuffixedPath(p string) bool {
	return strings.Contains(p, "[") ||
		strings.Contains(p, ".iter-") ||
		strings.Contains(p, ".attempt-") ||
		strings.Contains(p, ".item-")
}

// emitJSON pretty-prints v (matching inspect/trace/ls/graph: NewEncoder +
// two-space indent + trailing newline).
func emitJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fprintf(stderr, "awf outputs: json encode: %v\n", err)
		return ExitRunFailed
	}
	return ExitOK
}

// outputsContract is implemented in Task 3. Stub keeps the file compiling.
func outputsContract(events []state.Event, blobs state.Blobs, runID, wfPath string, stdout, stderr io.Writer) int {
	_ = events
	_ = blobs
	_ = runID
	_ = wfPath
	_ = loader.Load
	_ = ir.HasErrors
	_ = engine.Fold
	fprintf(stderr, "awf outputs: outputs: form not yet implemented\n")
	return ExitUsage
}
```

> Note: the `_ = loader.Load` / `_ = ir.HasErrors` / `_ = engine.Fold` lines in the stub keep the imports used until Task 3 fills the body; remove them in Task 3.

- [ ] **Step 4: Wire dispatch in `cli/cli.go`.** Add the case next to `case "trace":` (line ~141), using the identical free-function call shape:

```go
	case "outputs":
		return cliOutputs(args[1:], stdout, stderr)
```

And add `outputs` to the usage listing (the `fprintln(w, ...)` subcommand lines around `cli/cli.go:154-156`):

```go
	fprintln(w, "  outputs   read a completed run's typed outputs as JSON")
```

- [ ] **Step 5: Run the Task-2 tests**

Run: `go test ./cli/ -run TestOutputs -v`
Expected: PASS for `TestOutputsStep`, `TestOutputsStepSucceedsWhenUnrelatedBlobMissing`, `TestOutputsStepRejectsRuntimePath`, `TestOutputsMixingFormsIsUsage`.

- [ ] **Step 6: Commit**

```bash
git add cli/outputs.go cli/outputs_test.go cli/cli.go
git commit -m "feat(cli): awf outputs --step (targeted single-node read) + dispatch"
```

---

## Task 3: `cli/outputs.go` — the `outputs:` contract form

**Files:**
- Modify: `cli/outputs.go` (replace the `outputsContract` stub)
- Test: `cli/outputs_test.go` (add cases)

**Why:** The default form: re-load + digest-check the workflow (like `awf resume`), fold to a `*RunState`, evaluate `wf.Outputs` via `engine.EvaluateExports` (Task 1).

- [ ] **Step 1: Write the failing tests** (append to `cli/outputs_test.go`). The fixture writes a real workflow YAML, loads it to get the digest, and builds a matching run.

```go
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
	if rc := cliOutputs([]string{"r1", wfPath, "--state-dir", stateDir}, &out, &errb); rc != ExitOK {
		t.Fatalf("rc = %d (want %d); stderr=%s", rc, ExitOK, errb.String())
	}
	if !strings.Contains(out.String(), `"summary": "hello"`) {
		t.Fatalf("stdout = %q, want summary=hello", out.String())
	}
}

func TestOutputsContractDigestMismatch(t *testing.T) {
	wfPath, stateDir := seedOutputsRun(t, `{"summary":"hello"}`)
	// Rewrite the run with a different digest -> mismatch -> ExitUsage.
	writeRunLog(t, stateDir, "r2",
		state.Event{Type: engine.EventRunStarted, Data: marshal(t, engine.RunStartedData{RunID: "r2", WorkflowDigest: "WRONG"})},
	)
	var out, errb bytes.Buffer
	if rc := cliOutputs([]string{"r2", wfPath, "--state-dir", stateDir}, &out, &errb); rc != ExitUsage {
		t.Fatalf("rc = %d (want %d); stderr=%s", rc, ExitUsage, errb.String())
	}
	if !strings.Contains(errb.String(), "digest mismatch") {
		t.Fatalf("stderr = %q, want digest mismatch", errb.String())
	}
}

func TestOutputsContractUncommittedRefIsReadFailure(t *testing.T) {
	// Run whose "summarize" step never committed -> the output ref is
	// unresolvable -> ExitRunFailed (1), not usage.
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
	if rc := cliOutputs([]string{"r1", wfPath, "--state-dir", stateDir}, &out, &errb); rc != ExitRunFailed {
		t.Fatalf("rc = %d (want %d); stderr=%s", rc, ExitRunFailed, errb.String())
	}
}
```

> Add `"os"` and `"github.com/valbaudo/awf/loader"` to the test file's imports.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cli/ -run TestOutputsContract -v`
Expected: FAIL — the stub returns `ExitUsage` with "not yet implemented".

- [ ] **Step 3: Replace the `outputsContract` stub** in `cli/outputs.go`:

```go
// outputsContract evaluates the workflow's outputs: contract against the run.
// Folds the log to a *RunState, re-loads + digest-checks the workflow (spec §8
// pinning, like awf resume), then runs the shared engine.EvaluateExports with a
// top-level scope (ctxPath="", input=nil → input.* resolves against the run's
// own input, matching the engine).
func outputsContract(events []state.Event, blobs state.Blobs, runID, wfPath string, stdout, stderr io.Writer) int {
	rs, err := engine.Fold(events, blobs)
	if err != nil {
		fprintf(stderr, "awf outputs: build run state: %v\n", err)
		return ExitRunFailed
	}
	ld, err := loader.Load(wfPath)
	if err != nil {
		fprintf(stderr, "awf outputs: %v\n", err)
		return ExitUsage
	}
	if diags := ir.Validate(ld); ir.HasErrors(diags) {
		fprintf(stderr, "awf outputs: workflow %q has validation errors\n", wfPath)
		return ExitRunFailed
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		fprintf(stderr, "awf outputs: compute digest: %v\n", err)
		return ExitUsage
	}
	if rs.WorkflowDigest != digest {
		fprintf(stderr, "awf outputs: workflow digest mismatch — run %q was started with digest %q, file %q now hashes to %q (spec §8 forbids reading outputs against a changed definition)\n", runID, rs.WorkflowDigest, wfPath, digest)
		return ExitUsage
	}
	if len(ld.Workflow.Outputs) == 0 {
		fprintf(stderr, "awf outputs: workflow %q declares no outputs:; use --step <node-id>\n", wfPath)
		return ExitUsage
	}
	res, err := engine.EvaluateExports(rs, ld.Workflow, "", nil, blobs)
	if err != nil {
		// A referenced step did not commit (skipped or never ran), or a schema
		// mismatch — a data condition (the run did not produce this output),
		// distinct from a usage error. See spec §2.4 (run-success != output-success).
		fprintf(stderr, "awf outputs: could not produce outputs (a referenced step did not commit, or schema mismatch): %v\n", err)
		return ExitRunFailed
	}
	return emitJSON(stdout, stderr, res.Outputs)
}
```

Then delete the throwaway `_ = loader.Load` / `_ = ir.HasErrors` / `_ = engine.Fold` lines from the old stub (the real body now uses these symbols).

- [ ] **Step 4: Run the contract tests + the full cli package**

Run: `go test ./cli/ -run TestOutputs -v && go test ./cli/`
Expected: PASS — all `TestOutputs*`, and the rest of the `cli` package unaffected.

- [ ] **Step 5: Commit**

```bash
git add cli/outputs.go cli/outputs_test.go
git commit -m "feat(cli): awf outputs: contract form (digest-checked, shared EvaluateExports)"
```

---

## Task 4: Validate-time conditional-scope warning

**Files:**
- Modify: `ir/validate_refs.go` (two helpers + a 4-line insertion in `validateWorkflowExports` at line ~979)
- Test: `ir/validate_refs_test.go`

**Why:** Spec §2.4 — a top-level output may bind a step inside an `if`/`gate`/`map` scope that doesn't commit at runtime; that validates clean but fails `awf outputs`. Emit a non-fatal warning (AWF's `warnf` channel: `ir/validate.go:145`, `Severity: Warning`, "warnings inform but don't fail the run"). Do not attempt full reachability — a warning is the right altitude.

- [ ] **Step 1: Write the failing helper tests** (append to `ir/validate_refs_test.go`)

```go
func TestConditionallyScoped(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"summarize", false},
		{"recon.scan", false},
		{"if[0].then.draft", true},
		{"gate[0].generate.draft", true},
		{"map[0].body.x", true},
		{"loop[0].body.x", true},
	}
	for _, c := range cases {
		if got := conditionallyScoped(c.path); got != c.want {
			t.Errorf("conditionallyScoped(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestOutputStepRefs(t *testing.T) {
	got := outputStepRefs(TemplateValue(`"{{ step.foo.bar }} and {{ step.baz.qux }}"`))
	want := []string{"foo", "baz"}
	if len(got) != len(want) {
		t.Fatalf("outputStepRefs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outputStepRefs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./ir/ -run 'TestConditionallyScoped|TestOutputStepRefs' -v`
Expected: FAIL — `undefined: conditionallyScoped` / `undefined: outputStepRefs`.

- [ ] **Step 3: Add the two helpers** to `ir/validate_refs.go` (near the other helpers; add `"regexp"` to the import block):

```go
// outputStepRefPattern extracts the <id> from `step.<id>.<field>` references
// in an output template's source. Used only for the AWF1048 conditional-scope
// WARNING — not for resolution (validateTemplateValueRefs owns that).
var outputStepRefPattern = regexp.MustCompile(`step\.([A-Za-z0-9_-]+)\.`)

func outputStepRefs(tv TemplateValue) []string {
	var ids []string
	for _, m := range outputStepRefPattern.FindAllStringSubmatch(string(tv), -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// conditionallyScoped reports whether a producer's STATIC path lies inside a
// conditional/multiplicity scope (if/gate/map/loop), i.e. it may not commit at
// runtime. Mirrors the path-segment inspection in SingleMapBodyShape.
func conditionallyScoped(staticPath string) bool {
	for _, seg := range strings.Split(staticPath, ".") {
		switch {
		case strings.HasPrefix(seg, "if["),
			strings.HasPrefix(seg, "gate["),
			strings.HasPrefix(seg, "map["),
			strings.HasPrefix(seg, "loop["):
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run the helper tests to verify they pass**

Run: `go test ./ir/ -run 'TestConditionallyScoped|TestOutputStepRefs' -v`
Expected: PASS.

- [ ] **Step 5: Wire the warning into `validateWorkflowExports`.** In `ir/validate_refs.go`, inside the `for _, key := range outputKeys` loop, immediately AFTER the existing `validateTemplateValueRefs(c, "AWF1048", path, ...)` call (line ~979), add:

```go
		for _, refID := range outputStepRefs(wf.Outputs[key]) {
			if p, ok := producers[refID]; ok && conditionallyScoped(p.path) {
				c.warnf(path, "AWF1048", fmt.Sprintf("%s: output %q binds step %q inside a conditional scope (%s); it may not commit, and `awf outputs` will then error", catalog["AWF1048"], key, refID, p.path))
			}
		}
```

- [ ] **Step 6: Run the full `ir` suite (regression + no-false-positive guard)**

Run: `go test ./ir/`
Expected: PASS — no existing fixture should newly trip the warning (warnings don't fail validation, but a broken existing assertion would surface here).

- [ ] **Step 7: Commit**

```bash
git add ir/validate_refs.go ir/validate_refs_test.go
git commit -m "feat(ir): warn when a top-level output binds a conditionally-scoped step"
```

---

## Task 5: Man-page entry for `awf outputs`

**Files:**
- Modify: `man/awf.1.md`

**Why:** AWF's man page is the command contract; a shipped command needs an entry. (Use the `updating-the-manual` skill if available.)

- [ ] **Step 1: Add an `awf outputs` section** to `man/awf.1.md`, placed alongside the existing `awf inspect` / `awf trace` command sections and following their formatting. Content:

```markdown
## awf outputs

`awf outputs <run-id> [<workflow-path>] [--step <node-id>] [--state-dir <dir>]`

Read a completed run's typed outputs as JSON (pretty-printed). Read-only — it does
not modify the run.

Two forms:

- **`outputs:` contract** (default): pass `<workflow-path>`. The file is re-loaded
  and its digest is checked against the run's pinned `WorkflowDigest` (a mismatch is
  refused, exactly like `awf resume`); the workflow's top-level `outputs:` block is
  evaluated and emitted as a JSON object.
- **`--step <node-id>`**: emit one top-level node's typed output, read directly from
  the log + blob store (no workflow file needed). `<node-id>` is a top-level node id;
  gate/map-internal runtime paths are not addressable (use `awf inspect`/`awf trace`).

The exit code reflects the READ, not the run's outcome (it succeeds on a committed
output even if a later step failed — check `awf ls` or the original `awf run` for run
status): `0` output emitted; `2` bad invocation (bad flags, mixing the two forms,
digest mismatch, run-not-found, no `outputs:` declared); `1` the requested output
could not be produced (a referenced step did not commit, or the workflow fails
validation).

Note: a workflow whose `outputs:` binds a step inside a conditional scope (`if`,
`gate`, `map`) produces an `awf validate` warning, because that output may not be
producible if the step is skipped at runtime.
```

- [ ] **Step 2: Commit**

```bash
git add man/awf.1.md
git commit -m "docs(man): document awf outputs"
```

---

## Task 6: Doc hygiene — research note (MAIN working tree, separate)

**Files:**
- Modify: `docs/research/awf-as-agent-building-substrate.md` — **in the main working tree, NOT this worktree branch.**

**Why / mechanics:** Spec §2.9. This file is **untracked** (`docs/` is gitignored and it was never force-added) and is **absent from the `awf-correctable-gaps` worktree checkout**. So this edit cannot be made on the P1 worktree branch — do it in the main working tree as a standalone change (or `git add -f` it there if it should become tracked). Scope strictly to this one file.

- [ ] **Step 1: In the main working tree, open `docs/research/awf-as-agent-building-substrate.md`** and add a dated banner near the top:

```markdown
> **Update (since 2026-06-06):** SP1–SP5 have shipped. A1 (`agents:`), A2 (`continues:`
> execution), and A6/G3 (blob-as-input) are now BUILT — the keystone trio A1→A4→A3 is
> reduced to **A4 + A3 only**. The §3.2/§8 "continues: validated-but-not-executed" caveat
> below is stale.
```

- [ ] **Step 2: Update the gap tables** so A1 (SP2), A2 (SP3 — `AgentInvocation.Thread` + `Caps.Threaded` + per-adapter prepend, e.g. `agent/awfllm/transport.go:84-88`), and A6/G3 (SP1 — `input_files`/named `output_files` + `Backend.CopyTo`) are marked shipped rather than "partially_supported"/"genuinely_missing".

- [ ] **Step 3: Commit (in the main tree).** This is a documentation-only change independent of the P1 worktree branch:

```bash
git add -f docs/research/awf-as-agent-building-substrate.md
git commit -m "docs(research): mark A1/A2/A6 shipped (SP1-3); keystone is now A4+A3"
```

---

## Final verification

- [ ] **Run the full green bar**

Run: `make lint test`
Expected: PASS (Tasks 1–4 changes; the `cli`, `engine`, and `ir` suites all green).

---

## Self-Review (run against the spec)

**Spec coverage:** §2.2 surface → Task 2/3 (flags, two forms, deferred `--file`); §2.3 two forms → Tasks 2 (`--step` targeted read, runtime-path rejection) & 3 (`outputs:` form, digest guard, `input=nil`); §2.4 run-success≠output-success → Task 3 (uncommitted-ref → exit 1, classified message) + Task 4 (validate warning); §2.5 read-only + one refactor → Task 1; §2.6 reused seams + `EvaluateExports` factoring → Task 1 + Task 3 (`state.OpenBlobs`, `engine.Fold`, digest guard); §2.7 exit codes 0/1/2 → Tasks 2/3 (ExitOK/ExitRunFailed/ExitUsage); §2.8 testing (two forms, mismatch, no-outputs, uncommitted-ref, unrelated-blob-missing, `EvaluateExports` unit test, regression guard) → Tasks 1–4; §2.9 doc hygiene → Task 6; §2.10 deferred `--file` → explicitly out of scope. Man page (AWF "man page is the contract") → Task 5.

**Type consistency:** `EvaluateExports(rs *RunState, wf *ir.Workflow, ctxPath string, input map[string]any, blobs state.Blobs) (WorkflowExportResult, error)` is used identically in Task 1 (definition + call-path wrapper) and Task 3 (`engine.EvaluateExports(rs, ld.Workflow, "", nil, blobs)`). `cliOutputs`/`outputsStep`/`outputsContract`/`emitJSON`/`isRuntimeSuffixedPath` signatures are consistent across Tasks 2–3. `conditionallyScoped`/`outputStepRefs` defined and used in Task 4. Exit constants `ExitOK`/`ExitUsage`/`ExitRunFailed` per `cli/cli.go:34-39`.
