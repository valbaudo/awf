# AWF correctable-gaps roadmap — design

> Status: design (approved in brainstorming, pending written-spec review). Date: 2026-06-12.
> Author: maintainer-side verification pass over an external feasibility audit.
> Scope: plan the genuine correctable gaps the audit surfaced. P1 is fully specified
> (ships first, gets its own implementation plan); P2 and P3 are scoped roadmap entries,
> each to get its own brainstorm → spec → plan cycle when picked up.

---

## 0. Origin

Another project ("primacasa.ai") ran a feasibility study on converting all of its AI
workflows to AWF and concluded AWF was a poor fit. A maintainer-side verification pass over
that audit — grounded in current `main`, file:line — sorted every raised blocker into four
buckets:

| Audit claim | Verdict |
|---|---|
| No non-Claude tool loop (`awf/llm` rejects `tools`, `Turns:1`; no `react:` step) | **Real, planned-but-unbuilt** → P3 |
| Native-backend runs are non-resumable | **Real, deliberate boundary** → P2 |
| "Not a job queue / durable-execution platform" | **Real, deliberate identity** → Appendix A (WONTFIX) |
| "Therefore AWF hosts nothing; handlers stay black-box TS" | **Category error** → Appendix A |
| "Only runs in containers / needs Docker" | **False** (`awf/llm` is `Containerless:true`) → Appendix A |
| "Fresh-context gate forgoes implicit cache → multiplies cost" | **False** (gate independence ≠ fan-out cache-busting) → Appendix A |
| "Agent steps don't capture `output_files` as durable blobs" | **False** (fixed `a2f990b`, 2026-06-08) → Appendix A |
| "Forces Sonnet on gate/fallback" | **False** (no model imposed) → Appendix A |

One gap the audit *missed* and the verification pass surfaced: **there is no clean way for an
external caller to read a step's typed output back** — `engine.Run` returns `(Outcome, error)`,
the data lands at `node.completed.OutputsRef` in `.awf/blobs/`, and `awf trace --output json`
caps inlined content at 4 KB (`obs/content.go:17`). That is the most load-bearing friction for
the realistic "embed AWF as one step under an existing worker" pattern, and it is the cheapest
to close. It becomes **P1**.

A separate finding: AWF's own research note `docs/research/awf-as-agent-building-substrate.md`
(dated 2026-06-06) still reports gaps **A1** (`agents:`), **A2** (`continues:`), and **A6/G3**
(blob-as-input) as unbuilt — but **SP1/SP2/SP3 shipped them** post-Phase-6 (see `ROADMAP.md`,
"Agentic coordinator — SP1–SP5"). The doc mis-reports our own state; correcting it is folded
into P1.

---

## 1. Scope

Four real correctable gaps, sequenced cheap-first:

- **P1 — `awf output` read-back command + research-doc hygiene.** Read-only, ~days. **Fully
  specified here; ships first.**
- **P2 — native-backend resume.** Roadmap entry. **Recommendation: document the boundary +
  escape hatch now; build the workspace-snapshot only on demonstrated need.**
- **P3 — the tool-loop keystone** (`tools:` block [A4] + `react:` step + intra-step journaling
  [A3]). Roadmap entry. Weeks, format revision, **its own full cycle**.

The non-issues (WONTFIX / false / already-fixed) are recorded in **Appendix A** so this document
is a complete answer to the audit.

---

## 2. P1 — `awf output` + doc hygiene (full design)

### 2.1 Problem

An external orchestrator (e.g. a job handler that shells `awf run <wf> --input <json>` to
execute one AI step, then continues in its own language) cannot cleanly retrieve the result.
`engine.Run` returns only an `Outcome` + error (`engine/interpreter.go:82-91`); the typed data
is content-addressed at `node.completed.OutputsRef` in the blob store (`engine/commit.go:72`,
`engine/events.go:355`). The only read path today is `awf trace`, whose projected content is
bounded to 4 KB (`obs/content.go:17`) — a telemetry surface, not full-fidelity extraction. So
the caller must fold the run log and parse AWF's internal blob layout. P1 gives it a first-class
read command.

### 2.2 Surface

A new read-only subcommand, added as one `case "output":` arm in the dispatch switch
(`cli/cli.go:121-143`), mirroring the existing `ls`/`inspect`/`trace` commands:

```
awf output <run-id> [<workflow-path>] [--step <node-path>] [--file <name>] [--state-dir .awf]
```

### 2.3 Semantics (three forms)

1. **No `--step` — the run's `outputs:` contract (default).** Requires `<workflow-path>`.
   Re-load + validate + digest the workflow file and **refuse on digest mismatch against the
   run's pinned `WorkflowDigest`** — identical to how `awf resume` already guards (`cli/resume.go:209-224`,
   spec §8 hard error). Then `state.FoldFile` the run log → build the root scope → evaluate
   `wf.Outputs` and validate against `wf.OutputSchema`, reusing the **existing** export logic
   (`engine/workflow_exports.go:18`, `evaluateWorkflowExports`) adapted to the root run state
   instead of a sub-workflow child. Emit the resulting map as JSON to stdout.

2. **`--step <node-path>` — any single step's typed output.** **No workflow file needed.**
   `state.FoldFile` already materializes `Completed[path].Outputs` from each step's `OutputsRef`
   (`engine/fold.go:188-199`); look up the node path, emit its output map as JSON. A `--step`
   whose node has no `output_schema` (no typed output) is an explicit error, not empty output.

3. **`--file <name>` — a declared `output_files:` export, as raw bytes.** Part of the
   export-contract form, so it **also requires `<workflow-path>`** (and the same digest guard).
   Resolve the workflow's top-level `output_files.<name>` export — already legal, validated IR
   (`ir/types.go:26`, `AWF1048`) — via the existing `resolveNamedArtifactRef`
   (`engine/workflow_exports.go:63`) → `blobs.Get(ref)` → stream raw bytes to stdout. The
   embedder's "give me the file the workflow produced" path.

**Two forms, not to be mixed.** (1) The *export-contract form* — `<workflow-path>` given; emits
the run's `outputs:` map as JSON, or, with `--file <name>`, the named `output_files:` export as
raw bytes. (2) The *log-only form* — `--step <node-path>`; emits one committed step's typed output
as JSON, no workflow file needed. Combining `--step` with `<workflow-path>` or `--file` is a usage
error. A step's *internal* named files are out of scope for P1 (YAGNI; add later if needed).

### 2.4 What P1 deliberately does NOT touch

- **No new event, no engine write-path change.** `awf output` is a pure read-only projection of
  the same durable log + blobs that resume folds — the same discipline as `obs`/`awf trace`.
- **No validator or format change.** Top-level `outputs:` / `output_files` are *already*
  first-class IR fields (`ir/types.go:25-26`) and *already* validated (`ir/validate_refs.go:91`,
  diagnostic `AWF1048`). They are merely **inert at the top level today** — only evaluated on
  sub-workflow import. P1 gives the existing block a top-level *consumer*; it does not extend the
  grammar.
- **No persistence of the workflow source.** Like `awf resume`, the no-`--step` form takes the
  workflow file as an argument and re-hashes it against the pinned digest. Runs stay as they are.

### 2.5 Errors and exit codes

Following the CLI convention (`cli/cli.go:34-39`: 0 ok / 1 failed / 2 usage):

| Condition | Exit | Message |
|---|---|---|
| Workflow digest mismatch (no-`--step` form) | 2 | mirror `awf resume`'s §8 message |
| No `<workflow-path>` given and no `--step` | 2 | point at `--step` or supplying the workflow file |
| `--step` combined with `<workflow-path>` or `--file` (mixing forms) | 2 | "use either `--step` (log-only) or the workflow-export form, not both" |
| Workflow declares no `outputs:` (no-`--step` form) | 2 | "this workflow declares no `outputs:`; use `--step <id>`" |
| Unknown `--step` node path / not committed | 2 | list nothing; clear "no committed step at <path>" |
| `outputs:` references an uncommitted step | 2 | surface the eval error from `evaluateWorkflowExports` |
| Unknown `--file` name | 2 | "no captured artifact named <name>" |
| Run dir missing | 2 | reuse `requireRunDir` (`cli/util.go:66`) |

`awf output` reads **committed** state; it does not require the overall run to have succeeded
(a committed step is readable even if a later step failed), but an `outputs:` map that references
an uncommitted step errors rather than emitting partial data.

### 2.6 Reused seams (no new machinery)

| P1 piece | Existing function it reuses |
|---|---|
| Subcommand dispatch | `cli/cli.go:121-143` switch + `cli/util.go` helpers |
| Log read | `state.FoldFile` (concurrent-safe; used by `cli/resume.go`, integ tests) |
| Step output read | `engine/fold.go:188-199` (`OutputsRef` → blob → `Completed[path].Outputs`) |
| `outputs:` evaluation + `output_schema` validation | `engine/workflow_exports.go:18` (`evaluateWorkflowExports`) |
| Named artifact resolution | `resolveNamedArtifactRef` (`engine/workflow_exports.go:63`) |
| Digest guard | `cli/resume.go:209-224` pattern (`ld.ComputeDigest` vs `rs.WorkflowDigest`) |
| Blob bytes | `blobs.Get(ref)` |

The one genuinely new code is a thin `cli/output.go` that wires these together, plus a root-scope
analog of `evaluateWorkflowExports` (the existing function builds a *child* scope via
`childRunStateForCall`; the top-level form evaluates against the folded root `RunState` directly).

### 2.7 Testing

Unit tests over a folded fixture log (the `state.FoldFile` + fake-blobs pattern already used in
`cli/*_test.go`), covering: the three forms (`outputs:`, `--step`, `--file`); digest mismatch;
missing-`outputs:`; unknown step/file; and an `outputs:` ref to an uncommitted step. No
conformance bucket is required — the command introduces no new durable state and no new event,
so the deterministic-replay invariant is unaffected. `make lint test` is the green bar.

### 2.8 Doc hygiene (same phase, trivial)

Edit `docs/research/awf-as-agent-building-substrate.md`:

- Mark **A1** (`agents:` block) shipped — SP2.
- Mark **A2** (`continues:` execution) shipped — SP3 (the `AgentInvocation.Thread` field +
  `Caps.Threaded` + the per-adapter prepend, e.g. `agent/awfllm/transport.go:84-88`). The doc's
  §3.2 / §8 "validated-but-not-executed" caveat is now stale.
- Mark **A6/G3** (blob-as-agent-input) shipped — SP1 (`input_files` / named `output_files` +
  `Backend.CopyTo`).
- Add a dated banner at the top: "Since this note (2026-06-06), SP1–SP5 landed; the keystone
  trio A1→A4→A3 is now **A4 + A3 only**." This stops the doc from mis-reporting our own state.

---

## 3. P2 — native-backend resume (roadmap entry)

### 3.1 Gap and root cause

`awf resume` of a `--backend native` run hard-errors (`cli/backend.go:108-109`). Native returns
`SnapshotNone` and its `Snapshot`/`Restore` return `ErrUnsupported`
(`container/native/backend.go:79`). The resume *machinery* (log-fold, frontier re-exec) is
already backend-agnostic; only the reconstruction of the uncommitted frontier's container is
missing for native.

### 3.2 The real tension (the fork)

Docker resume reconstructs the frontier from a **digest-pinned image** — reproducible bytes that
satisfy the §8 pinning invariant. Native has no image; `Create` makes an empty host workdir and
ignores `image:`. A workspace-tar snapshot (mirroring the **already-shipped** docker
`snapshot: workspace` CAS-diff path from Phase-4 slice 4.4, through the same `Snapshot`/`Restore`
+ `Caps` seam) could restore the workspace *files* — but it **cannot pin the host toolchain**. So
native resume is fundamentally a *weaker* guarantee than docker's: workspace-state restored,
toolchain best-effort. The choice:

- **(a) Build it as explicitly best-effort.** Native `Caps.Snapshot` workspace-tar impl + relax
  the `readBackendKindFromLog` guard, documented as "native resume restores workspace, not
  toolchain; §8 pinning is relaxed for native." Effort: **moderate** (one Backend impl + guard
  relax + a conformance bucket).
- **(b) Don't build; document.** Keep native non-resumable; make the limitation, the
  `--backend docker` escape hatch, and the "re-drive a deterministic run" pattern crisp.

### 3.3 Recommendation

**(b) now, (a) scoped as a follow-up on demonstrated need.** Native+resume has near-zero value
for the motivating case: an embedder with its own durable queue (Postgres `SKIP LOCKED` +
orphan-recovery) would use `--backend docker` for anything long, or simply re-drive a
deterministic run. Building (a) means relaxing a load-bearing invariant (§8) for a need nobody
has demonstrated. P2's deliverable today is therefore **documentation**: a short man-page /
README note stating the boundary and the two escape hatches. The workspace-snapshot graduates to
its own slice the moment a real native+resume workload appears.

---

## 4. P3 — tool-loop keystone (`tools:` + `react:`) (roadmap entry)

### 4.1 Gap

The capability that lets an author *build* a native augmented-LLM agent (model + tools + loop) on
the `awf/llm` path, rather than only *wrapping* a pre-built CLI agent. With **A1 (`agents:`)
already shipped (SP2)**, the remaining trio is **A4 + A3**:

- **A4 — `tools:` block.** Top-level map; each tool = `{ name, description, input_schema, impl }`
  where `impl` is a parameterized `run:`/`exec` step. Digest-pinned. Reuses the existing execution
  substrate — no parallel tool runtime.
- **A3 — `react:` composite step kind.** *Distinct* from the atomic `agent:` step (so
  "one invocation = one commit" is untouched everywhere else). The engine adds `tools` to the
  awf/llm request → parses `tool_calls` → dispatches each as its `impl` step → appends
  `ToolMessage` results → loops to `max_turns`.

### 4.2 The two hard parts

1. **Intra-step journaling — the gating unknown.** The log is node-path-granular today; there is
   **no sub-event concept**. `react:` needs each `(model-call, tool-result)` round to be a
   sub-event under the step's node path so resume folds completed rounds and only the uncommitted
   round re-executes. This is the most invariant-sensitive change in the whole roadmap and the
   first thing P3's own brainstorm must resolve.
2. **The request shape is the *easy* part.** `openai-go` v3 (already pinned) exposes
   `params.Tools`, `ToolChoice`, `ToolMessage()`, and the streaming `ChatCompletionAccumulator`
   (`JustFinishedToolCall()`). ~1 day. It is *not* the gap; do not let it dominate the estimate.

### 4.3 Scope boundary (unchanged)

`react:` is **`awf/llm`-only** — the safe glass-box carve-out where AWF owns the entire call.
CLI adapters (claude/codex/droid/goose) stay black-box; the engine-mediated loop is **forever out
of scope** for them (research-doc G4-Tier2 "don't build"). Owning the loop only "reinvents the
harness" when there is an external harness to defer to; `awf/llm` has none.

### 4.4 Effort and the honest caveat

**Weeks, multi-slice, format revision** (man-page revision first, per "the man page is the
contract"), behind a fake-backend conformance test. Build order: **A4 → A3**.

Honest caveat carried from the audit: even fully built, this would **not** let an app reuse
in-process tool functions (e.g. a TS `verify_iban` sharing the app's live DB) — `impl` runs as a
containerized `run:` step, a different execution model. P3 is the right design for
*AWF-as-substrate* (build your own specialized agent in config), **not** a retrofit for an
existing app's in-process tools.

---

## 5. Build order

**P1 (this spec) → P2 (document) → P3 (own cycle).** P1 and the P2 documentation note are
independent and can land together. P3 is gated on its own brainstorm resolving the intra-step
journaling design before any code.

---

## Appendix A — Verified non-issues (nothing to build)

| Claim | Verdict | Why nothing to build |
|---|---|---|
| "Not a job queue / durable-execution platform / agent-team framework" | **WONTFIX (deliberate identity)** | Triple-documented (`README.md:172`, `AGENTS.md:77`, `man/awf-workflow.5.md:1205`). The single-host, non-distributed boundary is the axis AWF gave up so resume is exact log-replay. The queue stays in the embedding app; AWF hosts steps under it. |
| "Therefore AWF hosts nothing; every handler stays black-box-wrapped TS" | **Category error** | The integration is `awf run` invoked *under* the existing worker. The surface already exists cleanly: `cmd/awf/main.go` → `cli.Run` → contractual exit code; `--input` is schema-validated. P1 closes the only real friction (reading the result back). |
| "Only runs in containers / requires Docker" | **False** | `awf/llm` is `Containerless:true` (`agent/awfllm/adapter.go`); a pure-LLM run auto-selects the native backend (`cli/backend_features.go:83-89`) and runs daemon-free. (Gemini needs an OpenAI-compat endpoint — minor integration cost, not architectural.) |
| "Fresh-context gate forgoes implicit cache → multiplies fan-out cost" | **False (category error)** | The gate's "fresh context" is *judge independent of generator* (`man/awf-workflow.5.md:762`), not fan-out cache-busting. The man page tells authors to share `system_prompt` across `map`/`parallel` to keep the prefix cache warm (`:398-399`); the adapter reads back `cached_tokens` (`agent/awfllm/transport.go:144`). AWF is cache-*aware*, not cache-hostile. |
| "Agent steps don't capture `output_files` as durable blobs" | **Already fixed** | `engine/local_dispatcher_agent.go:254-274` captures via `Backend.CaptureFiles` on the same `Commit` path as code steps (fixed `a2f990b`, 2026-06-08). Only *containerless* adapters can't declare `output_files` — a correct, intentional rejection. |
| "Forces Sonnet on the gate/repair/fallback" | **False** | AWF imposes no model and has no fallback-model concept; `awf/llm` takes any `model` + `base_url`. The Sonnet entanglement was in the auditing app's own code. |

---
