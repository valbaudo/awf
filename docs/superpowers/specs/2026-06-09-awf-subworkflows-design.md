# AWF Subworkflows Design

**Status:** Approved design after `/plan-ceo-review` selective expansion.
**Date:** 2026-06-09
**Target:** Go 1.26, single `awf` binary.
**Scope:** Local importable `.awf.yaml` workflows, invoked through explicit `call:` steps.

---

## 1. Summary

AWF should support reusable workflow modules without becoming a generic YAML macro
system. A root workflow may declare local imports and call them as product-producing
steps. Imported workflow internals are private for references but visible in traces.
The parent sees only the call node product:

```yaml
imports:
  recon: workflows/recon.awf.yaml

graph:
  - id: recon_result
    call: recon
    input:
      target: "{{ input.target }}"

  - id: consume_report
    container: lab
    input_files:
      /work/report.md: step.recon_result.files.report
    run: ./use-report /work/report.md
```

The imported workflow declares an explicit export contract:

```yaml
workflow: recon
version: 1
input:
  type: object
  required: [target]
  properties:
    target:
      type: string

output_schema:
  type: object
  required: [summary]
  additionalProperties: false
  properties:
    summary:
      type: string

outputs:
  summary: "{{ step.final.summary }}"

output_files:
  report: step.final.files.report
```

This is a module boundary, not file inclusion. Parent workflows cannot reference
`step.final.*` from the child. They can reference only `step.recon_result.*` and
`step.recon_result.files.*`.

---

## 2. Goals

- Add top-level `imports:` for local `.awf.yaml` files.
- Add a step-like `CallStep` node with `id`, `call`, and typed `input`.
- Add workflow-level exports: `output_schema`, `outputs`, and `output_files`.
- Include imported workflow canonical IR, imported compose bytes, and imported asset
  bytes in the root definition digest.
- Snapshot imported workflow assets at run start and stage from the run-start
  snapshot on resume.
- Execute child workflows under deterministic call paths:

  ```text
  <call-id>.workflow.<child-path>
  ```

- Commit the parent-visible call product at `<call-id>`.
- Support multiple calls to the same import and nested calls with a finite depth limit.
- Use per-call invocation containers so repeated calls do not share mutable state.
- Use one shared loader safe-path helper for imports, compose files, and assets.
- Preserve AWF invariants: interpreter-only state writes, content-address-before-log,
  resume-by-fold, runtime pinning, and one pure node-addressing helper.
- Make `awf validate`, `awf run`, `awf resume`, `awf inspect`, `awf trace`, `awf graph`,
  and `awf ui` work with call boundaries and imported modules.

---

## 3. Non-Goals

- Remote URL/GitHub imports.
- Import registries, package managers, plugin layers, or lockfile update commands.
- Parent references to imported workflow internals.
- Cross-run caching of subworkflow execution.
- Distributed execution, multi-host scheduling, compensation, or saga semantics.
- Treating templating as a language. `call.input` preserves typed values but does
  not add arithmetic, calls, or loops.

---

## 4. Format Additions

### 4.1 Top-Level Imports

```yaml
imports:
  recon: workflows/recon.awf.yaml
  exploit: workflows/exploit.awf.yaml
```

Rules:

- Import ids use the existing step-id identifier discipline: ASCII identifier,
  no `.`, no path separators, no template syntax, and no reserved id `workflow`.
- Import ids share the module-local addressable namespace with step ids and
  aggregate product ids. Duplicates are validation errors.
- Import paths are relative to the declaring workflow file's directory.
- Import paths are slash paths normalized with `path.Clean`, checked with
  `fs.ValidPath`, localized with `filepath.Localize`, then opened through that
  module's `os.Root`.
- Absolute paths, backslashes, control characters, `..` escape, and symlink path
  components are rejected.
- Imports are local only.
- Import cycles are validation errors.
- Import/call nesting has a finite depth limit. The first implementation should use
  `maxImportDepth = 10` and `maxCallDepth = 10`.

### 4.2 Call Step

`call` is a step-like node. It has an `id` and produces typed outputs and named files.

```yaml
- id: recon_result
  call: recon
  input:
    target: "{{ input.target }}"
    candidates: "{{ step.seed.candidates }}"
```

Rules:

- `call` must name an imported workflow id visible to the current module.
- `id` joins the normal step/map-product id namespace for the parent workflow.
- `input` is a mapping of JSON field name to `TemplateValue`, which is raw JSON
  whose strings may contain AWF template slots. Objects and arrays are preserved.
- If the child declares `input`, resolved call input must be present and schema-valid.
- If the child declares no `input`, non-empty `call.input` is an authoring error.
- Parent refs see the call product as `step.<call-id>.<field>` and
  `step.<call-id>.files.<name>`.

Implementation type:

```go
type TemplateValue = json.RawMessage
```

`TemplateValue` is not `RawConfig`: AWF core evaluates it recursively, while
`with:` remains adapter-owned and opaque.

### 4.3 Workflow Exports

Workflow exports are explicit. `outputs` values use the same `TemplateValue`
encoding as `call.input`, so an exported field may be a string, number, boolean,
object, array, or null after template evaluation.

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

Rules:

- `outputs` evaluates after the child graph finishes.
- `outputs` resolves only against child-local scope: child input, child steps, child
  map products, `run.id`, and legal child `evaluate.*` contexts.
- Parent step refs are unavailable inside child exports unless passed through
  `call.input`.
- `output_schema` validates the evaluated `outputs` object.
- `output_files` entries are aliases to committed child artifacts. They never read
  live container files at export time.
- Workflow-level `output_files` is an artifact-export contract, not the existing
  step-level capture contract. It maps exported file name to child-local
  `step.<id>.files.<name>` refs.
- A child workflow export contract exists when it declares `outputs`,
  `output_schema`, or workflow-level `output_files`. Artifact-only child workflows
  are valid. `output_schema` is required only when `outputs` is non-empty or a
  parent/downstream ref uses typed fields from the call product.

---

## 5. Loading Model

Extend the loader output with modules while keeping plain structs:

```go
type LoadedDefinition struct {
    Workflow     *Workflow
    WorkflowPath string
    ComposeFiles map[string][]byte
    Assets       map[string]LoadedAsset

    RootModule string
    Modules    map[string]*LoadedModule
}

type LoadedModule struct {
    ID           string
    Workflow     *Workflow
    WorkflowPath string
    ComposeFiles map[string][]byte
    Assets       map[string]LoadedAsset
}
```

Keep these field names unless an implementation collision with existing code makes
one impossible. One module record owns one parsed workflow, its path, and the
external bytes loaded relative to that workflow file.

Loader responsibilities:

- Recursively load imports starting from the root workflow.
- Treat module identity as the dotted import address from the root: root is `""`,
  a direct import is `recon`, and a nested import is `outer.inner`. Importing the
  same absolute file through two aliases creates two logical modules in V1.
- Use a separate `os.Root` for each module directory.
- Normalize import, compose, and asset manifest paths through one shared helper
  using slash-path `path.Clean`, `io/fs.ValidPath`, `filepath.Localize`, and
  `filepath.IsLocal`.
- Open normalized paths through the module's `os.Root`; the path helper is
  normalization, while `os.Root` is the containment boundary.
- The shared path helper returns typed path errors with stable codes; the loader
  wraps those into `*loader.LoadError` at the import/load boundary.
- Reuse existing symlink rejection and regular-file checks for imports, compose
  files, and assets as AWF authoring policy, not as the only security boundary.
- Preserve existing `LoadedDefinition.Workflow` as the root workflow for backward
  compatibility with tests and callers.
- Return typed loader errors for filesystem/import failures:

  ```go
  type LoadError struct {
      Code    string
      Source  string
      Path    string
      Message string
      Err     error
  }

  func (e *LoadError) Error() string
  func (e *LoadError) Unwrap() error
  ```

  `awf validate` converts `*loader.LoadError` into an `ir.Diagnostic` so users get
  the same source/path/code formatting as validation failures.
- Provide narrow traversal helpers instead of a registry abstraction:

  ```go
  func (ld *LoadedDefinition) Root() *LoadedModule
  func (ld *LoadedDefinition) Module(id string) (*LoadedModule, bool)
  func (ld *LoadedDefinition) WalkModules(fn func(*LoadedModule) error) error
  ```

Call graph traversal should be central too:

```go
func (ld *LoadedDefinition) WalkReachableCalls(fn func(site CallSite) error) error
```

`CallSite` should carry parent module id, call path, call step, child module id, and
call depth. This prevents every CLI walker from inventing import traversal.

---

## 6. Digest Model

Keep `awf-d1`. This is an additive format feature, not a canonicalization scheme
change.

Digest input includes:

- root workflow canonical IR
- root compose bytes
- root asset bytes
- each imported workflow canonical IR, after loader normalization
- each imported compose byte stream
- each imported asset byte stream
- import graph structure and normalized import aliases/paths

Use deterministic framed entries so import bytes cannot alias compose or asset frames.
The framing should follow the existing length-prefixed digest style:

```text
frame("module-workflow")
frame(module-id)
frame(normalized-import-path)
frame(canonical-workflow-digest)

frame("module-compose")
frame(module-id)
frame(compose-path)
sha256(compose-bytes)

frame("module-asset")
frame(module-id)
frame(asset-id)
frame(asset-path)
sha256(asset-bytes)
```

Existing workflows without `imports`, `output_schema`, `outputs`, `output_files`, or
`call` retain their existing digest behavior. Imported YAML comment or formatting
changes alone do not change the digest; semantic imported workflow, compose, or
asset changes do.

Resume remains simple:

- Load root workflow and imports.
- Validate.
- Compute current digest.
- Compare to `run.started.workflow_digest`.
- Hard-fail on mismatch before execution.

No import lockfile ships in V1.

---

## 7. Runtime Model

### 7.1 Call Execution

`CallStep` runs as a normal step boundary with internal graph execution:

```text
call path: recon_result

recon_result
  ├── node.started{kind:"call"}
  ├── call.started{input_ref,runtimes}
  ├── recon_result.workflow.scan
  ├── recon_result.workflow.gate[1].attempt-1.generate.generate_poc
  └── node.completed at recon_result
```

Flow:

1. If the call path already has `node.completed`, skip it.
2. If no `call.started` exists:
   - evaluate typed call input against the parent scope
   - validate against child workflow `input`
   - store the resolved input in `Blobs`
   - create per-call child containers needed to resolve child agent runtime versions
   - resolve child agent runtimes
   - append and sync `call.started{input_ref,runtimes}`
3. If `call.started` exists:
   - read the recorded call input from `RunState.CallStarted`
   - recreate child containers
   - re-resolve runtimes and hard-fail on drift
4. Run the child graph under `<call-id>.workflow`.
5. Evaluate child workflow `outputs` from child-local scope.
6. Validate outputs against child `output_schema`.
7. Resolve workflow `output_files` as aliases to child committed artifact refs and
   verify every referenced CAS ref exists in `Blobs`.
8. Commit normal `node.completed` for the call path.

`node.started{kind:"call"}` is observational. If a process crashes after call
`node.started` but before `call.started`, resume may emit another `node.started`,
but it must proceed to exactly one durable `call.started` and must not corrupt fold.

### 7.2 Call Started Event

Add an event:

```go
const EventCallStarted = "call.started"

type CallStartedData struct {
    InputRef string `json:"input_ref"`
    Runtimes []ResolvedRuntime `json:"runtimes,omitempty"`
}
```

Fold state:

```go
type CallStartedRecord struct {
    Input map[string]any
    InputRef string
    Runtimes []ResolvedRuntime
}

type RunState struct {
    CallStarted map[string]CallStartedRecord
}
```

`call.started` is a durable invocation boundary. If the process crashes after it is
written but before child completion, resume reuses the recorded input and runtime pins.

### 7.3 Failure Semantics

If a child step fails:

- the failing child path records the root-cause `node.failed`
- the call boundary then records `node.failed` at the call path
- the call-boundary error text includes the failing child path, for example
  `child recon_result.workflow.scan: permanent_failure: ...`
- `try/catch` around the call sees the call path as the failed step

The implementation must preserve interpreter-only state writes. Do not let dispatchers
or backends append call failure events.

### 7.4 Half-Commit Recovery

If the child graph completed but the call product did not commit, resume must not rerun
the child graph. It should:

- fold committed child nodes
- rerun only export evaluation
- commit the missing call node product

This is required conformance coverage.

### 7.5 Container Lifetime And Snapshot Keys

Child workflow containers are per call invocation. They are long-lived across steps
inside that child invocation and destroyed when the call completes or fails.
Use the existing `*LocalDispatcher` scoped-handle pattern already used by map and
runtime compose. Do not add a new dispatcher interface for call preparation.

Runtime container identity must be qualified by call path:

```text
recon_result.workflow::lab
outer.workflow.inner.workflow::lab
```

Static child graph parent paths and runtime path/key construction are separate.
`ir.CallWorkflowParentPath(callPath)` is only for static validation and graph
projection. Runtime execution uses `engine.CallWorkflowRuntimePath(callRuntimePath)`
to produce `<call>.workflow`, and `engine.QualifiedContainerKey(runtimeParent,
container)` produces snapshot/runtime keys such as `recon_result.workflow::lab`.
Snapshot refs use this qualified identity, not bare container names, so repeated
calls do not restore each other's workspaces.

Dynamic map containers inside child workflows keep their existing per-item lifecycle,
nested under the child call path.

---

## 8. Validation Model

Add stable `AWF` diagnostic codes for import/call validation failures. Exact code
numbers should fit the existing catalog ranges. Required classes:

- invalid import id
- import path escape/absolute/backslash/symlink
- unreadable import file
- import cycle
- import depth exceeded
- unknown call target
- call input provided when child declares no input
- missing call input when child declares input
- bad call input template/reference
- call input schema mismatch
- child workflow used by call has no export contract when parent-visible typed or
  file refs require one
- bad workflow export template/reference
- workflow export schema mismatch
- bad workflow artifact export ref
- duplicate ids across parent step/map products and call step ids

`ir.Diagnostic` gains optional source attribution:

```go
type Diagnostic struct {
    Severity Severity
    Source   string `json:",omitempty"`
    Path     string
    Code     string
    Message  string
}
```

`Path` remains the static IR path inside that module. It is not overloaded with
module prefixes.

Parent validation sees a call step as a producer whose typed schema and exported
files come from the child workflow's export contract. Artifact-only child workflows
are valid producers for `step.<call-id>.files.<name>` without requiring
`output_schema`.

Child validation remains child-local. Imported internals do not enter the parent's
producer namespace.

---

## 9. CLI And Command Impact

`awf validate`:

- recursively load imports
- report imported-file diagnostics with `Source`
- compute digest including imported canonical workflow IR, compose bytes, asset
  bytes, and import graph structure

`awf run`:

- backend auto scans all reachable modules/calls for Docker-only features
- root `run.started` records root digest and root-level runtimes as today
- call-level runtimes are recorded in `call.started`
- run-start asset snapshots include module-qualified asset ids. Root assets keep
  their bare keys for old logs; imported assets use `moduleID/assetID`.

`awf resume`:

- reloads root and imports
- recomputes digest and hard-fails on drift
- uses recorded backend from root `run.started`
- folds `call.started`
- reuses recorded call input and checks call runtime drift
- restores child snapshots using qualified runtime container keys

`awf inspect` / `awf trace`:

- show call boundaries
- show child nodes under `callID.workflow.*`
- show `call.started` input ref/runtimes
- show call product outputs/files
- show child root-cause failures and call-boundary failure context

`awf graph` / `awf ui`:

- imported internals are visible as nested runtime/static nodes under the call node
- internals remain non-contractual for parent references

---

## 10. Tests And Conformance

Required unit coverage:

- shared safe-path helper normalizes `./x`, rejects backslashes/control chars,
  rejects escape paths, and permits `.` only for policies that explicitly allow
  root assets
- loader imports local files relative to module path
- loader rejects absolute, backslash, `..`, and symlink import paths
- loader rejects import cycles and depth overflow
- loader treats the same file imported under two aliases as two logical modules
- digest changes when imported workflow canonical IR changes
- digest does not change when only imported workflow comments or formatting change
- digest changes when imported compose/assets change
- `CallStep` marshal/unmarshal round-trip
- `CallStep` appears in every exhaustive walker
- call input typed evaluation preserves object/array/bool/number/string/null
- child export evaluation uses child-local scope only
- workflow artifact exports alias committed child artifact refs
- call product commit verifies exported file refs exist in `Blobs` before appending
  call `node.completed`
- root and child modules can both declare `asset.schema`; child refs resolve to the
  child run-start snapshot and root refs resolve to the root snapshot
- `call.started` folds into `RunState.CallStarted`
- crash after call `node.started` but before `call.started` resumes safely and
  writes exactly one durable `call.started`
- call runtime drift errors on resume
- snapshot keys are qualified by call path

Required fake-backend conformance:

- validate/run simple imported workflow
- resume simple imported workflow
- digest drift from semantic imported workflow changes hard-fails resume
- digest drift from imported asset bytes hard-fails resume
- crash after child completion before call commit resumes by committing the call product without rerunning child steps
- child artifact export flows through `step.<call-id>.files.<name>` into parent `input_files`
- artifact refs through a named aggregate inside a child workflow flow through the
  call's exported `output_files`
- repeated calls to the same import use separate inputs and isolated container state
- nested calls run and resume with `outer.workflow.inner.workflow.*` paths

Verification gate:

```bash
make lint test
govulncheck ./...
```

`govulncheck` must have no reachable untriaged vulnerabilities. Docker-specific
behavior should stay in integration tests only when the behavior cannot be proved by
the fake backend. V1 conformance must be runnable without Docker.

---

## 11. Deferred Work

Add to project follow-up backlog:

- import lockfile and remote-import provenance design
- benchmark and optionally cache per-module step/map-product indexes

Remote imports require a separate threat model covering pinning, offline resume, cache
layout, authentication, provenance, lockfile update semantics, and review UX.

---

## 12. Implementation Notes

Adding `CallStep` touches the closed node sum. The implementation plan must include
an exhaustive checklist for:

- `ir/node.go`
- `ir/node_unmarshal.go`
- `ir/node_marshal.go`
- `ir/walk.go`
- loader safe-path/import helpers
- validation structural/ref/schema/input-files/output-files passes
- `engine/interpreter.go`
- `engine/scope.go`
- artifact scope helpers
- CLI backend feature, runtime pinning, threaded guard, and capability walkers
- shared engine runtime-resolution helper used by CLI and call execution
- obs/trace/inspect projection
- graph/UI projection
- conformance fixtures

Avoid speculative abstractions. Use plain structs, helper functions, and existing package
boundaries.
