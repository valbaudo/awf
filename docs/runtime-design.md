# AWF Runtime — Implementation Design

**Status:** Draft. Companion to `AgentWorkflowFormat.md` (the standard). This document covers the *implementation* the standard deliberately leaves open: storage backend, scheduling/dispatch, executor mechanics.
**Date:** 2026-05-22
**Target:** Go 1.26 (current; 1.26 GA Feb 2026). Single CLI binary, `awf`.

---

## 1. Purpose & scope

A **reference implementation** of AWF — a runtime for *agentic pipelines* whose flagship is the evaluator-optimizer **gate**, TDD for a step (spec §5.5). It orchestrates black-box agent CLIs and commands against long-lived containers (a digest-pinned image or a compose project), enforces quality with independent judges + bounded repair, and checkpoints step outputs to a content-addressed artifact store so expensive stages aren't redone. It is **not** a durable-execution engine, an agent-team framework, or a DAG batch system (spec §1).

The bar is correctness on the things the identity needs: the gate loop behaves (independent evaluator, feedback-fed repair, bounded), checkpoint/resume skips committed stages, and the atomic artifact+journal commit holds. A conformance suite (§14) is the definition of done.

Locked decisions:

| Decision | Choice | Consequence |
|---|---|---|
| Flagship | First-class `gate` (generate → independent evaluate → repair) | The engine has a dedicated gate executor distinct from `loop`; repair threads the judge's feedback into the next generate. |
| Recovery | `retry` (transient) and gate `repair` (quality) are separate axes | Retry re-runs an identical step; repair regenerates conditioned on the judge's critique. |
| Outcomes | Mechanical only: `ok` / `retryable_failure` / `permanent_failure` | Quality is the gate's job; no `success:`/`semantic_failure` step classes. |
| Container backend | Docker/OCI; a container is a single digest-pinned image **or** a compose project. Durable state is a content-addressed **artifact store** (typed outputs + `output_files`), not a per-step FS snapshot. | Infra rebuilt from the image/compose recipe on resume; optional CoW FS snapshot only for `snapshot: workspace`; in-mem fake backend for fast tests. |
| Agent model | Orchestrate external agent CLIs (Claude Code first; Goose/Codex later) | Never call models directly; launch a harness, capture its typed result. |
| Agent checkpoint | Atomic per invocation | Resume re-runs a whole agent step from its pre-step snapshot; re-run may differ. |
| Executor | Tree-walking interpreter | Code mirrors the IR tree 1:1. |
| Containers | One long-lived instance per name, run-scoped | Shared evolving workspace / lab; parallel branches need distinct containers. |
| Scope | Single-host; checkpoint to skip work, not to distribute | No distributed dispatch in v1 (the earlier Kafka/Airflow-scale ambition is dropped — it contradicts the shared-container model). |

Two clean seams are drawn (interfaces only) so the parts that vary can be swapped without a rewrite — *not* to chase distributed scale (explicitly out, per the identity):

1. **Frontend ↔ IR** — YAML is one frontend; the IR is the stable contract.
2. **Durability = append-only event log** — resume is a fold over the log; observability is a projection of it. One local file-backed impl.

Out of scope (matches spec §12): distributed dispatch, multi-tenancy, secrets subsystem (stopgap only — §13).

---

## 2. Architecture overview

```
            ┌──────────────┐
  YAML ───▶ │ frontend/yaml│──┐
            └──────────────┘  │  (other frontends/SDKs target IR directly)
                              ▼
                        ┌───────────┐      validates       ┌──────────┐
                        │    ir     │────────────────────▶ │ validate │
                        └───────────┘                      └──────────┘
                              │ typed node tree + digest
                              ▼
   ┌───────────────────────────────────────────────────────────────┐
   │ engine (tree-walking interpreter)                               │
   │   • sequencing, if, loop, try, parallel, await, GATE            │
   │   • gate: generate→evaluate→repair · retry orchestration        │
   │   • owns the COMMIT BOUNDARY                                     │
   └───────┬───────────────────────────────────────────┬───────────┘
   NodeIntent│                                    events │
            ▼                                            ▼
     ┌─────────────┐                            ┌──────────────────┐
     │ Dispatcher  │  code → container.Exec     │ state            │
     │ (Local)     │  agent → AgentAdapter      │  • Log (append)  │
     └──────┬──────┘  await → SignalBroker      │  • Blobs (CAS)   │
            │                                    └────────┬─────────┘
   ┌────────┴─────────┐                                   │ project
   ▼                  ▼                                    ▼
┌─────────┐     ┌────────────┐                      ┌──────────┐
│container│     │   agent    │                      │   obs    │
│ Backend │     │  Adapter   │                      │  (OTel)  │
│(Docker) │     │(claude-code│                      └──────────┘
└─────────┘     │  …)        │
                └────────────┘
```

The interpreter is the only component that writes to `state`; everything else either executes work (Dispatcher/Backend/Adapter) or reads the log (obs).

---

## 3. Package layout

| Package | Responsibility | Depends on |
|---|---|---|
| `ir` | Stable IR types (the contract). Structural validation. Definition digest. | — |
| `frontend/yaml` | One frontend: YAML → IR types via **goccy/go-yaml** (`yaml.v3` archived Apr 2025); carries line/column for diagnostics. | `ir` |
| `loader` | Reads the workflow + imported workflow files, referenced compose files, and top-level `assets:` → `LoadedDefinition`; the I/O front for `validate`. Imports are relative slash paths confined to the declaring workflow's module dir with no symlink traversal; compose and asset reads use the same Go 1.24 `os.Root` confinement (rejects `..`/symlink escape); asset reads also enforce per-file bytes, total bytes, and file-count bounds. | `ir`, `frontend/yaml` |
| `template` | §7 mini-language: parser + reference extraction (state-free) + value substitution + bounded boolean evaluator (the evaluator reads state). | `state` (read; evaluator only) |
| `skillroute` | Deterministic v1 `bm25` router over top-level `skills:` corpora; builds weighted token documents from run-start asset snapshots and returns selected skill ids with finite scores. | `ir`, `state` (read) |
| `state` | Durability core: append-only `Log` + content-addressed `Blobs`; owns the atomic commit. | — |
| `container` | `Backend` interface + Docker impl + in-mem fake. `fs` snapshot. | — |
| `agent` | `Adapter` interface + registry; resolves `uses:` to a base adapter **or a declared `agents:` role** (a `DerivedAdapter` — a base adapter with a key-blind `with:` overlay, registered per role at run start); runs harness; produces typed result + metrics. | `container` |
| `signal` | `Broker`: delivers external signals to waiting `await` steps (`Receive`); **`ReceiveMatching` consumes the earliest-seq signal whose JSON payload satisfies an `await … where:` predicate — keyed signals**. The pause/cancel control surface lives in `signal/control.go`. | `state` |
| `engine` | Tree-walking interpreter; `Dispatcher` interface + `LocalDispatcher`; commit boundary; outcome classification. | all above |
| `retry` | Policy, backoff, retryable/permanent classification (incl. `non_retryable_exit_codes`). | — |
| `obs` | Read-only OTel projection of the log (`gen_ai.*` + `awf.*`); span tree; run-status fold (`DeriveStatus`); stdout/file + OTLP exporters. | `state` |
| `cli` | `validate`, `run`, `resume`, `signal`, `inspect`/`trace`, `ls`; validate/run/resume all use the recursive loader, so imported workflow diagnostics and digest checks are identical. | `ir`, `engine`, `obs` |
| `clock` | `Clock` + `IDGen` interfaces (injected everywhere time/ids are needed). | — |

Each package has one job and a narrow interface; `engine` is the only one that grows broad, so its sub-concerns (interpreter, dispatcher, commit, addressing) are separate files.

---

## 4. Core data model

### IR (`ir`)
A `Workflow{ID, Version, Input *JSONSchema, InputFiles WorkflowInputFiles, Env []string, Assets map[string]string, Skills map[string]SkillCorpus, Imports map[string]string, Agents map[string]AgentRole, Containers map[string]Container, OutputSchema *JSONSchema, Outputs map[string]TemplateValue, ArtifactExports ArtifactExports, Graph []Node, Digest string}`. `Digest` is a **self-describing** content hash (`awf-d1:sha256:…`) computed at load: the whole resolved IR marshaled to JSON (explicit `json` tags on every field) then RFC-8785 JCS — so it is independent of Go field/map order — folding in imported workflow definitions, referenced compose-file hashes, and asset bytes (spec §8 / Phase-1 design spec). `Node` is a sum type:

```go
type Workflow struct {
    ID              string
    Version         int
    Input           *JSONSchema
    InputFiles      WorkflowInputFiles
    Env             []string
    Assets          map[string]string
    Skills          map[string]SkillCorpus
    Imports         map[string]string
    Agents          map[string]AgentRole
    Containers      map[string]Container
    OutputSchema    *JSONSchema
    Outputs         map[string]TemplateValue
    ArtifactExports ArtifactExports
    Graph           NodeList
    Digest          string
}

type SkillCorpus struct {
    From   string // asset.<id>
    Layout string // v1: skill_dirs
    Router string // v1: bm25
}

type StepSkillRouting struct {
    From  string   // top-level skills corpus id
    Query Template // rendered in normal step scope
    Limit int
    Into  string   // absolute clean container path, not /
}

type Node interface{ isNode() }

type CodeStep struct {
    ID, Container, Run string
    Timeout      *time.Duration
    OutputSchema *JSONSchema   // optional; parsed from $AWF_OUTPUT
    OutputFiles  []string      // optional; captured into the artifact store on commit (spec §8)
    IdempotencyKey *Template    // optional
    Retry        *RetryPolicy
}
type AgentStep struct {
    ID, Container string
    Uses   string               // agent-runtime ref
    With   RawConfig            // opaque map; NOT destructured by core
    OutputSchema *JSONSchema    // required iff outputs referenced downstream
    OutputFiles  []string       // optional; captured into the artifact store on commit
    Skills      *StepSkillRouting // optional; route and stage selected skill dirs before launch
    Timeout     *time.Duration
    IdempotencyKey *Template
    Retry  *RetryPolicy
}
type SignalStep struct {
    ID, Await string
    Timeout *time.Duration
    OutputSchema *JSONSchema
}
type CallStep struct {
    ID, Call string              // import id
    Input map[string]Template    // validated against imported workflow input
    InputFiles map[string]string // child public name -> artifact ref
}
type Parallel struct { Children []Node }
type Loop struct { Until *Expr; MaxIters *int; Body []Node }
type If   struct { Cond Expr; Then, Else []Node }
type Try  struct { Do, Catch, Finally []Node }
type Gate struct {                      // the evaluator-optimizer (spec §5.5)
    Generate []Node                     // generator (and repair) steps
    Evaluate []Node                     // independent judge block; verdict = last node's typed output
    Until    Expr                       // pass condition over the verdict
    MaxAttempts int
}
type Skip struct { Reason string }      // early exit (spec §5.6)
type Map  struct {                      // dynamic fan-out (spec §5.7)
    ID          string                  // optional named aggregate product
    Over        Expr                    // runtime-sized typed array
    As          string                  // element binding name
    Container   string                  // per-item instance template
    Concurrency int
    MinSuccess  *Ratio                  // optional; default = all
    Body        []Node
}
```
`CodeStep`/`AgentStep` drop the old `Success *Expr` — quality is the gate's job, not a per-step predicate. Step outcomes are mechanical only (§6).

`RawConfig` is `map[string]any` preserved verbatim. The core never reads its keys; the named adapter validates it (§8).

### Node addressing (`engine/path`)
Pure function `Path(parent, node, siblingIndex, branch) string` implementing spec §4: step nodes by `id`; call children by `<call-id>.workflow.<child-path>`; control nodes by `keyword[index].branch` (`try[0].catch`, `if[2].else`, `loop[0].body.iter-3`, `gate[0].attempt-2.generate`, `parallel[1]`). Used as the journal key and OTel `awf.node.path`; runtime call paths must come from `engine/path`, not ad hoc string construction in the call executor.

### Container lifecycle (`container`)
One instance per declared name, created lazily on first reference and kept for the run (spec §3). The `engine` requests `Create` once per name, reuses the `Handle` across every step naming it, and `Destroy`s at run end or cancellation. Loop iterations on one container accumulate state; parallel branches must name distinct containers (validation rule below).

### Validation (`ir`)
Pure over a `LoadedDefinition` the `loader` already read (imported workflow files, compose files, and asset bytes included) — `validate` itself does no I/O. Enforces: exactly one of `run`/`uses`/`await`/`call`; `imports:` paths are relative `.awf.yaml` slash paths confined to the declaring workflow's module dir with no remote, absolute, backslash, control-character, `..`, or symlink-component traversal; call refs resolve to imports and expose only the imported workflow's `output_schema` fields and named `output_files` aliases; a container declares exactly one of `image`/`compose`, image is a digest not a tag, **every `image:` inside a referenced compose file is digest-pinned**, and `service` is set + resolvable for compose-backed containers; loop has `until` or `max_iters`; map has `over`/`as`/`container`/`concurrency`; gate has non-empty `generate`, an `evaluate` whose final node declares `output_schema`, `until`, and `max_attempts`; unique step ids and map aggregate ids in one namespace, with aggregate output ids rejected where they would duplicate sibling step ids and make `step.<id>` ambiguous; container refs resolve; `output_schema` present iff a `step.<id>.<field>` of this step, call product, or map aggregate is referenced; workflow `outputs:` match the top-level `output_schema` and top-level `output_files:` aliases resolve to named internal artifacts; step `input_files` refs resolve to `step.<id>.files.<name>`, `asset.<id>`, or `input.files.<name>` inside a called workflow, call `input_files` refs bind imported workflow public file input names to parent artifact refs, and workflow `input_files` declares those public file input contracts; top-level workflow input_files contracts use AWF1050; call input_files binding shape uses AWF1051; invalid artifact RHS refs remain AWF3007; output_files contract metadata remains AWF3009; top-level `skills:` corpora reference directory assets, use only `layout: skill_dirs` and `router: bm25`, and each child skill directory contains `SKILL.md`; agent-step `skills:` refs resolve to a declared corpus, carry a valid `query` template, set `limit` positive and <= 64, set `into` to an absolute clean path other than `/`, require a container, and reject `input_files` destinations that overlap `into` in either direction; named `output_files` contract objects have `path`, use only `format: json|jsonl`, require `format` when schema-bearing, and do not set both `schema` and `schema_ref`; **parallel branches (and `map` items) that run steps use distinct containers / compose projects** (spec §5.4); `input`/`output_schema` are valid **JSON Schema 2020-12**, with a warning when an agent `output_schema` uses features outside the conservative cross-backend floor (spec §7). Well-formedness is checked with `santhosh-tekuri/jsonschema/v6` `Compile()`; the floor is a hand-rolled warning-level check; compose digest-pinning is verified by walking the `compose-go/v2` model. Returns diagnostics with node paths (`Diagnostic{severity, path, code, message}`; collect-all, non-zero exit only on errors). `code` is a stable namespaced enum (`AWF1xxx` structural / `AWF2xxx` schema-floor / `AWF3xxx` digest-loader), an API for `--format json` consumers — never renumbered.

### Event log (`state.Log`)
Append-only sequence per run. One event:

```go
type Event struct {
    Seq      uint64
    Epoch    uint32       // run/resume invocation index
    TS       time.Time    // from Clock
    NodePath string
    Type     EventType
    PayloadRef string     // CAS pointer, optional
}
```

Event types: `run.started{input_ref, workflow_digest, backend, runtimes, assets, definition_ref}`, `run.finished`, `run.paused`, `run.skipped{reason}`, `skills.selected{library, library_digest, router, router_version, router_params, selected[]}`, `node.started`, `node.completed{outputs_ref, files_ref, snapshot_ref?, outcome}`, `node.failed{outcome, error}`, `call.started{input_ref,input_files,runtimes}`, `branch.taken{which}`, `loop.iter{n}`, `map.item{n, image_digest?, reason?}`, `retry.attempt{n, error, outcome}`, `signal.received{signal, payload_ref}`, `io.chunk{stream, ref}`, `agent.event{ref}`, `agent.tool.call/result`. Control-flow decisions that affect the resume path (`call.started`, `branch.taken`, `loop.iter`, `map.item`, `signal.received`, `run.skipped`) and skill-selection decisions (`skills.selected`) are events so the re-walk is deterministic.

Local impl: one append-only file per run, **etcd-WAL-style framed records** (`[8-byte-aligned length][CRC32C][payload]`). **fsync at durability-critical events** (`node.completed`, `call.started`, `run.*`, `signal.received`, `skills.selected`); `call.started` must be durable before any child frontier runs. High-frequency stream events (`io.chunk`, `agent.event`) are buffered and ride the next fsync (non-authoritative — a torn tail is dropped on fold). The fold scans from the start and stops at the first short read / CRC mismatch (torn-tail recovery). No persisted offset index — derivable by scanning; rebuild an in-memory index on open only if scan cost bites. Segmenting (etcd rotates its WAL at 64 MB) is deferred — chunk data lives in `Blobs`, so the log grows with event count, not payload.

### Blob store (`state.Blobs`)
Content-addressed; the hash sits **behind the `Blobs` interface** — v1 uses sha256 (OCI-aligned, hardware-accelerated), with BLAKE3 the upgrade path if hashing large blobs dominates profiles. Payload classes, one store: **artifacts** — run input, captured stdout, typed outputs, declared `output_files`, signal payloads, tool args/results, `io.chunk` data; and (only for `snapshot: workspace` containers) CoW FS diffs. Content addressing gives dedup + the "artifact exists" predicate the §8 atomicity guarantee needs.

### RunState
The in-memory fold of the log: per-node outcome + outputs, `CallStarted[path]`, branch decisions, loop counts, current epoch, run input. Built by replaying the log; consulted by the interpreter (skip completed nodes), call executor (reuse call inputs and runtime pins), and `template` (resolve refs).

---

## 5. Execution model

### Run start
Load imports + validate → compute `Digest` → resolve each `uses:` to a concrete adapter **identity + version** → select a concrete backend → snapshot top-level assets into `Blobs` → open log → append `run.started{input_ref, workflow_digest, backend, runtimes:[{ref,version}], assets, definition_ref}` (input validated against `Workflow.Input`). `definition_ref` is a **view-only** snapshot: the run's full canonical `LoadedDefinition` (JSON) stored once in `Blobs` (content-addressed, so identical definitions across runs dedup to one blob) so a reader — `awf ui` overlaying a past run — can render the run against the exact structure it executed against, even after the on-disk file is edited. It is **never** consulted for resume or pinning, which always re-read the live file (spec §8); `omitempty`, empty in pre-snapshot logs (readers fall back to the current file then). `--backend auto` selection belongs in CLI run before backend construction and before `run.started`: auto records `native` unless Docker-only workflow features are present, in which case it records `docker`. Docker-only features that auto routes to docker include static image-backed containers, compose-backed containers, workspace snapshots, runtime compose, and runtime map images — auto prefers docker there for a pinned, reproducible baseline. When auto records native it prints a caveat that resume restores `snapshot: workspace` workdirs but does not pin the host base environment. Explicit `--backend native` is broader: it *runs* static image-mode and `snapshot: workspace` workflows on the host (ignoring the declared image, with a no-isolation warning) and rejects only compose, runtime compose, and runtime map image (`cli/backend_features.go`). On resume, the on-disk resolved definition's digest (including current imported workflow files and asset bytes) **and** every resolved runtime version **MUST** equal the logged values, else hard error (spec §8); resume uses the backend kind recorded in `run.started`, never re-runs auto-selection. Native runs are resumable: committed steps are replayed, `snapshot: workspace` workdirs are restored from their last committed archive, and other containers are recreated fresh; resume preserves checkpoint integrity but not the host base environment (a one-line caveat prints on native resume).

### Tree-walking interpreter (`engine`)
Recursive walk of `Graph`. Sequential composition = iterate the slice. For each node:

1. Compute `node.path` (addressing function).
2. **Resume check:** if RunState shows this path `completed` in a prior epoch, skip and replay its recorded outputs. (Identical code path on first run and resume.)
3. Else execute, classify the outcome (§6), and at a step boundary run the **commit** (below).

Control nodes:
- `parallel`: dispatch children concurrently under a `context.Context`. A child raising after its retry budget cancels the `ctx`; siblings stop and the error propagates (§6). Commits from concurrent branches are serialized through the single log writer (the `Seq` counter orders them). (Distinct-container validation guarantees concurrent snapshots don't collide.)
- `loop`: evaluate `until` via `template`; emit `loop.iter{n}`; each iteration is a commit boundary.
- `if`: evaluate `cond`; emit `branch.taken{then|else}`.
- `try`: run `do`; on escaping error run `catch`; always run `finally` (incl. on cancellation); a `finally` error propagates.
- `await` (signal step): register with `SignalBroker`; block until delivery or `timeout`. Signals are durably journaled on receipt (even before the `await` is reached) and buffered per name, so an early signal is consumed, not lost. Delivery → `signal.received`, payload validated against `output_schema`, becomes outputs.
- **`call`**: a fresh call evaluates typed input, resolves and validates file inputs, stores the typed input blob, creates child runtime handles, resolves child runtimes, then appends and fsyncs `call.started{input_ref,input_files,runtimes}` before entering the child graph. Fold materializes `RunState.CallStarted[path]` so resume reuses the exact call input and checks call-level runtime drift before replaying the child frontier. Child workflow nodes execute under `<call-id>.workflow.<child-path>`. The call node commits at `<call-id>` with typed outputs and file aliases. A typed input blob may become an orphan CAS blob if the process crashes before `call.started` is appended. That is acceptable: the invariant is content-addressed bytes before a durable pointer, never a durable pointer to missing bytes.
- **`gate`** (spec §5.5): loop { run `Generate`; run the `Evaluate` block; verdict = its last node's typed output; test `Until` }. Pass → continue. `until` false → repair. Exhaust `MaxAttempts` → `rejected`, propagates (§6). **Crash ≠ verdict:** a mechanical failure of any generate/evaluate step is handled by that step's own `retry`; if it still fails, the gate propagates the failure and does **not** consume a repair attempt (you can't repair a crash, and a broken judge is never a rejection). Only a successful evaluation with false `until` consumes an attempt. Two properties are **enforced by the engine**: (1) **independence** — an agent evaluator launches in a *fresh context* (new session; it shares the container fs but not the generator's conversation), so the judge must test behavior not trust artifacts; a code evaluator is independent by construction. (2) **auto-feedback** — on attempt > 1 the engine passes the latest verdict as `AgentInvocation.Feedback` / resolves `{{ evaluate.<field> }}`; on attempt 1 it's empty. Each generate/evaluate is a commit boundary (`gate[i].attempt-N.*`); a crash mid-gate resumes at the right attempt.
- **`skip`** (spec §5.6): unwind to the nearest enclosing `loop`/`gate` iteration, `parallel` branch, or (if none) the run root, marking it `ok`; run `finally` blocks passed on the way (same unwind path as a propagating error, terminal-ok rather than failure). Inside a `parallel` branch it ends only that branch. Record the skip (`run.skipped{reason}` / iteration/branch-skip) in the log so resume reproduces it.
- **`map`** (spec §5.7): evaluate `over` to a typed array; for each element create a distinct per-item container instance and run `body` (element bound as `{{ <as>.* }}` / `{{ <as>.index }}`), at most `Concurrency` in flight; emit `map.item{n}`; each item is a commit boundary (`map[i].item-N.*`). Fan-in succeeds when `MinSuccess` is met (default: all); otherwise propagates like a failed `parallel`. A `map` may carry `image:` (a template) supplying each element's container image from the worklist (P6a); the runtime renders it per item, boots it, and records the booted content digest on `map.item`. An unrenderable/unbootable runtime image is a per-item `item_failed` with a `reason` (`image_render_failed` / `image_unavailable`), tolerated by `MinSuccess`, not a whole-map abort. **Fan-IN (shipped):** a `reduce:` clause collapses the surviving branches into ONE output committed at the map's aggregate path — a `quorum` verdict `{passed,votes,agree}`, or an author `run:` reducer fed every branch's named `output_files` (the SP1 artifact channel) — and a downstream `step.<map-id>.<field>` then resolves against the reducer's output, not the per-item array; `step.<map-id>.files.<name>` resolves to reducer artifacts. Maps without `reduce:` may expose the compact array of the final body step's typed output for another map's `over:`. Named aggregate refs belong in IR validation and the existing scope/reference helpers; do not introduce a product registry or a second runtime ownership model. A `prune:` clause (`keep: top(k)` / `stop_when`) discards losing branches mid-flight by a numeric score; a pruned branch is the third disposition `item_pruned` (neither pass nor failure, removed from the `MinSuccess` denominator and the quorum cohort), and the WHOLE frontier commits atomically as one `map.frontier` event so resume replays it verbatim, never re-derived. Per-item body-step addressing goes through `engine.ItemStepPath`.

`input_files` staging belongs in the interpreter because it resolves committed
artifact refs and recorded run-start asset refs from folded state before
dispatch. `asset.<id>` always stages the blob refs recorded in `run.started`;
resume verifies the live definition digest first, then stages from the recorded
snapshot, not from the current asset path. For a containerless adapter the
interpreter does not bind-mount anything; instead it loads the resolved bytes
into `AgentInvocation.InputFiles` so the adapter can forward them as inline
message parts.

For an agent step with `skills:`, the interpreter performs skill routing before
`RunWithRetry`. On a fresh step it renders `skills.query` with the normal step
scope, runs `skillroute` against the recorded run-start corpus snapshot, and if
no skill scores above zero returns a permanent step failure before dispatch and
before appending `skills.selected`. Otherwise it appends and fsyncs
`skills.selected`, then uses the recorded ids. On resume it reuses recorded ids
instead of rerouting, validating replay against the pinned run-start corpus
digest plus router name, router version, and router params. The interpreter
materializes selected skill files from the run-start assets under
`<into>/<skill-id>/...`, merges those files with `input_files`, performs one
destination collision/overlap check, and passes only resolved bytes to the
dispatcher for staging.

### Interpreter ↔ Dispatcher seam
The interpreter emits a `NodeIntent{path, node, resolvedInputs}` and consumes a `NodeResult`. It never touches Docker or a harness directly.

```go
type Dispatcher interface {
    Run(ctx context.Context, intent NodeIntent) (NodeResult, error)
}
```

`LocalDispatcher` runs code steps via `container.Backend.Exec`, agent steps via `agent.Adapter.Launch`, and signal steps via `signal.SignalBroker`, in-process. It enforces `timeout` via `context.WithTimeout`. It also validates captured artifacts after backend capture and before `OutcomeOK`: schema-bearing `output_files` contracts are checked there, including JSON/JSONL parsing and schema validation, and invalid captures become mechanical failures. Only validated captures flow back to the interpreter for commit/blob writes and `node.completed`. A future `QueueDispatcher`/`K8sDispatcher` drops in with no change above it. The interpreter owns the log, the commit boundary, staging, and retry orchestration; the dispatcher owns execution mechanics and capture validation only.

### Commit boundary (spec §8)
A single operation, the *only* way a node is recorded complete:

```
Commit(path, outputsRef, fileRefs, outcome):
   1. typed outputs + output_files already materialized in Blobs (CAS ⇒ existence is durable & verifiable)
      (for a snapshot: workspace container, a CoW FS diff is materialized here too)
   2. append node.completed{outputsRef, fileRefs, snapshotRef?, outcome}; fsync
```

The journal entry references artifacts that provably already exist, and the entry's existence *is* the completion record. A crash between (1) and (2) leaves orphan blobs (GC-able), never a completion without its artifacts. The durable unit is the artifact, not a live container — infrastructure is rebuilt from the recipe on resume.

For a call, the child graph commits its own internal nodes under the call path.
The public call node commits only the imported workflow's explicit typed outputs
and named file aliases.

### Resume = fold the log (spec §8)
`awf resume <run.id>`: verify definition digest **and** runtime versions; open a new epoch; fold the log into RunState; recreate each live container from its image/compose recipe (readiness re-runs via entrypoint / `up --wait`; a `snapshot: workspace` container restores its last committed CoW diff instead); re-enter the interpreter at the root. **Committed nodes are replayed from the journal — recorded outputs reused, not recomputed.** Only the **uncommitted frontier** re-executes — the in-flight node on each active branch (one under sequential flow, several under `parallel`, or a child frontier under an uncommitted `call`). For calls, folded `call.started` records reuse the exact input blob and runtime pins before the child frontier is replayed; drift is a hard error. For an agent step that re-execution may yield a *different* result (nondeterministic, and acceptable — its partial work was never committed). Committed events (their timestamps and the run id included) are **replayed from the log, not recomputed**; re-executed frontier nodes get fresh time/ids, which is correct (uncommitted). Branch/loop/signal/skip events force the same path.

### Cancellation (spec §8)
`awf cancel <run.id>` cancels the root `context`. In-flight dispatches observe the cancellation and stop; enclosing `finally` blocks run; the container instances are `Destroy`ed; a terminal `run.cancelled` is appended. A cancelled run is not resumable (the fold sees the terminal marker and refuses).

### Pause / breakpoint (spec §8)
`awf pause <run.id> [--before <node-path>]` stops dispatching new nodes at the next commit boundary (or when the interpreter reaches `<node-path>`), appends a non-terminal `run.paused`, and — unlike cancel — **does not** `Destroy` containers, so an operator can inspect the live workspace, the committed artifacts, and the trace. `awf resume` clears the marker and continues in a new epoch (same fold path; the still-running containers are reused, or recreated from recipe if they were lost). This is the breakpoint mechanism; there is no IR breakpoint node.

---

## 6. Outcomes & retry

### Outcome classification (`engine` + `retry`)
Step outcomes are **mechanical only** (spec §6); quality is the gate's job:

| Outcome | How detected | Retryable |
|---|---|---|
| `ok` | clean exit / schema-valid / signal delivered | — |
| `retryable_failure` | launch/transport error, `timeout`, nonzero exit (not declared permanent), unparseable agent output | yes (policy) |
| `permanent_failure` | exit in `non_retryable_exit_codes` (default `[78]`); agent refusal/policy block | no |

Recorded on `node.completed`/`node.failed`, emitted as `awf.node.outcome`. A **gate** additionally yields `rejected` when `Until` stays false after `MaxAttempts`.

**Propagation (spec §6).** A step that exhausts retries as a failure, or a gate that exhausts attempts as `rejected`, returns a typed `OutcomeError`. The interpreter propagates it to the nearest enclosing `try` (running `catch`), cancels parallel siblings on the way, and halts the run if no `try` encloses it.

### Retry vs repair — two axes
- **Retry** (`retry`) wraps each dispatch for *transient* faults. Default `{attempts:3, backoff:exp, initial:1s, max:60s, non_retryable_exit_codes:[78]}`; `retryable_failure` retries, `permanent_failure` doesn't. Backoff via `Clock`. Emits `retry.attempt`.
- **Repair** is the gate (§5 control nodes) for *quality* faults: regenerate conditioned on the judge's feedback. A step can be retried for flakiness *and* sit in a gate that repairs it for quality; they compose.

### External effects (`idempotency_key`)
On each attempt, the resolved `idempotency_key` (if declared) is passed to the step — env var `AWF_IDEMPOTENCY_KEY` for code steps, or threaded into agent `with`/env. The runtime does not dedupe; the external system does. No compensation/rollback machinery (spec §10); cleanup is the author's `try/finally`.

---

## 7. Container backend (`container`)

```go
type Backend interface {
    Capabilities() Caps                 // {Snapshot: [none, fs-cow, fs-archive], RuntimeImage: bool}  (RuntimeImage = P6a)
    Create(ctx, ContainerSpec) (Handle, error)   // image OR compose; brings to readiness (entrypoint / up --wait)
    Exec(ctx, Handle, Cmd) (ExecResult, <-chan IOChunk, error)  // ctx carries timeout; streamed output
    CaptureFiles(ctx, Handle, paths []string) ([]CapturedFile, error) // output_files → bytes; interpreter Puts at commit boundary
    Snapshot(ctx, Handle) (SnapshotRef, error)   // only for snapshot: workspace containers
    Restore(ctx, SnapshotRef, name string) (Handle, error)   // name = IR-declared container name (slice 4.4)
    Destroy(ctx, Handle) error
}
```

`Create` takes a `ContainerSpec` that is either a digest-pinned image or a compose project + primary `service`; it returns once the container/project is healthy. Re-creating on resume re-runs readiness — there is no separate snapshot to restore on the common path.

For a `map`'s runtime-resolved per-element image (P6a), `Create` reports the booted image's content digest on `Handle.ResolvedImageDigest` (empty for a static image or the native backend); the engine records it on `map.item`. `Caps.RuntimeImage` advertises whether a backend can honor a runtime image at all — native is `false` (it ignores `image:` and runs on the host), so the CLI guard (`cli/runtimeimageguard.go`) rejects a runtime-image workflow there before run start. docker is `true`: `Create` pulls a map's runtime-resolved image by digest before booting it (P6a), and — on a cold cache — also pulls a *static* digest-pinned image, retrying `ContainerCreate` once after a not-found error (F27); either way the booted digest is content-addressed, never a mutable tag. `Caps.Snapshot` for native is now `fs-archive` (was `none`) when the backend is constructed with blobs (`container/native/backend.go`): native captures a full gzip-tar workdir archive — not a CoW diff — so `snapshot: workspace` restores on native resume; with nil blobs it stays `none`.

**Docker impl.** One long-lived instance/project per declared name, driven **programmatically** via the Docker Engine SDK + the compose Go API (`compose-go` / `docker/compose`) — **not** by shelling the `docker` CLI (no binary-in-`$PATH` dependency, structured errors, programmatic readiness waits; integration tests use the same Docker SDK directly — slice 4.1 plan Design Q1 dropped the original `testcontainers-go` choice as redundant with what the Backend itself does). Image-backed: create + start from a digest. Compose-backed: bring the project up run-scoped with `--wait` semantics (compose owns networks/`depends_on`/`healthcheck`); exec routes to the named service. `Exec` streams stdout/stderr as `IOChunk`s (feeds live tap + `io.chunk` events), injects `AWF_OUTPUT`/`AWF_IDEMPOTENCY_KEY`, honors the ctx deadline. `CaptureFiles` copies declared `output_files` out and returns the captured bytes; the interpreter Puts them to the artifact store at the commit boundary. `Snapshot`/`Restore` are used **only** for `snapshot: workspace` containers, via a CoW layer diff against the base image (containerd snapshotter / overlay diff) stored as a CAS blob — **not** `docker commit` (avoids layer bloat + non-reproducibility). `fs`-level only.

**Fake impl** (tests): in-memory filesystem map; `CaptureFiles` reads from the map; `Snapshot`/`Restore` clone it. Exercises the §8 guarantees without Docker.

**Snapshot/Restore impl** lives in `container/docker/snapshot.go` (slice 4.4). Streaming gzip-tar via `ContainerDiff` + `CopyFromContainer` through `state.Blobs`; 3-segment SnapshotRef `<blob-ref>@<image>@<base64-json-of-cmd-entrypoint>` embeds runtime config so Restore re-creates faithfully. Restore streams via `io.Pipe` (one goroutine; goleak coverage from slice 4.1). `Backend.Snapshot` materializes the diff blob in `Blobs` and returns a ref; the interpreter then records the ref on `node.completed`. This keeps the *log* single-writer invariant intact (the Backend appends no events) and matches the SotA executor-writes-CAS-blob / orchestrator-records-ref pattern (containerd, BuildKit); the `CaptureFiles`-returns-bytes asymmetry is structural — a `SnapshotRef` is intrinsically a ref.

- **CLI Backend selection (slice 4.5 + auto follow-up)** lives in `cli/backend.go`.
  `--backend {auto,fake,docker,native}` is resolved before backend construction
  and before `run.started`: `auto` selects native unless Docker-only workflow
  features are present, otherwise docker. Docker-only features include static
  image-backed containers, compose-backed containers, workspace snapshots,
  runtime compose, and runtime map images. The selected concrete kind is recorded
  in `engine.RunStartedData.Backend`. Resume reads that kind from the journal via
  `readBackendKindFromLog` and never re-runs auto-selection. A private
  `newBackend(ctx, kind, runID, blobs)` function switches only on concrete kinds
  (`fake`, `docker`, `native`). Production constructs the docker client via
  `client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())`
  and threads it into `docker.New`. Tests inject `cli.Runner.Backend` directly;
  the private helpers are never invoked on the test-injection path.
- **Concurrent-safe log read (slice 4.5)**: `state.FoldFile(path)` is the
  observer-only entry point for reading a log that a writer goroutine
  may be actively appending to. Unlike `state.OpenLog`, it never
  truncates torn tails (read-only `os.Open` + `scanFile`). Used by
  slice 4.5's pause-resume integ test's poll helper.

The Phase 4 conformance entry for Docker is `conformance.RunDockerSuite(t, factory)` in
`conformance/docker_suite_test.go`. It runs Buckets 9/10/11 against any
`DockerBackendFactory`; the production wiring lives in
`conformance/conformance_docker_test.go` and uses `docker.New`. Both files are
double-gated (`_test.go` + `//go:build integ`) so they only compile under
`go test -tags integ`. The base `conformance.RunSuite(t, factory)` (Bucket 1-8 against
fake) is unchanged and remains the fast Docker-free green bar.

---

## 8. Agent orchestration (`agent`)

```go
type Adapter interface {
    Ref() string                                   // e.g. "anthropic/claude-code"
    Version(ctx) (string, error)                   // resolved at run start; pinned for replay (spec §8)
    ValidateConfig(with RawConfig) error           // adapter owns its with-schema
    Launch(ctx, container.Handle, AgentInvocation) (<-chan AgentEvent, <-chan AgentOutcome, error)
}

type AgentInvocation struct {
    With         RawConfig
    OutputSchema *ir.JSONSchema   // nil ⇒ no typed output required
    IdempotencyKey string         // "" if none
    Feedback     map[string]any   // gate repair: prior verdict, injected into context (nil on attempt 1)
    InputFiles   []InputFile      // resolved file bytes for containerless adapters; empty for container-backed steps
}
type AgentResult struct {
    Output  map[string]any        // validated against OutputSchema (or nil)
    Metrics MetricSet             // best-effort, provenance-tagged (gen_ai.* + awf.cost.*)
    TranscriptRef string          // CAS pointer
    FinishReason string
    Refused bool                  // ⇒ permanent_failure
}
```

**Registry.** `uses:` resolves to a registered adapter at run start; resolution is logged. On resume, an unresolvable ref fails (spec §8). A top-level `agents:` role registers a `DerivedAdapter` under the role name at run start: it wraps the role's base `uses:` adapter and folds the role's convenience fields (`model`, `system_prompt`) plus its `with:` into a key-blind overlay a step's own `with:` then shallow-merges over (step wins). The role is pinned like any runtime — its base version is drift-checked on resume.

### Adapter capability matrix

`Caps` are static runtime declarations. The core may use them for runtime
pinning, container requirements, gate guards, resume preflight, and docs, but
provider-specific behavior stays inside the named adapter and its opaque
`with:` config. `ContextEvidence` means the adapter can render engine-assembled
source context as untrusted evidence without treating it as active prior
conversation. Normal conversation continuation still uses `Threaded`.

| Runtime ref | Mode | Native schema | Containerless | Threaded | Context evidence | Persistent session | Status |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `anthropic/claude-code` | strict CLI | yes | no | no | no | no | built-in |
| `factory/droid` | strict CLI | no | no | no | no | no | built-in |
| `block/goose` | strict CLI | no | no | no | no | no | built-in |
| `openai/codex` | strict CLI | yes | no | no | no | no | built-in |
| `awf/llm` | single HTTP call | no | yes | yes | yes | no | built-in |
| `openai/codex-live` | live app-server | yes | yes | no | no | yes | built-in |
| `block/goose-live` | live ACP | no | yes | no | no | yes | reserved implementation-track ref; no adapter registered yet |
| `anthropic/claude-code-live` | live PTY proof spike | yes | yes | no | no | yes | deferred; not supported |

The registered Codex live ref uses the same `uses:` resolution and
run-start/runtime-version pinning path as strict refs. Live refs do not add a
separate runtime registry or inspection command; live output continues through
`AgentEvent`, `agent.event`, `awf trace`, and UI projections. The Goose live ref
remains reserved, and the Claude Code live ref stays deferred until the PTY
proof spike proves turn-boundary, permission, transcript-correlation,
prompt-injection, and reconnect behavior.

**Claude Code adapter (first).** Runs the CLI inside the container: `claude -p <prompt> --model <m> --output-format stream-json [--max-turns N] [tool allowlist]`. Streams stream-json → `AgentEvent`s for the live tap and `agent.*` span events. Reads `total_cost_usd` + `usage` → `MetricSet{cost.source=reported, gen_ai.usage.*}`. Maps `with` keys and `mcp://` tool refs to the harness's MCP config; passes the idempotency key into the agent env.

**Typed output (spec §4.2).** Layered: (1) the harness's structured-output mode when exposed — Claude Code's `--json-schema` is the Phase 5 path, the adapter forwards the step's `output_schema` verbatim and reads `result.structured_output`; (2) schema-aligned parse of the final message — for *future* adapters lacking native schema validation, implemented as a structuring call (a separate stateless LLM invocation with a forced tool call against `output_schema`; pattern pinned in Phase 5 design Appendix H, not shipped in Phase 5); (3) bounded re-prompt — internal to Claude Code's `--json-schema` loop in Phase 5; external for future structuring-call adapters; (4) else `retryable_failure` (unparseable output, surfaced as `*agent.ErrUnparseableOutput`). The conformance suite pins the layer-2 contract via Bucket 15 (`Caps{NativeSchema: false}` against a test-supplied extractor closure) even though Phase 5's only real adapter is layer-1+3. `Output` fields are what `template` binds to — never raw text.

**Goose/Codex adapters (later).** Same interface; report tokens but not dollar cost → `MetricSet{cost.source=derived}`. Derived cost is deferred (see §10): no `pricing` package ships until a token-only adapter actually needs it, and it will derive only from a run-start-pinned, log-recorded table.

**Independence for free.** Each agent step is its own `claude -p` invocation with **no** `--resume`/session sharing, so a gate's generator and evaluator never share a conversation — the spec's enforced independence (§5.5) falls out of how steps are launched. On a repair attempt the engine passes `Feedback` (the prior verdict), which the adapter renders into the prompt; the generator thus sees the critique but not the evaluator's reasoning.

---

## 9. Templating (`template`)

Two operations, nothing more (spec §7):

- **Substitution:** `{{ run.id }}`, `{{ input.<field> }}`, `{{ step.<id>.exit_code|stdout }}`, `{{ step.<id>.<field> }}` from RunState; inside a gate's `generate`, `{{ evaluate.<field> }}` resolves to the latest evaluator verdict (empty/null on the first attempt). Typed `<field>` refs (code via `$AWF_OUTPUT`, agent, signal payload) resolve to typed values. A reference to a step inside a loop resolves to its latest completed iteration. Values exceeding the inline limit are rejected at resolution (not truncated).
- **Condition evaluation** for `if.cond`/`loop.until`/`gate.until`: bounded evaluator over references, literals, comparisons (`== != < <= > >=`), boolean ops (`&& || !`). No arithmetic, calls, or loops. `loop.until` is evaluated do-while (after each iteration).

Tiny lexer→parser→evaluator. Because refs are typed, comparisons operate on real numbers/enums; the fragile-string problem cannot occur.

---

## 10. Observability (`obs`)

A **consumer of the log**, not a parallel path.

- **Trace:** a deterministic projection `obs.Project(events, blobs) → []Span` over the folded log; the span tree mirrors the `engine/path` addressing tree. A **step** span STARTs on `node.started` and FINALIZES on `node.completed`/`node.failed`; an unfinalized `node.started` projects as a **Pending** span (the in-flight or crashed frontier). Control scopes (`if`/`loop`/`try`/`parallel`/`gate`/`map`) are synthesized to enclose their children. AWF-specific attributes live under `awf.*` (the stable contract — `awf.workflow.id/version/digest`, `awf.run.id/epoch`, `awf.node.path/kind/outcome`, `awf.node.duration_ms`, `awf.scope.kind`, `awf.exit_code`; agent: `awf.cost.usd`, `awf.cost.source`, `awf.agent.turns`; gate: `awf.gate.attempt`, `awf.gate.attempts`, `awf.gate.outcome`; run root: `awf.run.cost.usd`). GenAI fields reuse OTel **`gen_ai.*`**, confined to one swappable mapping file (`obs/attrs.go`, semconv **v1.41.1** — that file is the single source of truth for the exact attribute set): `gen_ai.usage.input_tokens/output_tokens`, `gen_ai.usage.cache_read.input_tokens`, `gen_ai.usage.cache_creation.input_tokens`, plus `gen_ai.conversation.id` + `session.id` (both the run id, for backend grouping). A `gate.attempt` projects a `gen_ai.evaluation.result` event (with `gen_ai.evaluation.name`). A `skills.selected` event projects the skill library, router metadata, and selected ids/scores; there is no query or query-hash projection. Prompts / agent I/O / typed-output *values* are emitted only behind the opt-in `--capture-content` flag (default OFF). `*_ref` are CAS pointers, never inline.
- **Exporters:** default local file/stdout exporter (zero-infra, used by conformance tests); optional OTLP via env/flag.
- **Live tap:** the CLI subscribes to the dispatcher's `IOChunk`/`AgentEvent` streams and renders them tail-style for the in-flight node. Same streams chunked into the log (large chunks offloaded to Blobs).
- **Live state / run status:** `obs.DeriveStatus(events)` folds the log to `paused` / `finished` / `failed` / `cancelled` / `incomplete` — pure and OS-free. `awf ls` resolves the `incomplete` (started, no terminal event) case to `running` vs `crashed` via a sidecar advisory `flock` the run process holds for its lifetime (the kernel frees it on any death, incl. crash/SIGKILL), keeping the running-vs-crashed split off the log. In a trace, the same `node.started`-without-terminal case is a **Pending** span.

### Cost (the harness's own estimate — no derived cost, no `pricing` package)
`MetricSet` carries `awf.cost.usd` + `awf.cost.source` and `gen_ai.usage.*` token counts; `obs` projects them **verbatim** and **omits** the cost attribute when cost is absent (never a misleading `0`). The reported figure is the harness's **own estimate** — Claude Code's `total_cost_usd` is, per Anthropic's docs, a client-side estimate computed from a price table bundled at build time, explicitly "not for billing or financial decisions." AWF inherits exactly that property: it surfaces the number for dev insight / approximate budgeting, **never** for billing or hard financial gating, and makes zero attempt to improve its precision. Because the harness *version* is pinned at run start (`RunStartedData.Runtimes`), its build-time price table is pinned with it, so a resumed run's estimate is stable. The run root carries a folded `awf.run.cost.usd` rollup (sum of leaf-step costs in sorted-path order, so a `parallel`/`map` is not double-counted). Note: the adapter is responsible for deduping the harness's per-message costs into one `MetricCost` per step (Claude Code shares one message ID across parallel tool calls); by the time a cost reaches `obs` it is already one figure per step.

The `awf.cost.source` domain (`reported` / `derived` / `unavailable`) documents the field for a future derived-cost world; **Phase 6 ships no derived cost and no `pricing` package**. Derived (Σ tokens×rate) cost is deferred to the first token-only adapter (Goose/Codex) that lacks a reported figure; when it ships it will derive **only** from a run-start-pinned, log-recorded table, and `obs` will emit the logged value verbatim — never recomputing at projection or resume.

---

## 11. Determinism (`clock`)

`Clock` (now, sleep) and `IDGen` are injected, never global — for testability (a controllable fake clock/ids in unit tests; `testing/synctest` for concurrency/time) and to keep nondeterminism out of global state. The **run id is the only nondeterministic id**: minted once (crypto/rand) at run start, persisted in `run.started`, and on resume **read from the log, not regenerated**. Every other identity is deterministic — node = addressing path (§4), blob = content hash, epoch = counter — and committed events are replayed from the log, not recomputed, so there is no clock/id *reseed* step. (Agent re-execution is nondeterministic by design — spec §8.) Underpins reproducible conformance tests.

---

## 12. CLI (`cli`)

| Command | Behavior |
|---|---|
| `awf validate <file>` | Recursively load imports + validate IR; print diagnostics; non-zero exit on invalid. |
| `awf run <file> [--input <json>] [--run-id <id>] [--state-dir <dir>] [--backend auto\|fake\|docker\|native] [--agent-env <csv>]` | Execute; print run.id; live tap to console. Default `auto` records native unless Docker-only workflow features select docker (for a pinned baseline). Explicit `--backend native` runs image-mode + `snapshot: workspace` on the host (ignoring the declared image, with a no-isolation warning) and rejects only compose/runtime-compose/runtime-map-image. |
| `awf resume <run.id>` | Verify digest + runtime versions, fold log including call-start records, use the recorded backend, recreate containers from recipe, continue in a new epoch. Native runs are resumable: committed steps replay, `snapshot: workspace` workdirs restore, others recreate fresh; a one-line caveat notes the host base environment is not pinned. |
| `awf pause <run.id> [--before <node-path>]` | Halt dispatch at the next commit boundary (or when execution reaches a node); leave containers up; mark `paused` (non-terminal, resumable). The breakpoint mechanism. |
| `awf signal <run.id> <signal> [--input <json>]` | Deliver a signal (durably journaled, buffered) to a waiting/future `await` step. |
| `awf cancel <run.id>` | Cancel the run: interrupt in-flight steps, run `finally`, tear down containers, mark `cancelled`. |
| `awf inspect <run.id> [--fold <statuses>] [--depth <n>] [--output text\|json]` | Offline: render the addressing tree as a fold-by-status text tree with recorded step durations. |
| `awf trace <run.id> [--otlp <endpoint>] [--capture-content] [--output otel\|json]` | Offline: project the log into OTel spans; export to stdout (default) or OTLP. |
| `awf ls [--output text\|json]` | List runs under `.awf/runs/` with a derived status (running/paused/finished/failed/cancelled/crashed). |

Storage layout: per-run log at `.awf/runs/<run.id>/log`; shared CAS blob store at `.awf/blobs/` (dedup across runs).

---

## 13. Secrets (stopgap only)

Spec §9 leaves secrets open; no subsystem here. Stopgap: named secrets from `--secrets <file>`/host env, **injected as container env at exec only**, never written to the log, redacted from spans. Not exposed to templating. Replaced when the spec resolves §9.

---

## 14. Testing & conformance

- **Unit:** each package against its interface; `template` evaluator table tests; `ir` validation diagnostics; the node-addressing function. Concurrency/time-dependent behavior (`retry` backoff, `timeout`, `parallel` cancellation, `await` delivery, gate attempts) is tested under `testing/synctest` (stable Go 1.25+) for deterministic virtual time; injected `Clock`/`IDGen` remain for replay-reseed determinism.
- **Conformance suite (the bar):** drives the §8 guarantees against the **fake backend**:
  1. *Pinning* — resume against a mutated definition **or** a drifted runtime version is rejected (even when a view-only `definition_ref` snapshot is present, it never bypasses the live-file check). The snapshot itself is exercised too: a run records a resolvable `definition_ref` that reconstructs to the exact definition that ran.
  2. *Exact committed-prefix replay* — run to completion; re-run killing the process after each commit boundary and resuming; assert committed nodes are replayed (not re-executed) and the final RunState matches (modulo epoch tags and the in-flight frontier). Includes a `parallel` case where the crash leaves **several** uncommitted in-flight branches, all re-executed.
  3. *Atomic commit* — inject crashes between artifact-write and journal-append; assert no `node.completed` references a missing artifact, and no artifact-without-completion is treated as complete.
  4. *Propagation* — a `retryable_failure` exhausting retries, or a gate `rejected`, is caught by an enclosing `try.catch` and halts the run when uncaught.
  5. *Gate* — with a fake generator/evaluator: passes when `until` holds; the evaluator launches fresh (no shared session with the generator); repair injects the prior verdict into the next generate (empty on attempt 1); stops at `MaxAttempts` with `rejected`; resumes mid-gate at the right attempt after a crash.
  6. *Skip* — `skip` ends the nearest iteration/run as `ok` and runs enclosing `finally` (lab teardown) on the way out.
- **Integration:** Docker backend smoke tests; Claude Code adapter against a recorded/fake harness for output-schema parsing (incl. malformed `**verdict: pass**` → typed `verdict=pass`); an end-to-end gate where the evaluator rejects a benign-payload exploit; signal early-delivery + buffering + timeout; cancellation runs `finally` and tears down containers.

---

## 15. Build phases

**Phase 0 (bootstrap, prerequisite):** `go mod init` + toolchain pin (`go 1.26` / `toolchain go1.26.4`); `make lint test build`; CI green-bar gate; package skeleton.

1. **Skeleton:** `ir` + `frontend/yaml` (goccy) + `loader` + `validate` + digest; `template` parser; `clock`; `state` (log + blobs). (`engine/path` and the fake backend move to Phase 2 — see the Phase-1 design spec.)
2. **First runnable slice:** tree-walking `engine` + `engine/path` + `LocalDispatcher` for **code steps only** (with `timeout`, `$AWF_OUTPUT` typed outputs, `input`); `template` evaluator; sequential + `if` + `loop`; commit boundary; resume-by-fold + digest check; **fake backend**; conformance suite green on fake backend.
3. **Control flow + the gate:** `if`/`loop`/`try`/`parallel`/`map`; **`gate`** (generate→evaluate→repair, feedback threading, `MaxAttempts`, `rejected`) — the flagship, built early because it's the differentiator; `retry`; `await` + `awf signal`; `awf cancel`/`awf pause` + teardown.
4. **Docker backend:** real containers (long-lived, shared) from image **or** compose project, readiness via entrypoint / `up --wait`, `output_files` capture, streamed `Exec`, env injection; CoW `snapshot: workspace` opt-in.
5. **Agent orchestration** ✅ (slices 5.1–5.4, shipped 2026-05-29): `agent` registry + Claude Code adapter; `output_schema` enforcement via Claude Code's `--json-schema` (layers 1+3 native; layer 2 pinned by Bucket 15 contract for future non-native-schema adapters); gate evaluator independence enforced *structurally* (separate `claude -p` invocations, no `--continue`/`--resume`, `--no-session-persistence` always); engine-side AgentStep dispatch in `engine/local_dispatcher.go runAgentStep`; live event tap (`cli/agent_event_printer.go`); `RunAgentSuite` real-Claude conformance (Buckets 14a + 14c — "gate end-to-end under real claude"). Bucket 14b ships as a unit test in `agent/claude/launch_test.go` (Anthropic's structured-outputs API makes the error path mechanically inducible only via mock). (`engine/gate.go` was untouched — the seam held.)
6. **Observability** ✅ (slices 6.1–6.3): `obs` read-only OTel projection (`gen_ai.*` + `awf.*`); the two additive log writers (`node.started` event + `node.completed.metrics`); gate attempts → `gen_ai.evaluation.result`; run-level cost rollup; default stdout/file + optional OTLP exporters; opt-in `--capture-content`; `awf ls`/`inspect`/`trace`; a live per-step cost line + final run-cost summary on `awf run`/`resume`. **No `pricing` package and no derived cost** — deferred to the first token-only adapter.

From Phase 2 on, each phase keeps the conformance suite green (Phase 0–1 are gated by unit tests; the §14 suite tests execution/resume/gate, which begin in Phase 2).

**Goroutine-leak discipline:** any slice introducing goroutines into a package
X for the first time MUST add `goleak.VerifyTestMain` to `X/main_test.go`
(or equivalent) so leaks surface immediately in `make test`. Slice 4.1
established the pattern for `container/` and `container/docker/`.

---

## 16. Deferred (tracked against spec §9)

- Secrets subsystem (§13 is a stopgap).
- Pricing-table format/versioning portability.
- `map` fan-in — **SHIPPED** (coordinator SP2/SP5): `reduce:` (quorum / author `run:` reducer) collapses survivors to one output at the map path; `prune:` (`keep: top(k)` / `stop_when`) discards losers by score with the `item_pruned` disposition, committed atomically as one `map.frontier` event (resume-replayed). Still deferred: `reduce:`/`prune:` on `parallel` (no durable per-branch status record yet) — `parallel` has no `reduce:`/`prune:` surface in the format.
- Artifact-store / `snapshot: workspace` retention + GC (refcount + TTL). (The
  `snapshot: workspace` capture/restore *wiring* shipped in Phase 7 slice 7.1;
  per-commit snapshot blobs accumulate — bounded by content-dedup to *distinct
  workspace deltas* (the diff tar is deterministic/timestamp-free), with no
  `keepBytes`/TTL cap — until a blob-GC slice. Long `snapshot: workspace` runs
  grow the blob store until then.)
- `vm` snapshot backend (in-memory process state via CRIU/Firecracker) — hard even where it ships (cross-host TCP, GPU); only if a real workload needs it.
- Intra-agent-step (per-turn) resume — agent steps are atomic for now.

(The earlier distributed-dispatch / Kafka-scale seams are intentionally dropped — they contradict the single-host shared-container identity.)
