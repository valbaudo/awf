# AWF P3 — the tool-loop keystone (`tools:` + `react:`) — design

> Status: design — approved in brainstorming (2026-06-13), grounded in a code/prior-art
> verification workflow against `origin/main @ 0b81246` (file:line anchors throughout), then
> **hardened by a 5-lens adversarial self-critique workflow** (curated findings applied; the review
> verified ~30 anchors and corrected the six listed in §11). This is the **own full cycle** the
> correctable-gaps roadmap (`2026-06-12-awf-correctable-gaps-design.md` §4) scoped for P3.
> Author: maintainer-side, continuing the primacasa.ai-audit remediation.
> Scope: the design for `tools:` (A4) + `react:` (A3) on the `awf/llm` path. The man-page format
> revision lands first (per "the man page is the contract"); implementation follows in its own
> plan. Build order: **A4 → A3**.

---

## 0. Origin and the one corrected premise

The correctable-gaps roadmap named P3 "the tool-loop keystone" and flagged **intra-step
journaling** as "the most invariant-sensitive change in the whole roadmap" and "the gating
unknown P3's own brainstorm must resolve." This spec resolves it — and corrects the premise that
made it look hard.

**Corrected premise:** the durable log is *not* merely node-path-granular. Three control kinds
already journal **resume-safe per-round sub-events** keyed at the composite's node path with an
ordinal `N` in the payload, which `Fold` accumulates into per-path slices:

- `gate.attempt` → `RunState.GateAttempts[gatePath]` (`engine/fold.go:245-268`)
- `map.item` / `map.frontier` → `RunState.MapItems[path]` (`engine/fold.go:270-304`)
- `loop.iter` → `RunState.LoopIters[path]` (`engine/fold.go:237-243`)

`react.round` is the **fourth instance of this existing pattern**, not a new journaling
primitive. Two existing handlers are its templates: **`engine/gate.go`** for the per-round marker
(`startN := len(GateAttempts[gatePath]) + 1`, `gate.go:93`; marker appended+`Sync`ed after each
*completed* unit, `gate.go:161-170`), and **`engine/reduce.go`** for the per-round synthetic
**model leaf** — a committed `node.completed` with no backing IR node, explicitly guarded on
resume by `if _, ok := rs.LookupCompleted(nodePath); ok { … }` (`reduce.go:150`) and committed via
`Commit(log, blobs, nodePath, DispatchResult{Outcome: OutcomeOK, Outputs: out}, false)`
(`reduce.go:193`). This de-risks P3 from "weeks of scary invariant work" to "additive — compose
two already-solved mechanisms, plus a modest amount of new arg-staging/scope plumbing (§3.3)."

---

## 1. What we are building

The capability to *build* a native augmented-LLM agent (model + tools + loop) on the `awf/llm`
path, rather than only *wrapping* a pre-built CLI agent. With **A1 (`agents:`) and A2
(`continues:`) already shipped (SP2/SP3)**, the remaining trio is **A4 + A3**:

- **A4 — `tools:` block.** A top-level map; each tool = `{description, input_schema, impl}` where
  `impl` is a parameterized `run:`/`exec` step. Digest-pinned. **Reuses the existing code-step
  *execution* substrate** (the `Backend.Exec` + `Commit` path, via a synthesized `CodeStep` exactly
  as `engine/reduce.go` does for the reducer) — **no parallel tool runtime**. (The *format* type
  for `impl` is a new id-less struct, and the arg-staging + scope binding are new plumbing — §3.3.
  "No new executor", not "no new code".)
- **A3 — `react:` step.** A *new* node kind (distinct from the atomic `agent:` step, so
  "one invocation = one commit" is untouched everywhere else). The engine attaches the selected
  tools to the `awf/llm` request → parses `tool_calls` → dispatches each as its `impl` step →
  appends `ToolMessage` results → loops to `max_turns`.

### 1.1 Locked decisions (from brainstorming)

1. **Journaling = Fork B (two-level, gate-style).** Each round's model call commits its own
   synthetic leaf `node.completed` (reduce-style, with an explicit resume guard); each tool impl
   commits its own leaf `node.completed`; a thin `react.round` marker (raw `Log.Append`+`Sync`)
   closes the round. The `react:` node commits one terminal `node.completed`. (§4)
2. **Tool args reach the impl as a file + scalar convenience.** The full `arguments` JSON is
   stored **verbatim** (§4.5 invariant), staged into the impl's container via the `Backend.CopyTo`
   seam, and exposed as `{{ args_file }}`; top-level *scalar* fields are additionally bound as
   `{{ args.<field> }}` (best-effort parse, absent if unparseable). (§3.3)
3. **`max_turns` exhaustion → `OutcomeOK` + `stop_reason: "max_turns"`**, with the dangling final
   tools **not** dispatched, so the gate (AWF's verification mechanism) judges the truncated
   answer. (§5)
4. **Scope.** `react:` is `awf/llm`-only (`Caps.Containerless && Caps.Threaded`); v1 is the
   OpenAI-compat path only (the Ollama-native path is rejected); `tools:` is **required** on a
   `react:` step (≥1 name); `tool_choice` is **auto**, not configurable in v1; `max_turns`
   default **8**. (§2, §6)

---

## 2. Scope & boundary

- **Adapter gating.** `react:` runs only against an adapter with `Caps.Containerless` *and*
  `Caps.Threaded` — today exactly `awf/llm` (`agent/awfllm/adapter.go:86-88`). Validate rejects a
  `react:` step whose `with.uses` resolves to any other adapter at load-time (a new diagnostic),
  and the dispatch path keeps a defensive guard (mirroring the threaded-request guard at
  `engine/local_dispatcher_agent.go:76-81`). CLI adapters (claude/codex/droid/goose) stay
  black-box — the engine-mediated loop is **forever out of scope** for them (research-doc
  G4-Tier2 "don't build": owning the loop only reinvents the harness when there is an external
  harness to defer to; `awf/llm` has none).
- **v1 = OpenAI-compat only.** The `awf/llm` adapter selects its wire path on the resolved
  `structured_output` strategy, **not** on `base_url` (`transport.go:57`:
  `if cfg.StructuredOutput == soOllamaFormat { … streamOllama }`). v1 attaches/parses tools only in
  the OpenAI-compat build site (`streamOpenAI`); a `react:` step whose config resolves to
  `structured_output: ollama_format` (the Ollama-native path, which lacks tool wiring) is rejected
  with a clear error, not a silent tool drop. (Ollama-native tool calling is a flagged follow-up.)
- **Mixed containerization is intended.** The model call is containerless (`awf/llm`); each tool
  **impl is an ordinary containerful `run:`/`exec` step** with its own `image:`. This is the whole
  point: the glass-box LLM call + black-box tool execution on the proven code-step substrate.

---

## 3. Format surface (A4) — the man-page revision

The man-page revision (`man/awf-workflow.5.md`) lands **first**, as the contract. Two additions.

### 3.1 The top-level `tools:` block

A new top-level workflow field (a sibling of `graph:` / `outputs:` — `ir/types.go:25-27`; note the
top-level node list is `graph:`, there is no `steps:` key): a map from tool name to a tool
definition.

```yaml
tools:
  check_iban:
    description: Validate an IBAN and return its issuing bank.
    input_schema:
      type: object
      properties:
        iban: { type: string }
      required: [iban]
    impl:
      run: ./validate --args-file {{ args_file }}   # or: --iban {{ args.iban }}
      image: tools/fin:1.4@sha256:abc…
      timeout: 30s
```

- **`description`** (string, required) — sent to the model as the tool's description.
- **`input_schema`** (JSON Schema, required) — sent to the model as the function parameters
  (subject to the §7 JSON-Schema floor, same as `output_schema`). Reused verbatim as the OpenAI
  `shared.FunctionDefinitionParam.Parameters` (`shared.FunctionParameters` = `map[string]any`,
  exactly the existing `ResponseFormat` cast at `transport.go:110`).
- **`impl`** — the executable tool, a `run:`/`exec` step body carrying `image`/`container`,
  `timeout`, `retry`, `output_files`, `input_files`. **Format note:** `impl` is a dedicated id-less
  type (e.g. `ir.ToolImpl`), **not** a reused `ir.CodeStep` (whose `ID` field is `json:"id"`
  *without* `omitempty`, `ir/node.go:21-22`, so it would serialize an empty `"id":""` into the JCS
  digest). At **execution** time the engine synthesizes a `CodeStep` from the `ToolImpl` and
  dispatches it through the existing substrate exactly as the reducer does (`reduce.go:216-294`:
  synthesize `&ir.CodeStep{Run, Container, OutputSchema, OutputFiles}` → `Backend.CopyTo` inputs →
  `Backend.Exec` → `Commit`). No new executor, container path, retry, or output-capture code.
- The whole `tools:` map folds into the workflow digest automatically (whole-workflow JCS,
  `ir/digest.go:22-77`); every tool `impl`'s `image:` is digest-pinned exactly like any
  containerful step.

### 3.2 The `react:` step

A new node kind, registered as a **control-style wrapper node of the Map class** — a single-key
wrapper (`react:`) whose value is a config object carrying a **required `id`** (Map is the
precedent: a wrapper node that carries an `id`, `ir/node.go:123-124`). It is registered via the
four synchronized edits documented at `ir/node.go:3-16` (type+`isNode()`; `node_marshal.go`;
`controlKeys` in `node_unmarshal.go`; the `unmarshalControl` case) **and** the `wantKinds`
constant bump 12→13 in `ir/node_test.go:471` (`TestNodeRegistryExhaustive`).

```yaml
- react:
    id: answer_question
    with:                      # the awf/llm config minus prompt (see below)
      uses: awf/llm
      model: gpt-4o
      base_url: https://api.openai.com/v1
      system_prompt: "You answer finance questions. Use tools to verify every figure."
    prompt: "{{ input.question }}"      # the initial user turn (step-level, not a with: key)
    tools: [check_iban, lookup_rate]    # subset of top-level tools:, required, ≥1
    max_turns: 8                        # default 8
    output_schema:                      # optional typed final answer (natural-stop only — §5)
      type: object
      properties:
        answer: { type: string }
      required: [answer]
```

- **`with`** — the same `awf/llm` config an `agent:` step uses (`model`, `base_url`,
  `system_prompt`, …) **minus `prompt`**. The `awf/llm` adapter today *requires* `prompt` as a
  with-key (`agent/awfllm/validate.go:64`); for `react:` the engine owns the messages array and
  supplies the initial user turn from the step-level `prompt`, so `react:` validates `with:` with a
  **prompt-exempt** variant of `ValidateConfig`. The `rejectedKeys` guard (incl. `tools`,
  `validate.go:35`) stays intact — A4/A3 route tools through dedicated fields, **not** through the
  opaque `with:` map (keeping `with:` a closed schema rather than overloading it).
- **`prompt`** — the initial user message (templated, scalars only per existing constraints).
- **`tools`** — a list of names selecting which top-level tools this step exposes (required, ≥1;
  each must exist in the top-level `tools:` map — a new validate cross-ref). A `react:` step with
  no tools is just an `agent:` step; the format refuses it.
- **`max_turns`** — integer ≥ 1, default 8. One "turn" = one model call (+ any tools it requests).
- **`output_schema`** — optional; see §5 for its interaction with `stop_reason` (it must not
  itself declare a reserved `stop_reason` property — a validate check).

**Addressing (Map-class semantics).** Following the Map precedent exactly: the node's **runtime
path is `react[N]`** (keyword[index], like `gate[N]`/`map[N]` — control nodes do *not* use their
`id` as the path segment; `ir/walk.go` computes `map[i]` ignoring `Map.ID`). `RunState.ReactRounds`
is therefore keyed by `react[N]`. The node's **output is referenceable by `id`** —
`{{ <id>.* }}` and `awf outputs --step <id>` — via a **producer registration** that maps the `id`
to the `react[N]` path and carries its `output_schema`, exactly as Map registers
`producers[v.ID] = producer{path, kind, schema}` (`ir/validate_refs.go:162-177`). That producer
edit is part of §6. (Throughout §4, `R` denotes the runtime path `react[N]`.)

### 3.3 Tool-argument binding (the file + scalar decision)

When the model emits a `tool_call` for tool `T` with `arguments` (a JSON string), the engine:

1. **Stages** the **verbatim** `arguments` bytes (the §4.5 invariant — never parsed-then-
   reserialized) into `T.impl`'s container via the **`Backend.CopyTo` seam directly** — a
   synthesized `container.InputFile{Path, Content}` (`reduce.go:263-266` is the precedent for
   staging an engine-generated blob this way; `container/backend.go:119` is the seam). It is **not**
   routed through the declarative `input_files` resolver, which only accepts static named refs
   (`step.<id>.files.<name>`, `input.files.<name>`, `asset.<id>` — `engine/input_files.go:88`,
   `AWF3007`) and cannot resolve a fresh runtime blob. The container path is exposed to the impl's
   templates as `{{ args_file }}`. Structured (object/array) arguments are read by the impl from
   this file — **never** interpolated into a command line, so AWF4004 (arrays in `{{ }}`,
   `template/eval.go:32`) and shell-injection are both sidestepped.
2. **Binds** top-level *scalar* fields of `arguments` into the impl's template scope as
   `{{ args.<field> }}` via a **new scope arm** — modeled on the override-constructor pattern
   `NewScopeWithVerdict` (`engine/scope.go:66`), **not** reusing the map-specific
   `resolveAsBinding` (`scope.go:296-368`, hard-wired to walk `map[N].item-K` paths). The binding
   is a **best-effort** parse of the verbatim bytes: non-scalar fields and an unparseable
   `arguments` simply leave `args.*` empty (use `args_file`). A scalar arg containing shell
   metacharacters is safe only when the author uses `exec:`/quoting — documented; the file path is
   the recommended pattern.

Both are **deterministic on resume** *because* of the §4.5 verbatim invariant: the re-staged
`args_file` reproduces the exact bytes the model emitted (read back from the committed model leaf),
never a re-serialization.

---

## 4. Journaling & resume (Fork B) — the load-bearing design

### 4.1 The per-round structure

For a `react:` step with runtime path `R` (= `react[N]`), round `K` (1-based) journals:

| Sub-path | Event | Writer | Carries |
|---|---|---|---|
| `R.round-K.model` | `node.completed` (`OutcomeOK`) | `engine.Commit` (synthetic leaf, reduce-style) | the assistant turn's text + `finish_reason` + the **ordered** `tool_calls` list, each `{index, id, name, arguments-as-raw-string}` (§4.5) + token/cost usage |
| `R.round-K.tool-J` | `node.completed` (`OutcomeOK`) | `engine.Commit` (synthesized code step) | tool `J`'s impl result (the `ToolMessage` content) |
| `R.round-K` | **`react.round`** marker | raw `Log.Append` + `Log.Sync` | `{N: K}` (a pure round cursor) |
| `R` (once, at loop end) | `node.completed` (`OutcomeOK`) | `engine.Commit` | the final answer + `stop_reason` (§5) |

- **`J` = the tool_call's stable `Index`** (openai-go exposes `Index int` on each
  `FinishedChatCompletionToolCall`), preserved as the position in the ordered `tool_calls` list on
  the `.model` leaf. So two calls to the *same* tool in one round get distinct, resume-stable
  `tool-0`/`tool-1` paths and distinct `args_file`s.
- **The model call is a committed synthetic leaf** — this is the hazard-killer. On resume the
  round's `tool_calls` (and their verbatim `arguments`) are read back from `R.round-K.model`, so
  the **non-deterministic model is never re-sampled** for a round whose tools already ran. This is
  the `runIf` "record the decision, don't recompute the condition" *intent* (`interpreter.go:347-
  355`) — but with **gate-style `Sync`'d durability** (`gate.go:161-170`), not `branch.taken`'s
  deliberately-unSync'd append (`interpreter.go:387-389`), because re-sampling the model is **not**
  first-run-equivalent (unlike re-evaluating an `if` condition).
- **Each tool impl is a committed leaf** at `R.round-K.tool-J`, a synthesized code-step commit. It
  resume-skips via the existing `LookupCompleted` short-circuit inside `runCodeStepWithContext`
  (`engine/interpreter.go:246`; the handler spans `:245-341`, `Commit` at `:336`).
- **The `react.round` marker is thin** and gate-class durable: `Log.Append`+`Log.Sync`
  (`gate.go:161-170`), **not** loop's fsync-riding append (`interpreter.go:465` deliberately skips
  `Sync`). It carries only `{N}` — a pure round cursor meaning "round K fully settled (model +
  every dispatched tool committed)". `finish_reason` lives on the **`.model` leaf** (its authoritative
  home — it is produced there and is needed at the *frontier* round to make the stop decision
  *before* any marker exists; replayed rounds `1..startK-1` were all `tool_calls` by construction).
  So nothing is duplicated and there is no drift path. Everything load-bearing is in the committed
  `.model`/`.tool-J` leaves, read back by sub-path on resume.

### 4.2 Why this preserves *one-invocation = one-commit*

That property — "exactly one `node.completed` **per node path**" — is a **writer-side discipline**,
not a fold-enforced check: `Commit` refuses non-`OutcomeOK` (`commit.go:48-50`) and `Fold` rejects
a non-ok `node.completed` (`fold.go:181-184`), but `Fold`'s `EventNodeCompleted` arm is plain
last-write-wins (`rs.Completed[e.Path] = nr`, `fold.go:224`, **no** duplicate-path error — unlike
`run.started`/`call.started`/`skills.selected`). So the invariant rests entirely on each handler's
`LookupCompleted` short-circuit. It holds for `react:` because every leaf (`.model`, each
`.tool-J`) and the terminal `R` commit at a *distinct* path, and `react.round` is a distinct
*event type* (like `gate.attempt`), not a second `node.completed` at the same path.

**Consequence (ties to §4.3):** because the property is writer-discipline and the synthetic
`.model` leaf does **not** inherit the `interpreter.go:246` short-circuit (that lives *inside*
`runCodeStepWithContext`; control-style handlers like `runGate`/`runMap` implement their *own*
cursor), `runReact` must (a) short-circuit on `LookupCompleted(R)` at entry (terminal skip), and
(b) explicitly guard the `.model` leaf per round (§4.3). A missing guard would *silently* overwrite
rather than error — hence the explicit guards.

### 4.3 The engine additions (all additive)

1. **`engine/path.go`** — add `roundSep = ".round-"` (alongside `iterSep`/`attemptSep`/`itemSep`,
   `path.go:23-27`) and helpers `RoundPath(R, k)` → `react[N].round-K`, `ModelPath(roundPath)` →
   `…model`, `ToolPath(roundPath, j)` → `…tool-J`. The separators are the single source of truth;
   nothing hand-formats paths.
2. **`engine/events.go`** — add `EventReactRound` (a *commit-class* marker, sibling of
   `EventGateAttempt`) + `ReactRoundData{N int}` (a pure round cursor; `finish_reason` lives on the
   `.model` leaf, §4.1). Add a `ReactRoundData`
   round-trip test in `engine/events_test.go` (alongside `TestGateAttemptDataRoundTrip` /
   `TestLoopIterDataRoundTrip`) — there is **no** engine-package tag-reflection test to inherit
   (`tags_test.go` is `package ir` only).
3. **`engine/fold.go`** — one new arm: `EventReactRound` → *append* to
   `RunState.ReactRounds[R] []ReactRoundRecord` (mirror `fold.go:245-268`). Append-order =
   arrival-order = replay-order. The `Fold` default arm already ignores unknown types
   (`fold.go:359-370`), so the new event type is **additive and fold-default-arm-safe**: existing
   (non-`react`) logs remain byte-identical and replayable.
4. **`engine/runstate.go`** — `ReactRounds map[string][]ReactRoundRecord` + `LookupReactRounds` /
   `RecordReactRound` accessors (mirror `LoopIters`/`GateAttempts`, `runstate.go:361-431`).
5. **`engine/react.go`** (new) — `runReact`, composing `runGate` (`gate.go:58-193`) and `runReduce`
   (`reduce.go:150-294`):
   - **Terminal short-circuit:** `if _, done := rs.LookupCompleted(R); done { return OutcomeOK }`.
   - `startK := len(rs.LookupReactRounds(R)) + 1` — the resume cursor.
   - **Replay** rounds `1..startK-1`: rebuild the OpenAI `messages` array purely from the committed
     `.model` leaves (assistant turns + verbatim `tool_calls`) and `.tool-J` leaves (tool results)
     via `LookupCompleted` — no model call, no tool dispatch. Read each round's `finish_reason`
     from `ReactRounds[R]`.
   - **Execute** from round `startK` (a frontier round may be *partially* committed):
     1. **Model leaf, guarded:** `if mr, ok := rs.LookupCompleted(ModelPath); ok` → read the
        assistant turn + `tool_calls` + `finish_reason` back from `mr` (do **not** re-sample the
        model); `else` → dispatch the awf/llm model call with tools attached (§5), then `Commit` the
        synthetic `.model` leaf (reduce-style, `reduce.go:150/193`). This explicit guard is what
        neutralizes the model-committed-but-tool-crashed hazard.
     2. **Terminate? (decide *before* dispatching any tool).** If `finish_reason != "tool_calls"` →
        **natural stop** (no tools to run; go to the terminal commit, §5). If
        `finish_reason == "tool_calls"` **and** `K == max_turns` → **truncated stop**: do **not**
        dispatch the dangling tool_calls; go to the terminal commit (§5). Otherwise (tools
        requested, budget remains) → step 3.
     3. **Dispatch tools** (in `Index` order): for each `tool_call`, `if rs.LookupCompleted(ToolPath)`
        → skip; `else` → bind args (§3.3) + dispatch the impl as a synthesized code step + `Commit`
        the `.tool-J` leaf. Tool-failure handling per §4.5.
     4. **Close the round:** `Log.Append`+`Sync` the `react.round` marker — **only after** the model
        leaf and every dispatched tool leaf have committed (crash≠verdict ordering, §4.4) — then
        loop to round `K+1`.
   - On stop (step 2), `Commit` the terminal `node.completed` at `R` (§5). The handler is
     resume-idempotent: the terminal-`R` short-circuit (entry) plus the per-round model-leaf guard
     make a re-entered partial round re-derive the same decision without re-sampling the model or
     re-running a committed tool.

### 4.4 crash ≠ verdict

The `react.round` marker is appended **only after** the round's model leaf *and every dispatched*
tool leaf have committed (the `gate.go:110-113` ordering: a mechanical failure propagates *before*
the marker). So a half-finished round is never folded complete: resume re-enters at it, its
already-committed leaves short-circuit (model via the §4.3 explicit guard; tools via their own
`LookupCompleted`), and only genuinely-uncommitted work re-runs. The model leaf is `Sync`'d before
any tool side-effect (it goes through `Commit`), so a committed tool can always read its triggering
`tool_calls` back. Re-billing and duplicate side-effects are bounded to genuinely-uncommitted
leaves; tool impls inherit the existing `idempotency_key` mechanism (`interpreter.go:255`) for
authors who need exactly-once external effects.

### 4.5 Tool-failure, malformed-call, and verbatim-args handling

These are the *common* paths of a tool loop, not edge cases. v1 behavior:

- **Verbatim-arguments invariant (load-bearing for resume determinism).** The `.model` leaf stores
  each `tool_call` as `{index, id, name, arguments}` where `arguments` is the **raw string the
  model emitted**, stored verbatim. The engine **never** unmarshals-then-reserializes it before
  staging. (`node.completed` persists `dr.Outputs` via `json.Marshal`→`Blobs.Put`→`Fold`
  `json.Unmarshal` into `map[string]any`, `commit.go:63-72`/`fold.go:191-203`; a Go *string* value
  round-trips byte-identically, a nested `map[string]any` does **not**.) `args_file` receives these
  exact bytes; `{{ args.<field> }}` is a *discarded* best-effort parse for ergonomics.
- **Tool impl non-zero exit** (the tool ran and failed, distinct from an infra/dispatch failure):
  the exit code + stdout + stderr are captured and **fed back to the model as the `ToolMessage`
  content**; the `.tool-J` leaf commits `OutcomeOK` with this result; the react step does **not**
  fail. This is the conventional augmented-LLM behavior and what makes a gate-judged loop useful
  (the model can recover; the gate judges the final answer; `max_turns` bounds runaway). *(This is
  a deliberate divergence from a normal code step, whose non-zero exit is a step failure
  `interpreter.go:328-334` — `runReact` wraps the impl dispatch to convert a tool's own non-zero
  exit into committed result data.)* A genuine **infra/dispatch failure** (backend can't exec the
  container), after the impl's own `retry:` policy, remains a hard react-step failure.
- **Unknown / hallucinated tool name** (a `tool_call` naming a tool not in `react.tools` — the
  model can emit this; `Strict` on `FunctionDefinitionParam` constrains argument *shape*, not tool
  *selection*): no impl is dispatched; an error `ToolMessage` ("unknown tool `<name>`") is fed back
  so the model can correct; the round proceeds (bounded by `max_turns`).
- **Malformed `arguments` JSON:** the verbatim bytes are still staged to `args_file` (the impl
  decides what to do with them); `{{ args.<field> }}` scalars are simply absent. v1 does **not**
  validate `arguments` against `input_schema` engine-side.

---

## 5. `max_turns` exhaustion and the terminal output contract

The react step commits `OutcomeOK` in **all** stop cases; its typed output always carries a
reserved `stop_reason` field. Two cases:

- **Natural stop** (`finish_reason != "tool_calls"`): `stop_reason: "stop"`. If `output_schema` is
  declared, the final assistant turn must conform — validated by the existing typed-output
  machinery (`local_dispatcher_agent.go:236-252`); non-conforming → the existing schema-failure
  path (retry/fail). The output is the validated object plus `stop_reason`.
- **`max_turns` stop** while the model still wants tools: the engine stops **immediately after
  committing the final `.model` leaf** and does **not** dispatch that round's dangling `tool_calls`
  (their results could never be consumed — avoids wasted side-effects/billing). The terminal output
  is `{ stop_reason: "max_turns", text: <last assistant text, possibly ""> }`, and `output_schema`
  is **not** enforced (the answer is explicitly truncated). Authors gate on `stop_reason` first.

**Validate check:** an `output_schema` on a `react:` step may not declare a property named
`stop_reason` (reserved; avoids a collision with the engine-injected field). The exact placement of
`stop_reason` relative to the typed object (sibling vs reserved top-level key) is pinned in the
implementation plan; the contract above (always-OK, always-has-stop_reason, schema-enforced-on-
natural-stop-only) is fixed here.

The `Outcome` enum gains **no** `loop_exhausted` value — staying `OutcomeOK` keeps the terminal
`node.completed` valid under the only-ok-commits fold rule (`fold.go:181-184`).

---

## 6. The `awf/llm` tool integration (the "easy part" — ~a day, verified)

`openai-go v3.39.0` (pinned, `go.mod`) has the complete tool API; the review verified every symbol.
In `transport.go:streamOpenAI` (the OpenAI-compat build site; the Ollama path, `streamOllama`, is
rejected for v1 per §2):

1. **Attach tools:** `params.Tools = []openai.ChatCompletionToolUnionParam{
   openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{Name, Description, Strict,
   Parameters: shared.FunctionParameters(inputSchemaMap)}) }` — one entry per selected tool;
   `Parameters` is the tool's `input_schema` as `map[string]any` (same cast as the existing
   ResponseFormat path, `transport.go:110`). Adds the `…/v3/shared` import.
2. **Tool choice:** leave `params.ToolChoice` **unset** — `auto` is the documented SDK default when
   `Tools` is non-empty, matching the v1 auto-only decision. (If ever set explicitly, the field is
   `ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("auto")}` — `OfAuto` is a
   `param.Opt[string]` *field*, not a constructor; needs the `…/packages/param` import.)
3. **Parse streamed `tool_calls`:** drive an `openai.ChatCompletionAccumulator`
   (`acc.AddChunk(chunk)`; `acc.JustFinishedToolCall()` → `FinishedChatCompletionToolCall{Index, ID,
   embedded …Function{Name, Arguments string}}`) — the documented path, replacing the content-only
   delta loop (`transport.go:128-149` reads only `Delta.Content` today). Keep the existing
   `len(chunk.Choices) > 0` guard (`transport.go:134`) on the accumulator path; a trailing
   usage-only chunk (with `IncludeUsage`) has empty `Choices`.
4. **Build the next turn:** append the assistant turn via `acc.Choices[0].Message.ToParam()`
   (rebuilds the assistant param incl. `tool_calls`) then one `openai.ToolMessage(result, id)` per
   result; re-issue `NewStreaming` with the grown messages.

**Message history shape.** The engine handler owns the loop and the messages array. Today the
awf/llm message history is `ThreadTurn{User, Assistant string}` (`agent/types.go:79-86`), which
cannot represent an assistant turn carrying `tool_calls` nor a tool-role message. P3 adds a
**tool-aware turn shape** (a `ReactTurn`/message-union carried on a react-specific invocation
surface), supplied by the engine to the adapter, gated by `Caps`. This is the one non-trivial
integration point beyond the `transport.go` request shape; it stays inside the `awf/llm` + engine
boundary (no change to `agent:`/`continues:`). `classifyOpenAIErr` (`transport.go:160`) already maps
transport faults.

### 6.1 Validation & gating (`ir/validate*.go`)

- Every `react.tools[*]` name resolves to a top-level `tools:` entry; `react.tools` is non-empty.
- `react.with.uses` resolves to a `Containerless+Threaded` adapter (run-start gating mirrors
  `local_dispatcher_agent.go:76-81`); a config resolving to `structured_output: ollama_format` is
  rejected (no tool wiring on the Ollama path).
- `max_turns ≥ 1`; each tool `input_schema` and the optional `output_schema` pass the §7 floor;
  `output_schema` declares no reserved `stop_reason` property (§5).
- **Producer registration** for `{{ <id>.* }}` / `awf outputs --step <id>` (the §3.2 addressability
  claim): an `indexProducers`/`validate_refs` arm for `react` keyed by its `id` → `react[N]` path,
  carrying its `output_schema` (mirroring the Map producer at `ir/validate_refs.go:162-177`). The
  same multiplicity constraint Map's product addressing carries (addressability of a `react:` nested
  inside `loop`/`gate`/`map`) applies; if that proves fiddly, `{{ <id>.* }}` for a nested `react:`
  is scoped out of v1 (the `--step` read still works).

---

## 7. Testing

- **Fake-backend conformance bucket** (new): a `react:` workflow driven by a fake awf/llm adapter
  emitting scripted `tool_calls`, asserting the full round structure and the terminal answer.
  Deterministic — no network. (No change to the deterministic-replay invariant: the new event type
  is additive and fold-default-arm-safe.)
- **Resume conformance:** kill after `R.round-2`'s marker → resume re-enters at round 3, and the
  model/tools of rounds 1–2 are **not** re-dispatched (the fake adapter/backend counts calls), with
  the messages array rebuilt identically from the committed leaves. **Plus** the torn-frontier case
  (kill after `R.round-K.model` commits but before `tool-0`) → resume reads the same `tool_calls`
  back (model **not** re-sampled) and runs only the uncommitted tools. **Plus** two-calls-to-the-
  same-tool in one round → distinct `tool-0`/`tool-1` by `Index`.
- **Unit tests:** `EventReactRound` fold arm + `Lookup/RecordReactRounds`; the `startK` cursor + the
  `.model`-leaf guard; the verbatim-args invariant (a nested-object `arguments` round-trips byte-
  identically through commit→fold and re-stages identical `args_file` bytes); arg-binding
  (`{{ args.<field> }}` scalar; object via `{{ args_file }}` — no AWF4004, no injection);
  `RoundPath`/separator round-trip; `max_turns` → OK+`stop_reason` with dangling tools **not**
  dispatched; tool non-zero exit fed back as `ToolMessage`; unknown-tool-name `ToolMessage`;
  adapter-gating + Ollama-path rejection; the validate cross-refs + the producer registration.
- **Regression:** existing `gate`/`loop`/`map`/`reduce`/`continues:` and `workflow_exports` suites
  stay green (this change adds, never edits, their fold arms). `make lint test` is the bar.

---

## 8. P2 doc note (folded into this cycle)

Independent of P3, land the correctable-gaps P2 deliverable (recommendation *b*): a short
man-page (`man/awf.1.md`) + README note documenting that `--backend native` runs are
non-resumable (matching the error at `cli/backend.go:108`), with the two escape hatches —
`--backend docker` for resumable runs, or re-drive a deterministic run. No code change; pure
documentation. It can land with or before the P3 work.

---

## 9. What this deliberately does NOT do (YAGNI + honest caveat)

- No `tool_choice` configuration, no forced/parallel tool-choice modes (v1 = auto).
- No parallel tool **runtime** — tool impls dispatch sequentially within a round on the existing
  code-step substrate (a future `concurrency:` for within-round tool fan-out is a follow-up).
- No Ollama-native tool calling (v1 = OpenAI-compat).
- No engine-side validation of model `arguments` against `input_schema` (v1).
- No engine-mediated loop for CLI adapters (forever out of scope).
- **Honest caveat carried from the audit:** even fully built, `react:` does **not** let an app
  reuse in-process tool functions (e.g. a TS `verify_iban` sharing the app's live DB) — `impl`
  runs as a containerized step, a different execution model. P3 is the right design for
  *AWF-as-substrate* (build your own specialized agent in config), **not** a retrofit for an
  existing app's in-process tools.

---

## 10. Build order

1. **Man-page format revision** (`man/awf-workflow.5.md`): `tools:` block + `react:` step — the
   contract, first.
2. **A4 — `tools:` block:** IR (`ir/node.go` 4-edit registry + `wantKinds` bump + an `ir.Tool` +
   id-less `ir.ToolImpl` type + top-level `Workflow.Tools`), validate, digest (automatic), and the
   arg-staging (`Backend.CopyTo`) + `args.*`/`args_file` scope plumbing (§3.3). Tool impls are
   dispatchable via a synthesized `CodeStep` reusing the existing substrate (the `reduce.go`
   precedent).
3. **A3 — `react:` step:** the new node kind + producer registration + `engine/react.go`
   (`runReact`), the `transport.go` tool request/parse + the tool-aware message shape, journaling
   (§4), failure handling (§4.5), and the terminal contract (§5).
4. **P2 doc note** (§8) — independent; land anytime in this cycle.

Each lands behind the fake-backend conformance bucket; `make lint test` green throughout.
The implementation **plan** (writing-plans) sequences these into ordered TDD steps and pins the
remaining implementation-level choices (the exact `ReactTurn` message-union type; the precise
placement of the `stop_reason` field in the terminal output).

---

## 11. Adversarial-review corrections applied (audit trail)

The 5-lens review verified ~30 file:line anchors and flagged six substantive items, all folded in:

1. **`.model`-leaf resume guard** (was a §4.3/§4.4 contradiction) → explicit `LookupCompleted`
   guard on the per-round model leaf, `reduce.go:150` precedent (§4.3 execute step 1).
2. **Node-path/addressing** (control nodes are `keyword[N]`, not id-keyed) → `react:` is a
   Map-class wrapper; path `react[N]`; `{{ <id>.* }}` via a producer registration (§3.2, §6.1).
3. **Tool-loop failure modes** (non-zero exit / malformed args / hallucinated tool) → new §4.5.
4. **`max_turns`-on-tool_calls** (undefined terminal payload + wasteful dispatch) → §5 (no dangling
   dispatch; defined truncation envelope; schema relaxed).
5. **Verbatim-args determinism invariant** → §4.5 (raw-string storage, never re-serialized).
6. **Overstated reuse** → §1/§3.1/§3.3 distinguish "verbatim *execution* substrate" (true) from
   new format type + `Backend.CopyTo` staging + new scope arm (the real, modest new code).

Anchor/precision fixes also applied: `graph:` not `steps:` (§3.1); Ollama gated on
`structured_output`, not `base_url` (§2/§6); `streamOpenAI` is one of two build sites, not "the
only" (§6); the one-completion-per-path invariant is writer-discipline, not fold-enforced (§4.2);
`tags_test.go` is IR-only → engine round-trip test instead (§4.3.2); `ToolChoice` shorthand
corrected (§6.2); `wantKinds` 12→13 (§3.2); `impl` is an id-less type, not `ir.CodeStep` (§3.1).
