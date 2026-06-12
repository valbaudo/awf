# AWF correctable-gaps roadmap — design

> Status: design — approved in brainstorming, then hardened by an adversarial self-critique + a
> code/prior-art verification workflow (11 curated revisions applied; see git history). Date: 2026-06-12.
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

- **P1 — `awf outputs` read-back command + research-doc hygiene.** Read-only, ~days. **Fully
  specified here; ships first.**
- **P2 — native-backend resume.** Roadmap entry. **Recommendation: document the boundary +
  escape hatch now; build the workspace-snapshot only on demonstrated need.**
- **P3 — the tool-loop keystone** (`tools:` block [A4] + `react:` step + intra-step journaling
  [A3]). Roadmap entry. Weeks, format revision, **its own full cycle**.

The non-issues (WONTFIX / false / already-fixed) are recorded in **Appendix A** so this document
is a complete answer to the audit.

---

## 2. P1 — `awf outputs` + doc hygiene (full design)

### 2.1 Problem

An external orchestrator (e.g. a job handler that shells `awf run <wf> --input <json>` to
execute one AI step, then continues in its own language) cannot cleanly retrieve the result.
`engine.Run` returns only an `Outcome` + error (`engine/interpreter.go:82-91`); the typed data
is content-addressed at `node.completed.OutputsRef` in the blob store (`engine/commit.go:72`,
`engine/events.go:355`). The only read path today is `awf trace`, whose projected content is
bounded to 4 KB (`obs/content.go:17`) — a telemetry surface, not full-fidelity extraction. So
the caller must fold the run log and parse AWF's internal blob layout. P1 gives it a first-class
read command.

**Command name: `awf outputs` (plural).** It projects the workflow's `outputs:` contract (a named
map), matching the plural-named-export convention of GitHub Actions (`steps.<id>.outputs.*`) and
Argo (`status.outputs`). Deliberately **not** `awf output` (singular): that overloads the
`--output {text,json}` format flag already used by `ls`/`inspect`/`trace`/`graph` — an ergonomic
hazard (clig.dev "no ambiguous/similar commands"; System.CommandLine "a parent command is a
grouping noun"). Not `awf result` either: AWF returns a named map, not one terminal value (the
Temporal/Prefect shape).

### 2.2 Surface

A new read-only subcommand, added as one `case "outputs":` arm in the dispatch switch
(`cli/cli.go:120-145`), mirroring the existing `ls`/`inspect`/`trace` commands:

```
awf outputs <run-id> [--workflow <path>] [--step <node-id>] [--state-dir .awf]
```

> Surface note (refined during implementation): the workflow is a **`--workflow` flag**,
> not a second positional. Go's `flag` package stops at the first non-flag arg, so a
> `<run-id> <workflow-path>` + interspersed-flags surface can't parse `<run-id> --step …`;
> a single positional (run-id) + flags matches the `inspect`/`trace`/`ls` family
> (`parseRunIDFirst`). "the workflow-path / the export form" below means `--workflow`.

Artifact (file) read-back is **deferred** from P1 — see §2.10.

### 2.3 Semantics (two forms)

1. **No `--step` — the run's `outputs:` contract (default).** Requires `<workflow-path>`.
   Re-load + validate + digest the workflow file and **refuse on digest mismatch against the
   run's pinned `WorkflowDigest`** — identical to how `awf resume` already guards
   (`cli/resume.go:209-224`, spec §8 hard error). Then fold the run log
   (`state.FoldFile → []Event`, then `engine.Fold(events, blobs) → *RunState`) and evaluate
   `wf.Outputs` / validate `wf.OutputSchema` via the **new shared exported** `engine.EvaluateExports`
   (§2.6 — factored out of `evaluateWorkflowExports` so the sub-workflow-call path and this
   top-level path are one implementation). The shared function builds the scope itself; the
   top-level caller passes **`input=nil`**, so `input.*` resolves against the run's own input
   exactly as the engine does at top level (see §2.6). Emit the resulting map as JSON to stdout.

2. **`--step <node-id>` — one top-level node's typed output.** **No workflow file needed, and no
   full fold.** Scan the log events for the `node.completed` at `<node-id>`, `blobs.Get` its single
   `OutputsRef`, and decode — a *targeted* read, not `engine.Fold`. (Full `engine.Fold` errors if
   *any* committed blob is missing (`engine/fold.go:62-63`); reading one step's output must not
   require the whole run's blob integrity — a real concern for a partially-completed or interrupted
   run.) `<node-id>` is a **top-level node id** — a step, agent, call, `map`, or `loop` id (a
   top-level `map`'s aggregate is readable via its id). It is **not** a runtime-suffixed internal
   path: a value containing `[`, `.iter-`, `.attempt-`, or `.item-` is rejected — "P1 reads top-level
   node ids; gate/map-internal outputs are out of scope; use `awf inspect`/`awf trace`." (The engine
   itself refuses cross-attempt/cross-item external refs, and a gate commits no bare-path `Completed`
   entry, so there is no stable address to expose anyway.) A `--step` whose node has no
   `output_schema` (no typed output) is an explicit error.

**The two forms must not be mixed.** The *export-contract form* needs `<workflow-path>`; the
*log-only form* needs `--step`. Supplying `--step` together with `<workflow-path>` is a usage error.

**JSON format.** Pretty-printed, two-space indent, via `json.NewEncoder(stdout).SetIndent("", "  ")`
— matching the established AWF convention of all four existing JSON emitters
(`inspect`/`trace`/`ls`/`graph`); the encoder's trailing newline is preserved (line-oriented
consumption). (kubectl `-o json` is likewise indented-by-default; a TTY-aware compact mode à la
`gh` is a possible later refinement, not P1.)

### 2.4 Run-success ≠ output-success (the sharp edge)

Top-level `outputs:` are **inert during a normal run** — `engine.Run` never evaluates them
(`wf.Outputs` is touched only in `engine/workflow_exports.go`, called only from
`engine/call_step.go:121` for sub-workflow *calls*). `awf outputs` evaluates them for the **first
time** at read-time. Consequence: **a run can succeed yet `awf outputs` fail** — e.g. an output
binds `{{ step.X.field }}` where `X` sits in the not-taken branch of an `if` (so `X` never
committed → AWF4002 "step not yet committed"), or the bound value mismatches `output_schema`.
`validate` does not *error* on this — it checks producer-existence + schema-field, not runtime
reachability (`ir/validate_refs.go:947-979`); P1 adds a non-fatal *warning* (below).

This is the documented failure mode of Argo (issue #2167: Argo hard-failed a workflow when a
declared output referenced a `when:false`-skipped step; treated as a bug, fixed to skip
gracefully). AWF's stance + mitigations:

- **Default: hard error, never silent partial data** (correct for a strict export contract; matches
  Prefect "no result ⇒ nothing to return").
- **A validate-time *warning* (new, in P1):** when a top-level output binds a producer whose static
  path is inside a conditional/multiplicity scope (`if[` / `gate[` / `map[` / `try[`.catch), emit a
  non-fatal `AWF1048`-class warning — "output %q binds step %q inside a conditional scope; it may
  not commit, and `awf outputs` will then error." Cheap: piggybacks on `producers[id].path` and
  mirrors the existing AWF5002/5003 cross-scope path-segment checks. It does **not** attempt full
  reachability (undecidable with `if`); a warning is the right altitude.
- **`awf outputs` classifies its own failure:** "output X references step Y, which did not commit
  (skipped or never ran)" — distinct from "the run failed" — so a caller can branch on it.
- **Future:** a `--partial` opt-in (emit `null`/omit for uncommitted-source outputs — the GitHub
  Actions empty-value model) when richer conditional outputs are wanted. Never the silent default.

### 2.5 What P1 touches and deliberately does not

- **No new event, no engine write-path change.** `awf outputs` reads the same durable log + blobs
  resume folds. The `--step` form is a pure events-level read like `obs`/`awf trace`; the `outputs:`
  form additionally builds a `RunState` + scope and evaluates templates (pure reads, no side
  effects) — deeper than the `obs` span projection, but still write-free.
- **One small read-side engine refactor.** The `outputs:` form needs the export-eval logic, which is
  currently unexported and child-call-specific. P1 factors it into the shared exported
  `engine.EvaluateExports` (§2.6) — a refactor, not a behavior change.
- **No grammar / format change.** Top-level `outputs:` / `output_files` are *already* first-class IR
  fields (`ir/types.go:25-26`) and *already* validated (`ir/validate_refs.go:91`, `AWF1048`); they
  are merely inert at the top level today. P1 gives them a consumer (and the §2.4 warning, which is
  additive).
- **No persistence of the workflow source.** Like `awf resume`, the no-`--step` form takes the
  workflow file as an argument and re-hashes it against the pinned digest.

### 2.6 Reused seams + the one refactor

| P1 piece | Code |
|---|---|
| Subcommand dispatch | `cli/cli.go:120-145` switch + `cli/util.go` helpers |
| Blob store | `state.OpenBlobs(filepath.Join(stateDir, "blobs"))` (precedent: `resume.go:129`, `run.go:135`, `trace.go:67`) |
| Log → events | `state.FoldFile` (concurrent-safe; used by `cli/resume.go`, integ tests) |
| `outputs:` form: events → `*RunState` | `engine.Fold(events, blobs)` (`engine/fold.go:67`) — exported; populates `Completed`/`Branches`/`Input`/… |
| `outputs:` evaluation | the new exported `engine.EvaluateExports` (builds the top-level scope itself — see below) |
| `--step` form: targeted read | scan events for `node.completed` at the id → `blobs.Get(OutputsRef)` → decode (no full fold) |
| Digest guard | `cli/resume.go:209-224` pattern (`ld.ComputeDigest` vs `rs.WorkflowDigest`) |

**The one refactor (`outputs:` form).** `evaluateWorkflowExports` (`engine/workflow_exports.go:18`)
and `resolveNamedArtifactRef` (`engine/artifact_refs.go:10`) are unexported and child-call-specific:
the function first calls `childRunStateForCall`, which prefix-strips the parent's keys — correct for
a sub-workflow call, **wrong for a top-level run** (whose keys are already at their bare ids). A
naive "call the existing function with `callPath=""`" silently yields an empty `Completed`, and
every output ref fails AWF4002 (verified: `stripChildPrefix("summarize","workflow") = ("", false)`).
The clean factoring mirrors the engine's own top-level-vs-call scope split at
`engine/interpreter_context.go:33-43`:

```go
// new, exported — body is workflow_exports.go:22-76, scope built from the PASSED args
func EvaluateExports(rs *RunState, wf *ir.Workflow, ctxPath string, input map[string]any, blobs state.Blobs) (WorkflowExportResult, error)
```

- `call_step.go:121` keeps the call-specific lines: `child := childRunStateForCall(...)` then
  `EvaluateExports(child, wf, ir.CallWorkflowParentPath(path), callInput, blobs)`.
- `cli/outputs.go` calls `EvaluateExports(foldedRS, wf, "", nil, blobs)` — no prefix-strip,
  `ctxPath=""`, **`input=nil`** so `input.*` falls back to `foldedRS.Input` via the default path
  (`hasInputOverride=false`, `engine/scope.go:94`), matching the engine's own top-level scope rather
  than taking the override branch.

This makes `awf outputs`' result **equal to the sub-workflow export of the same run by
construction** (one implementation) — guarded by a unit test of `EvaluateExports` (§2.8).

### 2.7 Errors and exit codes

`awf outputs`' exit code reflects the **read operation**, not the run's outcome — it deliberately
succeeds on a committed output even if a later step failed (the GitHub Actions model: output-read is
decoupled from `needs.<job>.result`; a caller checks `awf run`/`awf ls` for run success). With only
three values available (`cli/cli.go:34-39`: `ExitOK=0`, `ExitInvalid`/`ExitRunFailed=1`,
`ExitUsage=2`) the code is **coarse — the stderr message disambiguates within a code**, not the code
itself. Not renumbering to sysexits (64/66): `2` aligns with Go's `flag` package and the three values
are test-locked (`validate_test.go:305-311`).

| Class | Exit | Conditions (the message carries the detail) |
|---|---|---|
| Output emitted | 0 | success |
| Bad invocation / precondition | 2 | bad flags; `--step` mixed with `<workflow-path>`; neither `<workflow-path>` nor `--step`; digest mismatch; run dir missing (not-found, via `requireRunDir`, as `inspect`/`trace`); workflow declares no `outputs:` |
| Read failed | 1 | `outputs:` references an uncommitted step; unknown `--step` id; the re-loaded workflow fails validation |

(An earlier draft split "not-producible → 1" from "usage → 2" as if the code finely classified — it
does not: `1` already covers both `ExitInvalid` and `ExitRunFailed`. The honest contract is the three
classes above, with the message carrying the rest.)

### 2.8 Testing

Unit tests over a folded fixture log (the `state.FoldFile` + fake-blobs pattern in `cli/*_test.go`):
the two forms (`outputs:`, `--step`); digest mismatch; missing-`outputs:`; unknown / rejected
`--step` (including a runtime-suffixed path → rejected); a targeted `--step` read that **succeeds
even when an *unrelated* step's blob is absent** (locks the R2 no-full-fold behavior); and an
`outputs:` ref to an uncommitted step → exit 1 with the classified message. A focused **unit test of
`engine.EvaluateExports`** over a hand-built `RunState` asserts it yields the expected map — the
top-level vs sub-workflow-call equivalence is true *by construction* (both call the one function), so
no run-both-and-compare integration test is needed. Plus an `ir` test for the §2.4
**conditional-scope warning** (and a check that no existing fixture/example trips it).

**Regression guard:** the refactor edits `engine/call_step.go` + `engine/workflow_exports.go`, so the
existing `workflow_exports_test.go` and the sub-workflow export conformance must stay green — the
call-path behavior must not change. No new conformance bucket (read-only; no new durable state/event,
so the deterministic-replay invariant is unaffected). `make lint test` is the green bar.

### 2.9 Doc hygiene (same phase, trivial)

Update `docs/research/awf-as-agent-building-substrate.md` to mark **A1** (`agents:`, SP2), **A2**
(`continues:`, SP3 — the `AgentInvocation.Thread` field + `Caps.Threaded` + per-adapter prepend,
e.g. `agent/awfllm/transport.go:84-88`; the doc's §3.2/§8 "validated-but-not-executed" caveat is
stale), and **A6/G3** (blob-as-input, SP1 — `input_files` / named `output_files` + `Backend.CopyTo`)
as shipped, with a dated banner: "Since 2026-06-06, SP1–SP5 landed; the keystone trio A1→A4→A3 is
now **A4 + A3 only**."

**Mechanics:** this file is **untracked** — `docs/` is gitignored (`.gitignore:3`) and it was never
force-added (unlike the five tracked docs under `docs/`, including this spec). It is also **absent
from the `awf-correctable-gaps` worktree**. So the edit happens in the **main working tree** as a
separate step (or the doc is force-added into tracking if we want it versioned) — it is **not** part
of the P1 worktree branch. Scope the hygiene step to this one file; do not touch the five tracked
docs.

### 2.10 Deferred from P1: artifact (file) read-back

Returning a captured `output_files:` artifact by name (the original `--file` idea) is **deferred**
from P1. `state.Blobs.Get(ref) ([]byte, error)` buffers the **whole** blob via `io.ReadAll`
(`state/blobs.go:125`), and there is **no size cap on captured files** (docker `capture.go:94`,
native `capture.go:40`; the 256 MiB cap is Snapshot-only, `backend.go:115`). A buffered, uncapped
artifact read is an OOM footgun — precisely the large artifacts (corpora, PoCs, payload dumps) this
would serve. Every mature engine **streams** artifacts (Dagger `export --path`, Argo artifact
download, Nextflow `publishDir`, Gitaly `catfile` returning an `io.Reader`). So artifact read-back is
a follow-up that adds a streaming seam — `state.Blobs.Open(ref) (io.ReadCloser, error)` + `io.Copy`
to the sink, plus an optional `awf outputs <run-id> --file <name> --path <dest>` to write to disk
(the Dagger/Nextflow pattern).

**Honest interim gap.** With `--file` deferred, **P1 has no clean way to extract a produced file** —
only typed JSON (`outputs:` / `--step`). The sole workaround is the raw blob layout or `awf trace`'s
4 KB-bounded preview. For *artifact-centric* workflows (an offensive-security pipeline whose
deliverable *is* the captured payload / PoC / corpus), that is a real limitation: P1 ships
**typed-output-complete but artifact-incomplete**. If artifact retrieval is load-bearing for the
first real consumer, promote the streaming `Blobs.Open` slice **ahead of P2** rather than after.
(Chosen over building it into P1 now to avoid silently expanding the `state.Blobs` interface —
CLAUDE.md "don't assume architecture"; flip to "build streamed `--file` in P1" on request.)

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
   (`JustFinishedToolCall()`). Roughly a day's work (an estimate, not a measured figure) — and it
   is *not* the gap; do not let it dominate the effort estimate.

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
journaling design before any code. The implementation plan that follows this spec covers **P1
only**; P2 and P3 get their own plans when picked up.

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
