# AWF Subworkflows Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement local importable AWF subworkflows as explicit callable modules with typed outputs, named artifact exports, digest/resume safety, per-call container isolation, and fake-backend conformance.

**Architecture:** Extend `loader.Load` to build a module graph, keep validation and digest pure over `ir.LoadedDefinition`, and add a step-like `CallStep` that runs a child workflow under `<call-id>.workflow.*` while committing the parent-visible product at `<call-id>`. Persist `call.started{input_ref,runtimes}` before child execution, scope child containers per call, keep internals private for refs, and make CLI/obs/graph walkers use central loaded-definition traversal helpers.

**Tech Stack:** Go 1.26, packages `ir`, `frontend/yaml`, `loader`, `template`, `engine`, `cli`, `obs`, `graph`, `ui`; existing `state.Log`/`Blobs`, `container.Backend`, `agent.Adapter`, fake backend conformance; verification with `make lint test` plus `govulncheck ./...`.

---

## Source Documents

- Design spec: `docs/superpowers/specs/2026-06-09-awf-subworkflows-design.md`
- Workflow format authority: `man/awf-workflow.5.md`
- Runtime design authority: `docs/runtime-design.md`
- Project rules: `CLAUDE.md` and `AGENTS.md`

## File Map

Dependency and security preflight:

- Verify `go.mod`: record current toolchain/module update candidates; do not edit in this feature plan.
- Verify `go.sum`: ensure preflight leaves dependency files untouched.
- Add a separate dependency/security plan and PR only if preflight finds a reachable vulnerability that blocks this feature.

Documentation and format contract:

- Modify `man/awf-workflow.5.md`: document `imports`, `call`, workflow exports, path semantics, digest/resume behavior, and local-only security rules.
- Modify `man/awf-workflow.5`: regenerate from markdown if this repo's existing workflow expects checked-in roff.
- Modify `docs/runtime-design.md`: update loader, digest, execution model, validation, event log, and CLI lifecycle sections.
- Modify `README.md` only if examples or feature lists contradict import/call behavior.

IR and validation:

- Modify `ir/types.go`: add `Workflow.Imports`, `Workflow.OutputSchema`, `Workflow.Outputs`, `ArtifactExports`, and `TemplateValue`.
- Modify `ir/node.go`: add `CallStep`.
- Modify `ir/node_unmarshal.go`: add `call` as a step discriminator.
- Modify `ir/node_marshal.go`: marshal `CallStep` in the flat step shape.
- Modify `ir/node_test.go` / `ir/tags_test.go`: cover `CallStep` registry and tags.
- Modify `ir/walk.go`: include `CallStep` as a leaf step.
- Modify `ir/path.go`: add static helper for the child workflow parent path under a call.
- Modify `ir/loaded.go`: add `LoadedModule`, module map fields, and narrow traversal helpers.
- Modify `ir/digest.go`: fold module/import frames and module external bytes.
- Modify `ir/diagnostic.go`: add optional `Source` field and import/call diagnostic codes.
- Modify `ir/validate.go` and focused validation files: validate imports/calls/exports and module-local refs.
- Modify `ir/validate_refs.go`: treat `CallStep` as a producer using child export schema; validate workflow `outputs` templates child-locally.
- Modify `ir/validate_input_files.go`: resolve call artifact products.
- Modify `ir/validate_output_files.go`: validate workflow-level artifact export refs.

Frontend and loader:

- Modify `frontend/yaml/yaml_test.go`: add decode assertions for `imports`, workflow `output_schema`, `outputs`, and workflow `output_files`.
- Modify `loader/loader.go`: recursive module loading and per-module root handling.
- Modify `loader/assets.go`: reuse for module-local assets; avoid policy drift.
- Add `loader/errors.go`: typed loader errors that CLI validate can convert to diagnostics.
- Add `loader/errors_test.go`: assert loader error codes through `errors.As`.
- Add `loader/safepath.go`: shared manifest-path normalization for imports, compose files, and assets.
- Add `loader/safepath_test.go`: focused safe-path tests using `fs.ValidPath`, `filepath.Localize`, and `os.Root` containment expectations.
- Add `loader/imports.go`: recursive import graph loading, cycle/depth checks, module ids.
- Add `loader/imports_test.go`: focused import graph tests.

Template:

- Add `template/value.go`: typed value evaluation for raw JSON template values.
- Add tests in `template/value_test.go`.

Engine:

- Modify `engine/events.go`: add `EventCallStarted`, `CallStartedData`.
- Modify `engine/fold.go`: fold call-start records.
- Modify `engine/runstate.go`: add `CallStarted` map/accessors.
- Modify `engine/interpreter.go`: dispatch `CallStep`.
- Add `engine/interpreter_context.go`: unexported context struct that keeps call support from widening interpreter signatures.
- Add `engine/call_step.go`: call execution, call input persistence, child graph execution, export commit, failure boundary.
- Add `engine/runtime_resolution.go`: shared runtime-ref walking, adapter version resolution, and drift comparison used by CLI and call execution.
- Add `engine/workflow_exports.go`: evaluate workflow outputs and artifact aliases.
- Modify `engine/commit.go`: centralize `node.completed` append/sync behind an unexported helper.
- Modify `engine/signal_step.go`: route signal half-commit completion through the shared helper.
- Modify `engine/scope.go`: support scope with child-local input without mutating root `RunState.Input`.
- Modify `engine/artifact_scope.go`: resolve call product files from the call node.
- Add `engine/asset_keys.go`: root-compatible and module-qualified asset snapshot keys.
- Add `engine/runtime_handles.go`: create/destroy per-call child handles using the existing `*LocalDispatcher` scoped-handle pattern.
- Modify `engine/path.go`: add call runtime path and runtime container/snapshot key helpers separate from static diagnostic paths.
- Modify snapshot-related code in `engine/commit.go`, `engine/fold.go`, and CLI restore paths to use qualified container keys.

CLI:

- Modify `cli/run.go`: module-aware backend auto, asset snapshots, root lifecycle only.
- Modify `cli/resume.go`: digest reload, call runtime drift, qualified snapshot restore.
- Modify `cli/backend_features.go`: scan reachable modules/calls.
- Modify `cli/runtimes.go`: replace private runtime resolution implementation with wrappers or call sites that use `engine/runtime_resolution.go`.
- Modify `cli/threaded_guard.go`: module/call-aware threaded checks.
- Modify `cli/validate.go`: print diagnostic source file in text and JSON output.
- Modify `cli/ui.go`: compute graph/UI digests from `LoadedDefinition`.
- Modify `cli/inspect.go` and `cli/trace.go`: project call events/paths.

Obs, graph, UI:

- Modify `obs/project.go`: include call.started and call boundary state.
- Modify `graph/graph.go` and related instance code: render call nodes and nested child nodes.
- Modify `ui/src/projection.ts`: keep current projection unchanged when graph JSON is backward-compatible; add/update projection tests when graph JSON gains call-specific fields.

Conformance:

- Modify `conformance/suite.go` and fixture helpers.
- Add fixtures for simple call, resume drift, half-commit resume, artifact export, repeated calls, and nested calls.

Plan artifact persistence:

- Force-add `docs/superpowers/specs/2026-06-09-awf-subworkflows-design.md` and `docs/superpowers/plans/2026-06-09-awf-subworkflows.md`, or move them to a tracked location before implementation begins.

---

## Execution Phases

```text
dependency/security preflight
  |
  v
plan artifact visible to git
  |
  v
docs contract
  |
  v
IR node + module types
  |
  v
loader recursive imports + digest
  |
  v
validation + diagnostics
  |
  v
engine call.started + CallStep runtime
  |
  v
CLI/obs/graph command coverage
  |
  v
conformance + final docs
```

Run `make lint test` at the end of every implementation phase before committing. Run `govulncheck ./...` during preflight and again in final verification. Do not change dependencies in this feature plan; if `govulncheck` reports a reachable vulnerability that requires a dependency/toolchain change, stop and create a separate dependency/security plan and PR. Use focused tests for red/green work, but do not treat focused tests as the final gate.

---

## Task -1: Dependency And Vulnerability Preflight

**Files:**
- Verify: `go.mod`
- Verify: `go.sum`

- [ ] **Step 1: Record current baseline**

  ```bash
  if ! command -v govulncheck >/dev/null; then
    go install golang.org/x/vuln/cmd/govulncheck@latest
    export PATH="$(go env GOPATH)/bin:$PATH"
  fi
  go env GOVERSION GOTOOLCHAIN
  go list -m -u -json github.com/docker/docker github.com/docker/compose/v2 github.com/docker/cli github.com/compose-spec/compose-go/v2 github.com/moby/spdystream go.opentelemetry.io/otel go.opentelemetry.io/otel/sdk golang.org/x/crypto golang.org/x/sys golang.org/x/sync
  govulncheck ./...
  ```

  Expected: `govulncheck` is available, Go reports the currently pinned 1.26 patch level, module update candidates are visible, and no files are modified.

- [ ] **Step 2: Decide whether feature work can proceed**

  Continue only when one of these is true:

  - `govulncheck ./...` exits cleanly.
  - `govulncheck ./...` reports no reachable finding that has an available fix.
  - `govulncheck ./...` reports only findings that are already tracked by an existing security triage document.

  Stop this feature plan when `govulncheck` reports a reachable finding with an available fix or a toolchain/dependency migration requirement. In that case, write a separate dependency/security plan and PR first. Do not edit `go.mod`, `go.sum`, or `security/*` as part of the subworkflow implementation plan.

- [ ] **Step 3: Verify dependency files stayed untouched**

  ```bash
  git status --short -- go.mod go.sum security
  ```

  Expected: no output caused by this task. If this task changed dependency or security files, revert only those edits from this task before continuing.

---

## Task 0: Plan Artifact Persistence

**Files:**
- Verify: `.gitignore`
- Verify: `docs/superpowers/specs/2026-06-09-awf-subworkflows-design.md`
- Verify: `docs/superpowers/plans/2026-06-09-awf-subworkflows.md`

- [ ] **Step 1: Verify the plan files are ignored**

  ```bash
  git check-ignore -v docs/superpowers/specs/2026-06-09-awf-subworkflows-design.md docs/superpowers/plans/2026-06-09-awf-subworkflows.md
  ```

  Expected: both paths are ignored by `.gitignore:3:docs/`.

- [ ] **Step 2: Choose the persistence path**

  Use one of these two exact options:

  ```bash
  git add -f docs/superpowers/specs/2026-06-09-awf-subworkflows-design.md docs/superpowers/plans/2026-06-09-awf-subworkflows.md
  ```

  or move both files to a tracked planning location chosen by the maintainer before implementation starts.

- [ ] **Step 3: Verify staged visibility**

  ```bash
  git status --short
  ```

  Expected: the chosen plan/spec files are visible as staged or moved files, not hidden by ignore rules.

## Task 1: Documentation Contract

**Files:**
- Modify: `man/awf-workflow.5.md`
- Modify: `docs/runtime-design.md`
- Modify: `README.md` only if contradicted by the new contract

- [ ] **Step 1: Add failing docs grep check manually**

  Run:

  ```bash
  rg -n "imports:|call:|output_schema|output_files|call\\.started|workflow\\." man/awf-workflow.5.md docs/runtime-design.md
  ```

  Expected before implementation: no subworkflow-specific contract text beyond unrelated existing `output_schema`/`output_files` mentions.

- [ ] **Step 2: Update `man/awf-workflow.5.md`**

  Add sections named `Imports`, `Call Steps`, and `Workflow Exports`.

  The `Imports` section must state:

  ```text
  `imports:` maps local import ids to relative `.awf.yaml` files. Import paths
  are resolved relative to the declaring workflow file, must stay within that
  module directory, are interpreted as slash paths, and must not traverse symlink
  path components. Remote imports, absolute paths, backslashes, control characters,
  and `..` escape are not supported.
  ```

  The `Call Steps` section must include this example:

  ```yaml
  - id: recon_result
    call: recon
    input:
      target: "{{ input.target }}"
  ```

  It must state:

  ```text
  The call product is addressable as `step.recon_result.<field>` and
  `step.recon_result.files.<name>`. Imported internals are private for references.
  ```

  The `Workflow Exports` section must state that imported workflows return explicit
  typed outputs and named artifact aliases, and include this example:

  ```yaml
  output_schema:
    type: object
    required: [summary]
    additionalProperties: false
    properties:
      summary: { type: string }
  outputs:
    summary: "{{ step.final.summary }}"
  output_files:
    report: step.final.files.report
  ```

- [ ] **Step 3: Update `docs/runtime-design.md`**

  Add the runtime details:

  ```markdown
  `call.started{input_ref,runtimes}` is appended and fsynced before a child
  workflow graph executes. Fold materializes `RunState.CallStarted[path]` so
  resume reuses the exact call input and checks call-level runtime drift before
  replaying the child frontier.

  Child workflow nodes execute under `<call-id>.workflow.<child-path>`.
  The call node commits at `<call-id>` with typed outputs and file aliases.
  ```

- [ ] **Step 4: Verify docs text exists**

  Run:

  ```bash
  rg -n "call\\.started|imports:|Call Steps|Workflow Exports|<call-id>\\.workflow" man/awf-workflow.5.md docs/runtime-design.md
  ```

  Expected: each term appears in the relevant docs.

- [ ] **Step 5: Commit**

  ```bash
  git status --short
  git add man/awf-workflow.5.md docs/runtime-design.md
  if git status --short -- README.md | rg -q .; then
    git add README.md
  fi
  git commit -m "docs: define awf subworkflows"
  ```

## Task 2: IR Types, Node Shape, And Path Helpers

**Files:**
- Modify: `ir/types.go`
- Modify: `ir/node.go`
- Modify: `ir/node_unmarshal.go`
- Modify: `ir/node_marshal.go`
- Modify: `ir/walk.go`
- Modify: `ir/path.go`
- Modify: `ir/node_test.go`
- Modify: `ir/tags_test.go`
- Add: `ir/path_test.go`
- Modify: `frontend/yaml/yaml_test.go`

- [ ] **Step 1: Write failing `CallStep` decode/marshal tests**

  Add tests equivalent to:

  ```go
  func TestCallStepDecode(t *testing.T) {
      raw := []byte(`{
        "workflow":"root",
        "version":1,
        "imports":{"recon":"workflows/recon.awf.yaml"},
        "containers":{},
        "graph":[{"id":"run_recon","call":"recon","input":{"target":"{{ input.target }}"}}]
      }`)
      var wf Workflow
      if err := json.Unmarshal(raw, &wf); err != nil {
          t.Fatal(err)
      }
      cs, ok := wf.Graph[0].(*CallStep)
      if !ok {
          t.Fatalf("Graph[0] = %T, want *CallStep", wf.Graph[0])
      }
      if cs.ID != "run_recon" || cs.Call != "recon" {
          t.Fatalf("CallStep = %+v", cs)
      }
      if got := string(cs.Input["target"]); got != `"{{ input.target }}"` {
          t.Fatalf("Input[target] = %q", got)
      }
  }
  ```

  Add a YAML decode test that proves top-level workflow `output_files` and step
  `output_files` use different IR fields:

  ```go
  func TestDecodeWorkflowArtifactExportsAndStepOutputFiles(t *testing.T) {
      wf, err := Decode([]byte(`
  workflow: exports
  version: 1
  containers:
    lab: {image: alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000}
  output_files:
    report: step.final.files.report
  graph:
    - id: final
      container: lab
      run: ./final
      output_files:
        report:
          path: /out/report.jsonl
          format: jsonl
          schema_ref: asset.row_schema
  `))
      if err != nil {
          t.Fatal(err)
      }
      if got := wf.ArtifactExports["report"]; got != "step.final.files.report" {
          t.Fatalf("ArtifactExports[report] = %q", got)
      }
      step := wf.Graph[0].(*ir.CodeStep)
      if len(step.OutputFiles) != 1 || step.OutputFiles[0].SchemaRef != "asset.row_schema" {
          t.Fatalf("step OutputFiles = %#v", step.OutputFiles)
      }
  }
  ```

- [ ] **Step 2: Run red test**

  ```bash
  go test ./ir -run 'TestCallStepDecode|TestNodeRegistryExhaustive|TestAllExportedFieldsHaveJSONTags'
  ```

  Expected: fail because `CallStep` and `imports` fields do not exist or are not decoded.

- [ ] **Step 3: Add IR fields and node type**

  Implement:

  ```go
  type Workflow struct {
      ID              string                   `json:"workflow"`
      Version         int                      `json:"version"`
      Input           *JSONSchema              `json:"input,omitempty"`
      Env             []string                 `json:"env,omitempty"`
      Assets          map[string]string        `json:"assets,omitempty"`
      Imports         map[string]string        `json:"imports,omitempty"`
      Agents          map[string]AgentRole     `json:"agents,omitempty"`
      Containers      map[string]Container     `json:"containers"`
      OutputSchema    *JSONSchema              `json:"output_schema,omitempty"`
      Outputs         map[string]TemplateValue `json:"outputs,omitempty"`
      ArtifactExports ArtifactExports          `json:"output_files,omitempty"`
      Graph           NodeList                 `json:"graph"`
      Digest          string                   `json:"-"`
  }

  type TemplateValue = json.RawMessage
  type ArtifactExports map[string]string

  type CallStep struct {
      ID    string                   `json:"id"`
      Call  string                   `json:"call"`
      Input map[string]TemplateValue `json:"input,omitempty"`
  }

  func (*CallStep) isNode() {}
  ```

- [ ] **Step 4: Register `call` in node marshal/unmarshal**

  Add `call` to the step discriminator table and marshal as the flat step object.
  Keep `with:` untouched; `call.input` is not `RawConfig`.

- [ ] **Step 5: Add path helper**

  In `ir/path.go`, add:

  ```go
  const CallWorkflowSegment = "workflow"

  func CallWorkflowParentPath(callPath string) string {
      if callPath == "" {
          return CallWorkflowSegment
      }
      return callPath + "." + CallWorkflowSegment
  }
  ```

  This helper is only for static validation and graph projection. Runtime execution
  must use `engine.CallWorkflowRuntimePath`; runtime container and snapshot keys
  also belong in `engine`.

- [ ] **Step 6: Update walkers**

  Add `CallStep` as a leaf step in `ir.WalkNodes`. It has no inline child graph; children live in imported modules.

- [ ] **Step 7: Run focused tests**

  ```bash
  go test ./ir -run 'CallStep|NodeRegistry|JSONTags|Path'
  ```

  Expected: pass.

- [ ] **Step 8: Commit**

  ```bash
  git add ir
  git commit -m "ir: add call steps and workflow exports"
  ```

## Task 3: Loaded Modules And Recursive Loader

**Files:**
- Modify: `ir/loaded.go`
- Modify: `loader/loader.go`
- Add: `loader/errors.go`
- Add: `loader/errors_test.go`
- Add: `loader/safepath.go`
- Add: `loader/safepath_test.go`
- Add: `loader/imports.go`
- Add: `loader/imports_test.go`
- Modify: `loader/loader_test.go` when existing loader fixtures need module-aware assertions.

- [ ] **Step 1: Write failing loader tests**

  Add these tests:

  - `TestSafeRootRelPathNormalizesDotSlash`: `./lab/compose.yml` returns manifest path `lab/compose.yml` and a local OS path from `filepath.Localize`.
  - `TestSafeRootRelPathRejectsPathEscape`: `../bad.awf.yaml` fails before any filesystem open.
  - `TestSafeRootRelPathRejectsBackslashAndControls`: `workflows\\child.awf.yaml`, embedded NUL, tab, CR, and LF all fail.
  - `TestSafeRootRelPathAllowsDotOnlyWhenPolicyAllowsDot`: `.` is rejected for imports and allowed only for the existing asset policy that intentionally snapshots the module root.
  - `TestImportRelPathRequiresAWFSuffix`: `child.yaml` fails with diagnostic code `AWF_IMPORT_PATH_INVALID`.
  - `TestLoadImportsWorkflowRelativeToDeclaringModule`: create root and child workflow files under a temp dir, declare `imports.recon: workflows/recon.awf.yaml`, load the root, and assert `LoadedDefinition.Module("recon")` exists with `WorkflowPath` ending in `workflows/recon.awf.yaml`.
  - `TestLoadImportRejectsPathEscape`: declare `imports.bad: ../bad.awf.yaml`, use `errors.As(err, &loadErr)`, and assert `loadErr.Code == "AWF_IMPORT_PATH_ESCAPE"`.
  - `TestLoadImportRejectsSymlinkComponent`: create a symlink inside the workflow directory that points outside it, import through that symlink, use `errors.As(err, &loadErr)`, and assert `loadErr.Code == "AWF_IMPORT_SYMLINK"`.
  - `TestLoadImportRejectsCycle`: make `a.awf.yaml` import `b.awf.yaml` and `b.awf.yaml` import `a.awf.yaml`; use `errors.As(err, &loadErr)` and assert `loadErr.Code == "AWF_IMPORT_CYCLE"`.
  - `TestLoadNestedImports`: root imports `outer`, `outer` imports `inner`, and `LoadedDefinition.WalkModules` visits root, `outer`, and `outer.inner` in stable sorted order.
  - `TestLoadSameFileImportedTwiceCreatesTwoLogicalModules`: root imports the same file as `recon_a` and `recon_b`; assert both module ids exist as separate logical `LoadedModule` records with separate import edges.
  - `TestLoadImportRejectsInvalidImportID`: declare import ids `outer.inner` and `workflow`; use `errors.As(err, &loadErr)` and assert `loadErr.Code == "AWF_IMPORT_ID_INVALID"`. Import ID syntax is a loader concern because dotted IDs and reserved IDs break module address construction before validation can safely run.

  The first fixture should use:

  ```text
  root.awf.yaml
  workflows/recon.awf.yaml
  workflows/assets/prompt.txt
  ```

  and assert imported assets resolve relative to `workflows/`, not root.

- [ ] **Step 2: Run red loader tests**

  ```bash
  go test ./loader -run 'SafeRootRelPath|Import|Nested'
  ```

  Expected: fail because imports are not loaded.

- [ ] **Step 3: Add `LoadedModule`**

  In `ir/loaded.go`, add module structure and helpers:

  ```go
  type LoadedModule struct {
      ID           string
      Workflow     *Workflow
      WorkflowPath string
      ComposeFiles map[string][]byte
      Assets       map[string]LoadedAsset
  }

  type LoadedImportEdge struct {
      ParentID     string
      ImportID     string
      DeclaredPath string
      ChildID      string
  }

  func (ld *LoadedDefinition) Root() *LoadedModule
  func (ld *LoadedDefinition) Module(id string) (*LoadedModule, bool)
  func (ld *LoadedDefinition) WalkModules(fn func(*LoadedModule) error) error
  func (ld *LoadedDefinition) WalkImportEdges(fn func(LoadedImportEdge) error) error
  ```

  Preserve `LoadedDefinition.Workflow`, `WorkflowPath`, `ComposeFiles`, and `Assets` as root aliases. Keep `LoadedImportEdge.DeclaredPath` as the normalized manifest path from the declaring module, not an absolute filesystem path.

- [ ] **Step 4: Add typed loader errors**

  Add `loader/errors.go`:

  ```go
  type LoadError struct {
      Code    string
      Source  string
      Path    string
      Message string
      Err     error
  }

  func (e *LoadError) Error() string {
      if e == nil {
          return "<nil>"
      }
      if e.Source != "" {
          return e.Source + ": " + e.Message
      }
      return e.Message
  }

  func (e *LoadError) Unwrap() error {
      if e == nil {
          return nil
      }
      return e.Err
  }
  ```

  Add the internal safe-path error shape in `loader/safepath.go`:

  ```go
  type safePathError struct {
      Code    string
      Message string
      Err     error
  }

  func (e *safePathError) Error() string {
      if e == nil {
          return "<nil>"
      }
      return e.Message
  }

  func (e *safePathError) Unwrap() error {
      if e == nil {
          return nil
      }
      return e.Err
  }
  ```

  Loader tests must inspect loader failures with `errors.As(err, &loadErr)` and
  assert `loadErr.Code`. Do not make `loader.Load` return `[]ir.Diagnostic`;
  validation owns diagnostics after a `LoadedDefinition` exists.

- [ ] **Step 5: Implement shared loader safe-path normalization**

  Add `loader/safepath.go`. This is the single helper for authored manifest paths used by imports, compose files, and assets; do not add another ad hoc cleaner in `imports.go` or `assets.go`.

  ```go
  type safePathPolicy struct {
      Kind           string
      AllowDot       bool
      RequiredSuffix string
      EmptyCode      string
      AbsoluteCode   string
      BackslashCode  string
      ControlCode    string
      DotCode        string
      EscapeCode     string
      InvalidCode    string
      SuffixCode     string
      LocalizeCode   string
  }

  type safeRelPath struct {
      Manifest string
      Local    string
  }

  func cleanRootRelPath(declared string, policy safePathPolicy) (safeRelPath, error) {
      if declared == "" {
          return safeRelPath{}, &safePathError{Code: policy.EmptyCode, Message: fmt.Sprintf("%s path is empty", policy.Kind)}
      }
      if filepath.IsAbs(declared) {
          return safeRelPath{}, &safePathError{Code: policy.AbsoluteCode, Message: fmt.Sprintf("%s path must be relative", policy.Kind)}
      }
      if strings.ContainsRune(declared, '\\') {
          return safeRelPath{}, &safePathError{Code: policy.BackslashCode, Message: fmt.Sprintf("%s path must use forward slashes", policy.Kind)}
      }
      if strings.IndexFunc(declared, unicode.IsControl) >= 0 {
          return safeRelPath{}, &safePathError{Code: policy.ControlCode, Message: fmt.Sprintf("%s path contains control characters", policy.Kind)}
      }
      clean := path.Clean(declared)
      if clean == "." && !policy.AllowDot {
          return safeRelPath{}, &safePathError{Code: policy.DotCode, Message: fmt.Sprintf("%s path must name a file or directory", policy.Kind)}
      }
      if clean == ".." || strings.HasPrefix(clean, "../") {
          return safeRelPath{}, &safePathError{Code: policy.EscapeCode, Message: fmt.Sprintf("%s path escapes the workflow directory", policy.Kind)}
      }
      if !fs.ValidPath(clean) {
          return safeRelPath{}, &safePathError{Code: policy.InvalidCode, Message: fmt.Sprintf("%s path is not a valid slash path", policy.Kind)}
      }
      if policy.RequiredSuffix != "" && !strings.HasSuffix(clean, policy.RequiredSuffix) {
          return safeRelPath{}, &safePathError{Code: policy.SuffixCode, Message: fmt.Sprintf("%s path %q must end in %s", policy.Kind, clean, policy.RequiredSuffix)}
      }
      local, err := filepath.Localize(clean)
      if err != nil {
          return safeRelPath{}, &safePathError{Code: policy.LocalizeCode, Message: fmt.Sprintf("%s path cannot be localized", policy.Kind), Err: err}
      }
      if !filepath.IsLocal(local) {
          return safeRelPath{}, &safePathError{Code: policy.LocalizeCode, Message: fmt.Sprintf("%s path is not local after localization", policy.Kind)}
      }
      return safeRelPath{Manifest: clean, Local: local}, nil
  }

  func importRelPath(declared string) (safeRelPath, error) {
      return cleanRootRelPath(declared, safePathPolicy{
          Kind:           "import",
          RequiredSuffix: ".awf.yaml",
          EmptyCode:      "AWF_IMPORT_PATH_INVALID",
          AbsoluteCode:   "AWF_IMPORT_PATH_ABSOLUTE",
          BackslashCode:  "AWF_IMPORT_PATH_BACKSLASH",
          ControlCode:    "AWF_IMPORT_PATH_INVALID",
          DotCode:        "AWF_IMPORT_PATH_INVALID",
          EscapeCode:     "AWF_IMPORT_PATH_ESCAPE",
          InvalidCode:    "AWF_IMPORT_PATH_INVALID",
          SuffixCode:     "AWF_IMPORT_PATH_INVALID",
          LocalizeCode:   "AWF_IMPORT_PATH_INVALID",
      })
  }
  ```

  Import `fmt`, `io/fs`, `path`, `path/filepath`, `strings`, and `unicode`. Use `path.Clean`, not `filepath.Clean`, because workflow manifests are slash-path documents. Open the returned `Local` path through `os.Root`; the helper is normalization, not the containment boundary.

- [ ] **Step 6: Refactor loader around per-module root**

  Extract a `loadModule` helper:

  ```go
  func loadModule(absPath, moduleID string, stack []string, modules map[string]*ir.LoadedModule) (*ir.LoadedModule, error)
  ```

  Each call opens `os.OpenRoot(filepath.Dir(absPath))`, reads the workflow source,
  decodes YAML, reads compose files and assets relative to that module root through
  `cleanRootRelPath`, then recursively loads `wf.Imports` through `importRelPath`.
  Module ids are dotted import addresses (`""`, `recon`, `outer.inner`), not absolute
  file identities. Use the absolute path stack only for cycle detection. Validate every
  import map key before resolving its path: ids must match the same step-id identifier
  grammar, must not contain `.`, and must not equal the reserved `workflow` segment.
  Return `AWF_IMPORT_ID_INVALID` from the loader when an import key violates this rule.

  When `importRelPath` returns `*safePathError`, wrap it as:

  ```go
  var pathErr *safePathError
  if errors.As(err, &pathErr) {
      return nil, &LoadError{
          Code:    pathErr.Code,
          Source:  module.WorkflowPath,
          Path:    "imports." + importID,
          Message: pathErr.Message,
          Err:     err,
      }
  }
  ```

  Map loader failures to these exact codes:

  - invalid import id: `AWF_IMPORT_ID_INVALID`
  - safe path failures: the `safePathError.Code` returned by `importRelPath`, including `AWF_IMPORT_PATH_INVALID`, `AWF_IMPORT_PATH_ABSOLUTE`, `AWF_IMPORT_PATH_BACKSLASH`, and `AWF_IMPORT_PATH_ESCAPE`
  - symlink component: `AWF_IMPORT_SYMLINK`
  - cycle: `AWF_IMPORT_CYCLE`
  - nesting deeper than `maxImportDepth`: `AWF_IMPORT_DEPTH`
  - filesystem read/open/stat failure: `AWF_IMPORT_READ`
  - YAML decode failure: `AWF_IMPORT_DECODE`

- [ ] **Step 7: Detect cycles and depth**

  Use constants:

  ```go
  const maxImportDepth = 10
  ```

  Track the absolute workflow path stack. If the next absolute path already appears
  in the stack, return an import cycle error with the cycle path. If `len(stack)` would
  exceed `maxImportDepth`, return `AWF_IMPORT_DEPTH` before reading the next file.

- [ ] **Step 8: Run loader tests**

  ```bash
  go test ./loader
  ```

  Expected: pass.

- [ ] **Step 9: Commit**

  ```bash
  git add ir/loaded.go loader
  git commit -m "loader: load local workflow imports"
  ```

## Task 4: Digest Frames For Modules

**Files:**
- Modify: `ir/digest.go`
- Modify: `ir/digest_test.go`
- Modify: `cli/run.go`
- Modify: `cli/resume.go`
- Modify: `cli/validate.go`
- Modify: `cli/ui.go`
- Modify call sites in tests and docs if signature changes

- [ ] **Step 1: Write digest tests**

  Add these tests:

  - `TestDigestFoldsImportedWorkflowCanonicalIR`: mutate an imported workflow semantic field and assert the root digest changes.
  - `TestDigestIgnoresImportedWorkflowCommentOnlyChange`: change only imported YAML comments or formatting and assert the digest is unchanged.
  - `TestDigestFoldsImportedAssets`: mutate only an imported workflow asset and assert the root digest changes.
  - `TestDigestFoldsImportedComposeBytes`: mutate only an imported workflow compose file and assert the root digest changes.
  - `TestDigestFoldsImportIDAndNormalizedImportPath`: import the same child as `recon` and `scan`, then with two different normalized declared paths to equivalent files, and assert the digest changes when the logical import edge changes.
  - `TestDigestUsesNoAbsoluteWorkflowPaths`: load the same fixture from two temp directory roots and assert `ld.ComputeDigest()` returns the same digest.
  - `TestDigestUnchangedForWorkflowWithoutImports`: load a legacy workflow fixture and assert the digest matches the previous digest golden.

- [ ] **Step 2: Run red tests**

  ```bash
  go test ./ir -run 'Digest.*Import|DigestUnchanged'
  ```

  Expected: fail because imported module frames are not folded.

- [ ] **Step 3: Add loaded-definition digest helper**

  Prefer adding a method on `LoadedDefinition`:

  ```go
  func (ld *LoadedDefinition) ComputeDigest() (string, error)
  ```

  If `ld` has no imported modules and no import edges, return the existing root workflow
  digest exactly:

  ```go
  return ld.Workflow.ComputeDigest(ld.ComposeFiles, ld.Assets)
  ```

  Otherwise, compute the loaded-definition digest with existing `writeDigestFrame` framing:

  ```text
  loaded-definition-v1
  root
  <root workflow digest>
  module
  <module id>
  <module workflow digest>
  import-edge
  <parent module id>
  <import id>
  <normalized declared path>
  <child module id>
  ```

  Fold module frames sorted by module id, excluding the root. Fold import-edge frames
  sorted by `(ParentID, ImportID, ChildID)`. Each module workflow digest is produced by
  `module.Workflow.ComputeDigest(module.ComposeFiles, module.Assets)`. Do not fold raw
  YAML bytes, absolute workflow paths, absolute module directories, or OS-local path
  separators into the digest.

- [ ] **Step 4: Keep old workflow method for unit tests**

  Leave:

  ```go
  func (w *Workflow) ComputeDigest(composeFiles map[string][]byte, assets map[string]LoadedAsset) (string, error)
  ```

  Existing tests that construct a single workflow should not be forced to build modules.

- [ ] **Step 5: Update CLI call sites**

  Replace production calls to `ld.Workflow.ComputeDigest(ld.ComposeFiles, ld.Assets)` with `ld.ComputeDigest()` in `cli/run.go`, `cli/resume.go`, `cli/validate.go`, and `cli/ui.go`. The old workflow method remains available for tests that intentionally construct a single workflow.

  Verify with:

  ```bash
  rg -n 'ComputeDigest\(' cli engine ir loader
  ```

  Expected production result: CLI code calls `ld.ComputeDigest()`. Remaining direct `Workflow.ComputeDigest` calls are in `ir/digest.go`, `ir/digest_test.go`, or focused tests that intentionally bypass the loader.

- [ ] **Step 6: Run digest and CLI tests**

  ```bash
  go test ./ir ./cli -run 'Digest|Validate|Run|Resume'
  ```

  Expected: pass.

- [ ] **Step 7: Commit**

  ```bash
  git add ir/digest.go ir/digest_test.go cli
  git commit -m "ir: fold imported workflows into digest"
  ```

## Task 5: Validation And Diagnostic Source Attribution

**Files:**
- Modify: `ir/diagnostic.go`
- Modify: `ir/validate.go`
- Modify: `ir/validate_structural.go`
- Modify: `ir/validate_refs.go`
- Modify: `ir/validate_input_files.go`
- Modify: `ir/validate_output_files.go`
- Modify: `ir/validate_schema.go`
- Modify: `cli/validate.go`
- Add focused tests under `ir` and `cli`

- [ ] **Step 1: Add failing validation tests**

  Add tests for:

  ```go
  TestValidateUnknownCallTarget
  TestValidateCallInputAgainstChildSchema
  TestValidateRejectsParentRefInsideChildExport
  TestValidateCallProducerRefsUseChildExportSchema
  TestValidateCallArtifactRefsUseChildExportFiles
    TestValidateChildSchemaRefUsesChildModuleAssets
    TestValidateArtifactOnlyChildWorkflowCanBeCalledForFileRef
    TestValidateDiagnosticSourceForImportedWorkflow
    TestValidateRejectsDuplicateStepAndAggregateProductIDs
    ```

- [ ] **Step 2: Run red tests**

  ```bash
  go test ./ir ./cli -run 'Call|Import|DiagnosticSource'
  ```

  Expected: fail.

- [ ] **Step 3: Add diagnostic source**

  Update:

  ```go
  type Diagnostic struct {
      Severity Severity
      Source   string `json:",omitempty"`
      Path     string
      Code     string
      Message  string
  }
  ```

  Update `Diagnostic.String()` to render:

  ```text
  error AWF10xx at workflows/recon.awf.yaml:graph[0]: message
  ```

  when `Source` is non-empty.

- [ ] **Step 4: Add import/call diagnostic codes**

  Add catalog entries for import path errors, import cycles, unknown call targets,
  call input contract errors, child export contract errors, and invalid artifact exports.
  Keep codes sorted in `ir/diagnostic.go`.

- [ ] **Step 5: Validate modules**

  Update `Validate(ld)` so it validates every loaded module. For module diagnostics,
  set `Source` to the module workflow path or a stable path relative to the root if
  existing validate output should stay portable.

  Use an internal module validation context instead of reading root fields inside
  each pass:

  ```go
  type validationModule struct {
      ModuleID string
      Workflow *Workflow
      Assets   map[string]LoadedAsset
      Source   string
  }

  func validationModules(ld *LoadedDefinition) []validationModule
  ```

  Update `validateInputFiles`, `validateOutputFiles`, and schema-ref validation to
  take `validationModule`. In particular, `schema_ref: asset.<id>` must validate
  against `mod.Workflow.Assets` and `mod.Assets`, not `ld.Workflow.Assets` and
  `ld.Assets`, when the producer lives in an imported module.

- [ ] **Step 6: Validate calls**

    Add a pass that:

    - finds each `CallStep`
    - checks target import exists
    - rejects duplicate producer ids in each module, including step ids, call ids, map/reduce aggregate product ids, and any existing body-step reduce aliases kept for backward compatibility
    - checks child workflow export contract exists when a parent-visible typed or file ref requires one
  - indexes the call as a producer in the parent using child `output_schema` and workflow `ArtifactExports`
  - treats artifact-only child workflows as valid file producers without requiring `output_schema`
  - requires `output_schema` only when child `outputs` is non-empty or the parent uses `step.<call-id>.<field>` typed refs
  - checks `call.input` references against parent scope
  - checks child `outputs` references against child-local scope only
  - checks workflow `ArtifactExports` refs are static child-local `step.<id>.files.<name>` refs that point to committed-child producer contracts

- [ ] **Step 7: Update CLI validate output**

  `--format json` should include `Source` when set. Text output uses `Diagnostic.String()`.

  When `loader.Load` returns `*loader.LoadError`, convert it before printing:

  ```go
  var loadErr *loader.LoadError
  if errors.As(err, &loadErr) {
      diag := ir.Diagnostic{
          Severity: ir.Error,
          Source:   loadErr.Source,
          Path:     loadErr.Path,
          Code:     loadErr.Code,
          Message:  loadErr.Message,
      }
      // print through the same text/json path as validation diagnostics
  }
  ```

- [ ] **Step 8: Run validation tests**

  ```bash
  go test ./ir ./cli -run 'Validate|Call|Import|Diagnostic'
  ```

  Expected: pass.

- [ ] **Step 9: Commit**

  ```bash
  git add ir cli/validate.go
  git commit -m "ir: validate workflow imports and calls"
  ```

## Task 6: Typed Call Input Evaluation

**Files:**
- Add: `template/value.go`
- Add: `template/value_test.go`
- Modify: `engine/scope.go`

- [ ] **Step 1: Write typed value tests**

  Add these tests:

  - `TestEvalTemplateValuePreservesWholeRefObject`: raw JSON string `"{{ step.seed.obj }}"` returns the object value, not JSON-encoded text.
  - `TestEvalTemplateValuePreservesArray`: raw JSON string `"{{ step.seed.items }}"` returns the array value.
  - `TestEvalTemplateValueSubstitutesStringWithInlineRef`: raw JSON string `"scan {{ input.target }}"` returns a string with the scalar substituted.
  - `TestEvalTemplateValueRecursesIntoObject`: raw JSON object `{"target":"{{ input.target }}","items":"{{ step.seed.items }}"}` returns a typed object with string and array fields.
  - `TestEvalTemplateValueRejectsOversize`: an evaluated JSON value larger than the configured input limit returns a mechanical error before `call.started`.

  Whole-ref example:

  ```go
  got, err := template.EvalTemplateValue(json.RawMessage(`"{{ step.seed.items }}"`), scope)
  ```

  should return the actual `[]any`, not a JSON string.

- [ ] **Step 2: Run red tests**

  ```bash
  go test ./template -run 'EvalTemplateValue'
  ```

  Expected: fail because helper does not exist.

- [ ] **Step 3: Implement `EvalTemplateValue`**

  Implement:

  ```go
  func EvalTemplateValue(raw json.RawMessage, scope Scope) (any, error)
  ```

  Rules:

  - decode `raw` as JSON before evaluation
  - string values containing exactly one full `{{ ref }}` return the resolved typed value
  - string values with mixed text use existing string substitution
    - arrays and maps recurse into values and preserve structure
    - numbers, bools, and nil pass through unchanged
    - existing max inline byte checks still apply
    - whole-ref detection reuses the existing template scanner/parser machinery; do not add a second ad hoc `{{ }}` grammar with `strings.Contains`, `strings.TrimPrefix`, or similar checks. If current scanner helpers are unexported in a way that blocks reuse, extract a narrow unexported helper inside `template` and call it from both string substitution and typed value evaluation.

- [ ] **Step 4: Add child input scope support**

  Update `engine.Scope` so it can carry an input override:

  ```go
  func NewScopeWithInput(rs *RunState, wf *ir.Workflow, ctxPath string, input map[string]any) *Scope
  ```

  `resolveInput` reads the override when present, else root `rs.Input`.

- [ ] **Step 5: Run tests**

  ```bash
  go test ./template ./engine -run 'EvalTemplateValue|ScopeResolveInput'
  ```

  Expected: pass.

- [ ] **Step 6: Commit**

  ```bash
  git add template engine/scope.go engine/scope_test.go
  git commit -m "template: evaluate typed call input values"
  ```

## Task 7: Call Started Event And Fold State

**Files:**
- Modify: `engine/events.go`
- Modify: `engine/fold.go`
- Modify: `engine/runstate.go`
- Add tests in `engine/events_test.go`, `engine/fold_test.go`, `engine/runstate_test.go`

- [ ] **Step 1: Write event/fold tests**

  Add these tests:

    - `TestCallStartedDataRoundTrip`: marshal and unmarshal `CallStartedData` and assert `InputRef` and `Runtimes` survive. `WorkflowDigest` must not be present because root `run.started.workflow_digest` already pins imported workflow canonical IR, compose bytes, and asset bytes.
    - `TestFoldCallStartedMaterializesInputAndRuntimes`: fold a log containing `run.started` and `call.started`; assert `RunState.CallStarted["scan"]` contains the input blob ref and runtime versions.
    - `TestFoldCallStartedMissingInputBlobIsError`: fold a log where `call.started.input_ref` points at a missing blob and assert fold returns an error.
    - `TestFoldDuplicateCallStartedFails`: fold a log containing two `call.started` events for the same path and assert fold returns a corruption error instead of keeping either record.

- [ ] **Step 2: Run red tests**

  ```bash
  go test ./engine -run 'CallStarted'
  ```

  Expected: fail.

- [ ] **Step 3: Add event type and payload**

  Implement:

  ```go
  const EventCallStarted = "call.started"

  type CallStartedData struct {
      InputRef string `json:"input_ref"`
      Runtimes []ResolvedRuntime `json:"runtimes,omitempty"`
  }
  ```

- [ ] **Step 4: Add RunState fold state**

  Implement:

  ```go
  type CallStartedRecord struct {
      Input    map[string]any
      InputRef string
      Runtimes []ResolvedRuntime
  }
  ```

  Add `CallStarted map[string]CallStartedRecord` plus accessor methods.

- [ ] **Step 5: Fold event**

    In `Fold`, reject duplicate `call.started` records for the same event path before
    storing anything for that path. Read `InputRef` from `Blobs`, fail hard if the blob is
    missing, unmarshal into `map[string]any`, and store the input/ref/runtimes by event path.

- [ ] **Step 6: Run tests**

  ```bash
  go test ./engine -run 'CallStarted|Fold'
  ```

  Expected: pass.

- [ ] **Step 7: Commit**

  ```bash
  git add engine/events.go engine/fold.go engine/runstate.go engine/*test.go
  git commit -m "engine: fold call started events"
  ```

## Task 8: Workflow Export Evaluation

**Files:**
- Add: `engine/workflow_exports.go`
- Add: `engine/workflow_exports_test.go`
- Add: `engine/call_commit.go`
- Add: `engine/call_commit_test.go`
- Modify: `engine/commit.go`
- Modify: `engine/signal_step.go`
- Modify: `engine/artifact_scope.go`
- Modify: `ir/validate_output_files.go`

- [ ] **Step 1: Write export tests**

  Add these tests:

  - `TestEvaluateWorkflowOutputsAgainstChildScope`: child workflow `outputs.summary: "{{ step.summarize.summary }}"` evaluates from child steps under `<call>.workflow.*`.
  - `TestEvaluateWorkflowOutputsRejectsParentStepRef`: child workflow `outputs` referencing a parent-only step id fails validation.
  - `TestEvaluateWorkflowOutputFilesAliasChildArtifacts`: child workflow `output_files.report` aliases a committed child artifact without recapturing container bytes.
  - `TestValidateWorkflowArtifactExportRejectsDynamicRef`: workflow `output_files.report: "{{ step.final.files.report }}"` is rejected because artifact exports must be static refs.
  - `TestValidateWorkflowArtifactExportRejectsMissingChildArtifact`: workflow `output_files.report: step.final.files.missing` is rejected against child-local producer contracts.
  - `TestResolveCallArtifactRefByExportName`: parent `step.recon.files.report` resolves from the call node's committed export-name file map.
  - `TestEvaluateWorkflowOutputsSchemaFailure`: export evaluation returns a mechanical failure when exported JSON does not satisfy workflow `output_schema`.
  - `TestCommitCallProductRejectsMissingExportedFileRef`: an exported file CAS ref missing from `Blobs` returns an error and does not append call `node.completed`.
  - `TestNodeCompletedAppendAllowlist`: scan `engine/*.go` and assert the only file that constructs `state.Event{Type: EventNodeCompleted` is `engine/commit.go`; `engine/signal_step.go` and `engine/call_commit.go` must call `appendNodeCompleted` instead of appending directly.

- [ ] **Step 2: Run red tests**

  ```bash
  go test ./engine -run 'WorkflowOutput|WorkflowExport'
  ```

  Expected: fail.

- [ ] **Step 3: Implement export evaluator**

  Add:

  ```go
  type WorkflowExportResult struct {
      Outputs map[string]any
      Files   map[string]string // exported file name -> CAS ref
  }
  ```

  Evaluate each `wf.Outputs` `TemplateValue` with `template.EvalTemplateValue` and validate the resulting object against `wf.OutputSchema`.
  Resolve `wf.ArtifactExports` as aliases to committed child artifact refs.

- [ ] **Step 4: Centralize `node.completed` append/sync**

  First update `engine/commit.go` with one unexported append helper:

  ```go
  func appendNodeCompleted(log state.Log, path string, data NodeCompletedData) error {
      dataJSON, err := json.Marshal(data)
      if err != nil {
          return fmt.Errorf("marshal node.completed for %q: %w", path, err)
      }
      if err := log.Append(state.Event{Type: EventNodeCompleted, Path: path, Data: dataJSON}); err != nil {
          return fmt.Errorf("append node.completed for %q: %w", path, err)
      }
      if err := log.Sync(); err != nil {
          return fmt.Errorf("sync node.completed for %q: %w", path, err)
      }
      return nil
  }
  ```

  Route both existing `Commit` and the signal-step half-commit path through
  `appendNodeCompleted`. This corrects the existing split where `engine/commit.go`
  documents one writer but `engine/signal_step.go` also appends `node.completed`
  directly.

- [ ] **Step 5: Add call product commit helper**

  Add `engine/call_commit.go` with:

  ```go
  func commitCallProduct(log state.Log, blobs state.Blobs, path string, result WorkflowExportResult) (NodeResult, error) {
      for name, ref := range result.Files {
          if _, err := blobs.Get(ref); err != nil {
              return NodeResult{}, fmt.Errorf("call export file %q ref %q is missing from blobs: %w", name, ref, err)
          }
      }
      nr := NodeResult{Outcome: OutcomeOK, Outputs: result.Outputs, Files: result.Files}
      if result.Outputs != nil {
          outBytes, err := json.Marshal(result.Outputs)
          if err != nil {
              return NodeResult{}, fmt.Errorf("commit call product %q: marshal outputs: %w", path, err)
          }
          ref, err := blobs.Put(outBytes)
          if err != nil {
              return NodeResult{}, fmt.Errorf("commit call product %q: put outputs: %w", path, err)
          }
          nr.OutputsRef = ref
      }
      if err := appendNodeCompleted(log, path, NodeCompletedData{
          Outcome:    string(OutcomeOK),
          OutputsRef: nr.OutputsRef,
          Files:      result.Files,
      }); err != nil {
          return NodeResult{}, fmt.Errorf("commit call product %q: %w", path, err)
      }
      return nr, nil
  }
  ```

  `commitCallProduct` is deliberately call-specific because existing `Commit` accepts
  captured file bytes, while workflow artifact exports already point at committed
  CAS refs. It must never call `log.Append` or `log.Sync` directly; all `node.completed`
  durability goes through `appendNodeCompleted`.

- [ ] **Step 6: Ensure no live container reads**

  Tests must assert the export evaluator takes only `RunState`, `Workflow`, child input,
  and `Blobs`; it must not call `Backend.CaptureFiles`.

- [ ] **Step 7: Resolve call product artifact refs by exported name**

  Existing step `NodeCompletedData.Files` maps declared container capture path to
  CAS ref, and existing resolvers map named step output files through the step's
  `OutputFiles` contract. Call products are different: `runCallStep` commits
  `NodeCompletedData.Files` keyed by exported artifact name. Update artifact
  resolution so `step.<call-id>.files.<name>` detects call producers and reads the
  committed export-name entry directly.

- [ ] **Step 8: Run tests**

  ```bash
  go test ./engine -run 'WorkflowOutput|WorkflowExport|Artifact'
  ```

  Expected: pass.

- [ ] **Step 9: Commit**

    ```bash
    git add engine/workflow_exports.go engine/workflow_exports_test.go engine/call_commit.go engine/call_commit_test.go engine/commit.go engine/signal_step.go engine/artifact_scope.go ir/validate_output_files.go
    git commit -m "engine: evaluate workflow exports"
    ```

## Task 9: CallStep Runtime

**Files:**
- Modify: `engine/interpreter.go`
- Add: `engine/interpreter_context.go`
- Add: `engine/call_step.go`
- Add: `engine/call_step_test.go`
- Modify: `engine/local_dispatcher.go`
- Add: `engine/runtime_handles.go`
- Add: `engine/runtime_resolution.go`
- Modify: `engine/path.go`
- Modify: `engine/commit.go`
- Modify: `engine/fold.go`
- Modify: `cli/runtimes.go`

- [ ] **Step 1: Write runtime tests**

  Add these tests:

  - `TestRunCallStepCommitsCallProduct`: executing a call appends `node.started{kind:"call"}`, commits child steps under `<call>.workflow.*`, and commits parent-visible `node.completed` at `<call>`.
  - `TestRunCallStepPersistsCallStartedBeforeChild`: force the child graph to fail immediately and assert the log still contains `call.started`.
  - `TestRunCallStepResumeAfterCallStartedSpanOnly`: seed a log with call `node.started` but no `call.started`; resume may append a second observational `node.started`, but must append exactly one durable `call.started` and proceed.
  - `TestRunCallStepResumeUsesRecordedInput`: mutate the checkout input template after `call.started`, resume, and assert the recorded input blob is used.
  - `TestRunCallStepChildFailureFailsCallBoundary`: a child mechanical failure first appends `node.failed` at the child path, then appends `node.failed` at the call path with an error that includes the child path.
  - `TestRunCallStepRepeatedCallsHaveIsolatedContainers`: two calls to the same import write different container state and do not see each other's files.
  - `TestRunCallStepNestedCallPaths`: root -> outer -> inner execution uses `outer.workflow.inner.workflow.<step>` paths.
  - `TestRunCallStepChildScopeHidesParentSteps`: child templates cannot resolve parent-only `step.*` refs unless values were passed through `call.input`.

- [ ] **Step 2: Run red tests**

  ```bash
  go test ./engine -run 'RunCallStep|CallStep'
  ```

  Expected: fail.

- [ ] **Step 3: Refactor interpreter arguments before adding call behavior**

  Add an unexported context struct and update `interpNodes`/`interpNode` to accept it.
  This is a no-behavior-change refactor with tests passing before `CallStep` logic is
  added.

  ```go
  type interpreterContext struct {
      def        *ir.LoadedDefinition
      moduleID   string
      wf         *ir.Workflow
      input      map[string]any
      runstate   *RunState
      dispatcher Dispatcher
      log        state.Log
      blobs      state.Blobs
      clk        clock.Clock
      tap        io.Writer
      broker     *signal.Broker
  }
  ```

  `input` is nil for the root workflow and set for child calls. The context struct is
  an internal parameter object, not a new interface.

- [ ] **Step 4: Dispatch CallStep**

  In `interpNode`, add:

  ```go
  case *ir.CallStep:
      return runCallStep(ctx, v, ir.PathFor(parent, "", v.ID, idx), ictx)
  ```

- [ ] **Step 5: Implement call-start path**

  In `runCallStep`:

    - return `OutcomeOK` if call path is completed
    - append `node.started{kind:"call"}` before `call.started` or child graph execution; if a previous resume fold saw only an observational `node.started`, treat the call as not started and continue
    - find child module from loaded definition
    - create child container handles before runtime resolution because `ResolveRuntimes` validates agent/container references against concrete handles
    - if `RunState.CallStarted[path]` exists, reuse its input and recorded runtimes, then resolve current runtimes against the freshly created handles and call `CheckRuntimesDrift(recorded, current)` before executing the child
    - otherwise evaluate typed call input, validate schema, store the input blob, resolve runtimes against the child handles, append `call.started`, and sync
    - if handle creation or runtime resolution fails before `call.started`, append a call-boundary `node.failed` and leave no `call.started` record behind

- [ ] **Step 6: Move runtime resolution into engine**

  Add `engine/runtime_resolution.go` with pure helpers adapted from `cli/runtimes.go`:

  ```go
  type RuntimeRef struct {
      ModuleID      string
      NodePath      string
      RuntimeParent string
      Uses          string
      Container     string
  }

  func WalkRuntimeRefs(moduleID, runtimeParent string, wf *ir.Workflow) []RuntimeRef
  func ResolveRuntimes(ctx context.Context, refs []RuntimeRef, resolver agent.Resolver, handles map[string]container.Handle) ([]ResolvedRuntime, error)
  func CheckRuntimesDrift(recorded, current []ResolvedRuntime) error
  ```

  `cli/runtimes.go` should either delete its private walker/resolver or keep thin
  wrappers that call these engine helpers. `ResolveRuntimes` stores
  `ResolvedRuntime.Container` as `QualifiedContainerKey(ref.RuntimeParent,
  ref.Container)` so call-level runtime pins cannot collide with root containers.
  Error messages include `ModuleID` and `NodePath` for imported workflow diagnostics.

- [ ] **Step 7: Implement per-call containers**

    For child workflow containers:

    - require `dispatcher.(*LocalDispatcher)`, matching `runMap` and `runCompose`
    - create child handles with qualified runtime container names before runtime resolution
    - clone the dispatcher with child handles, child compose bytes, and the same backend/resolver/tap settings
    - destroy child handles after child execution with existing teardown grace patterns in a `defer`, including early failures after handle creation
    - record snapshot refs with qualified keys

  Add one runtime key helper in `engine/path.go`:

  ```go
  const CallWorkflowSegment = "workflow"

  func CallWorkflowRuntimePath(callPath string) string {
      if callPath == "" {
          return CallWorkflowSegment
      }
      return callPath + "." + CallWorkflowSegment
  }

  func QualifiedContainerKey(runtimeParent, container string) string {
      if runtimeParent == "" {
          return container
      }
      return runtimeParent + "::" + container
  }
  ```

  Use these helpers for child graph execution, child container handles, and snapshot refs; do not build
  strings like `recon_result.workflow::lab` inline.

- [ ] **Step 8: Execute child graph with child input scope**

  Run:

  ```go
  childParent := CallWorkflowRuntimePath(path)
  childCtx := ictx
  childCtx.moduleID = child.ID
  childCtx.wf = child.Workflow
  childCtx.input = callInput
  childCtx.dispatcher = childDispatcher
  interpNodes(ctx, child.Workflow.Graph, childParent, childCtx)
  ```

  The child input must flow through `NewScopeWithInput`, not by mutating `runstate.Input`.

- [ ] **Step 9: Commit call product**

  After child graph success, evaluate workflow exports and commit a normal
  `node.completed` at the call path. Commit `NodeCompletedData.Files` keyed by
  exported file name, not child capture path, so parent refs resolve as
  `step.<call-id>.files.<export-name>`.

- [ ] **Step 10: Handle failure**

  If child execution fails, preserve child root-cause `node.failed`, then append
  call-boundary `node.failed` at the call path. The call-boundary error must include
  the failing child path so `awf trace` and `try/catch` can attribute the parent-visible failure.

- [ ] **Step 11: Run tests**

  ```bash
  go test ./engine -run 'RunCallStep|CallStep|Snapshot'
  ```

  Expected: pass.

- [ ] **Step 12: Commit**

  ```bash
  git add engine cli/runtimes.go
  git commit -m "engine: execute workflow call steps"
  ```

## Task 10: CLI Lifecycle Coverage

**Files:**
- Modify: `cli/run.go`
- Modify: `cli/resume.go`
- Modify: `cli/backend_features.go`
- Modify: `cli/runtimes.go`
- Modify: `cli/threaded_guard.go`
- Modify: `cli/ui.go`
- Add/modify: `engine/asset_keys.go`
- Modify: `engine/assets.go`
- Add/modify: `engine/assets_test.go`
- Modify tests under `cli`

- [ ] **Step 1: Write CLI tests**

  Add these tests:

  - `TestRunBackendAutoScansImportedWorkflows`: root has no Docker-only features, imported workflow has `runtime compose`; `awf run --backend auto` selects Docker.
  - `TestResumeDigestDriftFromImportedWorkflowFails`: start a run, make a semantic change to an imported workflow, resume, and assert pinning rejects drift before execution.
  - `TestResumeCallRuntimeDriftFails`: start a call using a runtime image digest, mutate the runtime resolution, resume, and assert the recorded call runtime check fails.
  - `TestThreadedGuardScansCallWorkflows`: `awf validate --threaded` rejects an unsafe threaded reference that crosses into a called workflow.
  - `TestRunStartedAssetsAreModuleQualified`: root and imported workflow both declare `asset.schema`; root is stored as `schema`, child is stored as `recon/schema`, and refs resolve in the current module scope.
  - `TestUIUsesLoadedDefinitionDigest`: `awf ui` or its digest helper calls `ld.ComputeDigest()` so imported module drift is visible in UI projections.

- [ ] **Step 2: Run red tests**

  ```bash
  go test ./cli -run 'Import|Call|RuntimeDrift|BackendAuto|Threaded'
  ```

  Expected: fail.

- [ ] **Step 3: Centralize module/call traversal use**

  Update backend feature detection, runtime walking, and threaded guard to use
  `LoadedDefinition` traversal helpers instead of walking only `ld.Workflow`.

- [ ] **Step 4: Update run/resume digest calls**

  Ensure all CLI digest checks call `ld.ComputeDigest()`, including `cli/ui.go`.

- [ ] **Step 5: Update asset snapshotting**

  Store module-qualified assets in `run.started`. Existing `asset.<id>` refs inside a
  module resolve to the module-qualified snapshot key, not a root/global collision.

  Add the shared key helper:

  ```go
  const RootModuleID = ""

  func QualifiedAssetKey(moduleID, assetID string) string {
      if moduleID == RootModuleID {
          return assetID
      }
      return moduleID + "/" + assetID
  }
  ```

  Add a loaded-definition asset snapshot helper:

  ```go
  func StoreRunStartedAssetsForLoadedDefinition(blobs state.Blobs, ld *ir.LoadedDefinition) (map[string]RunStartedAsset, error) {
      out := map[string]RunStartedAsset{}
      if ld == nil {
          return nil, nil
      }
      if root := ld.Root(); root != nil {
          rootAssets, err := StoreRunStartedAssets(blobs, root.Assets)
          if err != nil {
              return nil, err
          }
          for id, asset := range rootAssets {
              out[QualifiedAssetKey(RootModuleID, id)] = asset
          }
      }
      if err := ld.WalkModules(func(module *ir.LoadedModule) error {
          if module.ID == RootModuleID {
              return nil
          }
          moduleAssets, err := StoreRunStartedAssets(blobs, module.Assets)
          if err != nil {
              return err
          }
          for id, asset := range moduleAssets {
              out[QualifiedAssetKey(module.ID, id)] = asset
          }
          return nil
      }); err != nil {
          return nil, err
      }
      if len(out) == 0 {
          return nil, nil
      }
      return out, nil
  }
  ```

  Update run-start snapshotting, `Scope`/input-file resolution, and output-file
  `schema_ref: asset.<id>` resolution to pass the current module id into
  `QualifiedAssetKey`. Root asset keys stay bare for old logs; imported module
  assets use `moduleID/assetID`.

- [ ] **Step 6: Run CLI tests**

  ```bash
  go test ./engine ./cli -run 'Asset|Import|Call|RuntimeDrift|BackendAuto|Threaded'
  go test ./cli
  ```

  Expected: pass.

- [ ] **Step 7: Commit**

  ```bash
  git add cli engine/asset_keys.go engine/assets.go engine/assets_test.go
  git commit -m "cli: wire workflow imports through run and resume"
  ```

## Task 11: Obs, Inspect, Trace, Graph, UI

**Files:**
- Modify: `obs/project.go`
- Modify: `cli/inspect.go`
- Modify: `cli/trace.go`
- Modify: `graph/graph.go`
- Modify: `graph/instances.go`
- Modify: `ui/src/projection.ts` when graph JSON gains call-specific fields.
- Add/update tests under `obs`, `cli`, `graph`, and `ui`

- [ ] **Step 1: Write projection tests**

  Add these tests:

  - `TestObsProjectsCallStartedAndChildPaths`: an event stream with `call.started`, child `node.started`, and call `node.completed` projects one call boundary plus child nodes.
  - `TestTraceShowsCallBoundaryAndChildFailure`: `awf trace` output includes both the call path and failing child path.
  - `TestGraphIncludesImportedChildNodesUnderCall`: graph JSON includes the parent call node and child nodes under `call.workflow`.

- [ ] **Step 2: Run red tests**

  ```bash
  go test ./obs ./cli ./graph -run 'Call|Import'
  ```

  Expected: fail.

- [ ] **Step 3: Project call events**

  `obs.Project` should understand `call.started` and show call nodes as real spans/boundaries. Child nodes stay visible under their call path.

- [ ] **Step 4: Update inspect/trace renderers**

  Show:

  - call path
  - call input ref
  - call runtimes
  - child nodes
  - call outputs/files
  - child root-cause failure and call boundary failure

- [ ] **Step 5: Update graph projection**

  Static graph should include call nodes and imported child template nodes nested under the call node. Runtime graph should map `callID.workflow.*` events to the right instance nodes.

- [ ] **Step 6: Run tests**

  ```bash
  go test ./obs ./cli ./graph ./ui
  if git diff --name-only -- ui/src | rg -q .; then
    make ui-test
    make ui
  fi
  ```

  Expected: Go tests pass. If `ui/src` changed, Vitest passes and `ui/dist` is regenerated.

- [ ] **Step 7: Commit**

  ```bash
  git add obs cli graph ui
  git commit -m "obs: project workflow call boundaries"
  ```

  Expected: when `ui/src` changed, the staged `ui` paths include both source updates and regenerated `ui/dist` output.

## Task 12: Conformance

**Files:**
- Modify: `conformance/suite.go`
- Modify: `conformance/fixtures.go`
- Add: `conformance/subworkflow_test.go`

- [ ] **Step 1: Add simple call conformance**

  Fixture:

  ```text
  root imports child
  parent calls child with input.target
  child code step emits output_schema field summary
  parent code step references step.child_call.summary
  ```

- [ ] **Step 2: Add half-commit resume conformance**

  Build the half-commit case by seeding state directly, not by adding a hidden test-only
  runtime hook:

  ```text
  1. Create a root/child fixture with root call path child_call and child leaf path child_call.workflow.final.
  2. Load the fixture and compute the loaded-definition digest.
  3. Create an in-memory or tempdir-backed fake log and blobs using existing conformance harness helpers.
  4. Put the root run input blob and append run.started with the loaded-definition digest.
  5. Put the call input blob {"target":"x"} and append call.started at path child_call.
  6. Put the child output/artifact blobs and append node.completed at path child_call.workflow.final.
  7. Do not append node.completed at path child_call.
  8. Resume with a fake backend that records dispatch calls.
  ```

  Expected: resume evaluates child workflow exports from the folded child result, commits
  exactly one `node.completed` at `child_call`, and records no dispatch for
  `child_call.workflow.final`.

- [ ] **Step 3: Add artifact export conformance**

  Child captures `output_files.report`, workflow exports `report`, parent stages
  `step.call.files.report` into a later step via `input_files`.

- [ ] **Step 4: Add named aggregate artifact export conformance**

  Child map/reduce exposes an aggregate product file such as
  `step.version_universe.files.item4`, workflow exports it as `output_files.item4`,
  and parent stages `step.call.files.item4`. Keep an existing body-step reduce ref in
  the fixture to prove backward compatibility.

- [ ] **Step 5: Add module asset collision conformance**

  Root and child both declare `assets.schema`. Root uses `asset.schema` in a parent
  step; child uses `asset.schema` in a child step or artifact `schema_ref`. Assert
  each resolves to its own run-start snapshot on resume.

- [ ] **Step 6: Add repeated call conformance**

  Parent calls the same import twice with different input. Assert outputs differ and
  container state does not leak.

- [ ] **Step 7: Add nested call conformance**

  Root calls `outer`, `outer` calls `inner`. Assert runtime paths include:

  ```text
  outer_call.workflow.inner_call.workflow.<leaf-step>
  ```

- [ ] **Step 8: Add digest drift conformance**

  Start a run, mutate imported workflow semantics or imported asset bytes, then
  resume. Expected: digest mismatch before execution. Add a separate assertion that
  imported workflow comment-only changes do not trigger drift.

- [ ] **Step 9: Run conformance**

  ```bash
  go test ./conformance/... -run 'Subworkflow|Import|Call'
  ```

  Expected: pass.

- [ ] **Step 10: Run full test gate**

  ```bash
  make lint test
  govulncheck ./...
  ```

  Expected: pass with no reachable untriaged vulnerabilities.

- [ ] **Step 11: Commit**

  ```bash
  git add conformance
  git commit -m "conformance: cover workflow calls"
  ```

## Fixture Appendix

Use these YAML bodies in loader, engine, CLI, and conformance tests. Adjust image
digests only to match existing fake-backend fixture conventions.

Simple call root:

```yaml
workflow: root-call
version: 1
input:
  type: object
  required: [target]
  properties:
    target: {type: string}
imports:
  recon: workflows/recon.awf.yaml
containers:
  lab:
    image: alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: child_call
    call: recon
    input:
      target: "{{ input.target }}"
  - id: consume
    container: lab
    run: "printf '%s' '{{ step.child_call.summary }}' > /out/consume.txt"
    output_files:
      consume: /out/consume.txt
```

Simple child:

```yaml
workflow: recon
version: 1
input:
  type: object
  required: [target]
  properties:
    target: {type: string}
containers:
  lab:
    image: alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000
output_schema:
  type: object
  required: [summary]
  properties:
    summary: {type: string}
outputs:
  summary: "{{ step.final.summary }}"
output_files:
  report: step.final.files.report
graph:
  - id: final
    container: lab
    run: "printf '{\"summary\":\"ok:%s\"}' '{{ input.target }}' > \"$AWF_OUTPUT\"; printf 'report' > /out/report.md"
    output_schema:
      type: object
      required: [summary]
      properties:
        summary: {type: string}
    output_files:
      report: /out/report.md
```

Typed input child assertion fixture:

```yaml
workflow: typed-child
version: 1
input:
  type: object
  required: [items]
  properties:
    items:
      type: array
containers:
  lab:
    image: alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000
output_schema:
  type: object
  required: [count]
  properties:
    count: {type: number}
outputs:
  count: "{{ step.count.count }}"
graph:
  - id: count
    container: lab
    run: "printf '{\"count\":2}' > \"$AWF_OUTPUT\""
    output_schema:
      type: object
      required: [count]
      properties:
        count: {type: number}
```

Nested call shape:

```yaml
workflow: outer
version: 1
imports:
  inner: inner.awf.yaml
containers: {}
output_schema:
  type: object
  required: [value]
  properties:
    value: {type: string}
outputs:
  value: "{{ step.inner_call.value }}"
graph:
  - id: inner_call
    call: inner
```

Repeated call assertion:

```yaml
graph:
  - id: first
    call: recon
    input: {target: "one"}
  - id: second
    call: recon
    input: {target: "two"}
```

Expected paths:

```text
child_call.workflow.final
outer_call.workflow.inner_call.workflow.<leaf-step>
first.workflow.final
second.workflow.final
```

Half-commit resume setup:

```text
1. Put the root input blob and append run.started with loaded-definition digest.
2. Put the call input blob {"target":"x"} and append call.started at path child_call with that input_ref.
3. Put the child output/artifact blobs and append node.completed at path child_call.workflow.final.
4. Do not append node.completed at path child_call.
5. Resume with the fake backend and assert only child_call commits; child_call.workflow.final is not dispatched again.
```

## High-Risk Test Skeletons

Use these as starting points for the red tests in the tasks above. Keep the helper
names local to the test files or map them to existing fixture helpers.

Loader typed error:

```go
func TestLoadImportRejectsPathEscape(t *testing.T) {
    root := writeWorkflowFile(t, "root.awf.yaml", rootWithImport("bad", "../bad.awf.yaml"))
    _, err := loader.Load(root)
    if err == nil {
        t.Fatal("Load succeeded, want error")
    }
    var loadErr *loader.LoadError
    if !errors.As(err, &loadErr) {
        t.Fatalf("error = %T, want *loader.LoadError", err)
    }
    if loadErr.Code != "AWF_IMPORT_PATH_ESCAPE" {
        t.Fatalf("Code = %q", loadErr.Code)
    }
}
```

Module asset collision:

```go
func TestRunStartedAssetsAreModuleQualified(t *testing.T) {
    ld := loadRootAndChildBothDeclaringAssetSchema(t)
    got, err := StoreRunStartedAssetsForLoadedDefinition(fakeBlobs(t), ld)
    if err != nil {
        t.Fatal(err)
    }
    assertBlobBytes(t, got, "schema", []byte(`{"root":true}`))
    assertBlobBytes(t, got, "recon/schema", []byte(`{"child":true}`))
}
```

Imported digest canonical semantics:

```go
func TestDigestFoldsImportedWorkflowCanonicalIR(t *testing.T) {
    ld1 := loadSubworkflowFixture(t, "child-output-summary")
    d1, err := ld1.ComputeDigest()
    if err != nil {
        t.Fatal(err)
    }

    ld2 := loadSubworkflowFixture(t, "child-output-renamed")
    d2, err := ld2.ComputeDigest()
    if err != nil {
        t.Fatal(err)
    }
    if d1 == d2 {
        t.Fatal("digest unchanged after semantic imported workflow change")
    }
}

func TestDigestIgnoresImportedWorkflowCommentOnlyChange(t *testing.T) {
    ld1 := loadSubworkflowFixture(t, "child-no-comment")
    ld2 := loadSubworkflowFixture(t, "child-comment-only")
    d1, err := ld1.ComputeDigest()
    if err != nil {
        t.Fatal(err)
    }
    d2, err := ld2.ComputeDigest()
    if err != nil {
        t.Fatal(err)
    }
    if d1 != d2 {
        t.Fatalf("digest changed for comment-only import edit: %s != %s", d1, d2)
    }
}
```

Half-commit call resume:

```go
func TestResumeHalfCommittedCallCommitsOnlyCallProduct(t *testing.T) {
    run := startSubworkflowRun(t)
    appendCallStarted(t, run, "recon_call")
    appendChildNodeCompleted(t, run, "recon_call.workflow.final")
    stepsBefore := fakeBackendStepExecutions(t, run)

    resumeRun(t, run)

    assertNodeCompleted(t, run, "recon_call")
    assertNoAdditionalStepExecutions(t, run, stepsBefore)
}
```

Workflow artifact export through call:

```go
func TestResolveCallArtifactRefByExportName(t *testing.T) {
    rs := foldedRunStateWithCallFiles(t, "recon_call", map[string]string{
        "report": "sha256:child-report",
    })
    wf := workflowWithCallProducer(t, "recon_call", "report")
    scope := NewScope(rs, wf, "consume")
    ref, err := resolveNamedArtifactRef(scope, wf, "recon_call", "report")
    if err != nil {
        t.Fatal(err)
    }
    if ref != "sha256:child-report" {
        t.Fatalf("ref = %q", ref)
    }
}
```

CLI digest scan:

```go
func TestAllProductionCLIDigestChecksUseLoadedDefinition(t *testing.T) {
    out := runCommand(t, "rg", "-n", `Workflow\.ComputeDigest|ComputeDigest\(`, "cli")
    assertNotContains(t, out, "Workflow.ComputeDigest")
    assertContains(t, out, "ld.ComputeDigest")
}
```

Repeated call isolation:

```go
func TestRunCallStepRepeatedCallsHaveIsolatedContainers(t *testing.T) {
    run := runWorkflow(t, repeatedCallFixture())
    assertStepOutput(t, run, "first_call", "summary", "first")
    assertStepOutput(t, run, "second_call", "summary", "second")
    assertCapturedFileDoesNotContain(t, run, "second_call.workflow.final", "state.txt", "first")
}
```

## Task 13: Final Verification And Review

**Files:**
- All touched files

- [ ] **Step 1: Run full verification**

  ```bash
  make lint test
  govulncheck ./...
  ```

  Expected: pass with no reachable untriaged vulnerabilities.

- [ ] **Step 2: Inspect diff**

  ```bash
  git diff --stat main...HEAD
  git diff --check main...HEAD
  ```

  Expected: no whitespace errors; diff limited to subworkflow implementation, docs, and accepted follow-ups.

- [ ] **Step 3: Run plan hygiene scan**

    ```bash
    rg -n 'ComputeDigest\(' cli engine ir loader
    rg -n 'state\.Event\{Type: EventNodeCompleted|EventNodeCompleted' engine
    ! rg -n -e 'T[B]D' -e '\bT[O]DO\b' -e 'implement[ ]later' -e 'fill[ ]in[ ]details' -e 'as[ ]needed' -e 'handle[ ]edge[ ]cases' -e 'Similar[ ]to[ ]Task' -e '<review-fix-file[s]>' docs/superpowers/plans/2026-06-09-awf-subworkflows.md
    awk '
  function fence(line,    s) {
      if (match(line, /^[ \t]*(```+|~~~+)/)) {
          s = substr(line, RSTART, RLENGTH)
          sub(/^[ \t]*/, "", s)
          return s
      }
      return ""
  }
  {
      m = fence($0)
      if (m == "") {
          next
      }
      c = substr(m, 1, 1)
      l = length(m)
      if (!open) {
          open = 1
          char = c
          len = l
          line = NR
          next
      }
      if (c == char && l >= len) {
          open = 0
      }
  }
  END {
      if (open) {
          print "unclosed code fence opened at line " line
          exit 1
      }
  }' docs/superpowers/plans/2026-06-09-awf-subworkflows.md
  ```

  Expected: production CLI uses `ld.ComputeDigest()`; any direct workflow digest
  calls are in `ir/digest.go`, `ir/digest_test.go`, or focused tests that bypass the
  loader. The `EventNodeCompleted` scan shows direct append construction only in
  `engine/commit.go`; other engine files call `appendNodeCompleted`. Placeholder `rg`
  prints no accidental placeholders, and `awk` exits successfully.

- [ ] **Step 4: Run code review**

  Use `superpowers:requesting-code-review` or gstack `/review` before merge.

- [ ] **Step 5: Address findings**

  Fix all P1/P2 findings with tests, rerun:

  ```bash
  make lint test
  govulncheck ./...
  ```

- [ ] **Step 6: Commit review fixes only when Step 5 changed files**

  Stage only the files changed while addressing Step 5 review findings, using an
  explicit `git add --` command with exact path arguments after checking status.
  Do not stage unrelated work from the worktree.

  ```bash
  git status --short
  git diff --cached --check
  git commit -m "fix: address subworkflow review findings"
  ```

  Between `git status --short` and `git diff --cached --check`, run `git add --` with
  only the real Step 5 review-fix paths printed by status. Do not run the commit command
  until those exact paths are staged and `git diff --cached --check` passes.

---

## Exhaustive CallStep Checklist

Every implementation must update or explicitly verify:

- `ir/node.go`
- `ir/node_unmarshal.go`
- `ir/node_marshal.go`
- `ir/node_test.go`
- `ir/tags_test.go`
- `ir/walk.go`
- `ir/path.go`
- `ir/validate_structural.go`
- `ir/validate_refs.go`
- `ir/validate_input_files.go`
- `ir/validate_output_files.go`
- `ir/validate_schema.go`
- `loader/errors.go`
- `loader/safepath.go`
- `loader/imports.go`
- `engine/interpreter_context.go`
- `engine/interpreter.go`
- `engine/path.go`
- `engine/scope.go`
- `engine/asset_keys.go`
- `engine/artifact_scope.go`
- `cli/backend_features.go`
- `cli/runtimes.go`
- `cli/threaded_guard.go`
- `cli/ui.go`
- `obs/project.go`
- `cli/inspect.go`
- `cli/trace.go`
- `graph/graph.go`
- `ui/src/projection.ts` if graph JSON changes
- conformance fixtures

---

## Self-Review

Spec coverage:

- dependency and vulnerability preflight: Task -1
- local imports, strict confinement, shared safe-path helper, module loading: Tasks 3, 5
- `CallStep`: Tasks 2, 9
- typed call input: Task 6
- workflow exports and artifact aliases: Task 8
- digest and resume drift: Tasks 4, 10, 12
- call-start event and half-commit recovery: Tasks 7, 9, 12
- per-call containers and snapshot keys: Task 9
- CLI/obs/graph command support: Tasks 10, 11
- conformance: Task 12
- deferred work: already recorded in `TODOS.md`

Placeholder scan:

- No implementation steps depend on unnamed "appropriate handling."
- Each task has exact files, commands, and expected results.
- Follow-up work is explicitly deferred in `TODOS.md`, not left as hidden scope.
- Final verification includes an explicit placeholder and code-fence hygiene scan.

Type consistency:

- `CallStep`, `TemplateValue`, `ArtifactExports`, `LoadError`, `safePathError`, `LoadedModule`, `LoadedImportEdge`, `CallStartedData`, `CallStartedRecord`, `RuntimeRef`, `WorkflowExportResult`, `appendNodeCompleted`, `commitCallProduct`, `CallWorkflowRuntimePath`, `QualifiedAssetKey`, `StoreRunStartedAssetsForLoadedDefinition`, and `QualifiedContainerKey` names are used consistently across tasks.
