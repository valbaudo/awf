AWF-WORKFLOW 5 "May 2026" "AWF" "AWF Manual"
============================================

# NAME

awf-workflow - the Agentic Workflow Format (AWF)

# DESCRIPTION

The Agentic Workflow Format (AWF) is a declarative format for *agentic pipelines*:
author-defined control flow whose steps are black-box agent CLIs (such as Claude
Code), shell commands, and external-signal waits, run against long-lived
containers, with an independent judge gating each stage. A workflow is a single
YAML document executed by **awf**(1). The current format version is **1**.

Its central construct is the **gate** (see **CONTROL FLOW AND THE GATE**): a
generator, an independent evaluator, and a bounded repair loop. The evaluator is
either an LLM judge or a deterministic check, and it runs independently of the
generator, so the agent's self-report is never the verdict.

This page is the format reference — the fields and their meaning. For what AWF is
and why it works this way, see the project README. The whole document is
content-addressed when a run starts; resuming a run requires the identical
definition (see **CHECKPOINTING AND RESUME**).

# TOP LEVEL

A workflow document has the following top-level shape:

    workflow: <id>
    version: 1
    input: <json-schema>          # optional; run parameters
    input_files: { <name>: <contract> }  # optional; imported workflow file inputs
    env: [ <NAME>, ... ]          # optional; host env-var names forwarded to agent steps
    imports:                      # optional; local subworkflows
      <id>: <relative-path.awf.yaml>
    assets:                       # optional; local files/dirs folded into the definition
      <id>: <relative-path>
    skills:                       # optional; local skill corpora for agent-step routing
      <corpus-id>:
        from: asset.<id>
        layout: skill_dirs
        router: bm25
    agents:
      <role>: { uses: <adapter-ref>, model, system_prompt, with } # optional; reusable roles (see AGENTS)
    containers:
      <name>: { image: <oci-ref>, resources: { cpu, mem } }   # or compose (see CONTAINERS)
    tools:                        # optional; named tool definitions offered to react: steps (see TOOLS)
      <tool-name>: { description, input_schema, impl }
    graph: [ <node>, ... ]
    output_schema: <json-schema>  # optional; required for imported workflow outputs
    outputs: { <field>: <template> }      # optional; typed workflow exports
    output_files: { <name>: step.<id>.files.<name> } # optional; artifact aliases

**workflow**
:   Required. A stable identifier for the workflow.

**version**
:   Required. The AWF format version. Current value: `1`.

**input**
:   Optional. A JSON Schema (see **TEMPLATING AND TYPED OUTPUTS**) for the run
    parameters, referenced as `{{ input.<field> }}`.

**input_files**
:   Optional. The public file-input contract for an imported workflow. Each key
    is a name the caller may bind from a call step; each value is an artifact
    contract. For example:

        input_files:
          report:
            format: json
            schema_ref: asset.report_schema
          notes: {}

**env**
:   Optional. A list of host environment-variable **names** to forward into this
    workflow's agent steps — the in-workflow equivalent of the **awf run
    --agent-env** allowlist, which it extends. Use it so a workflow that needs,
    say, `OPENAI_API_KEY` for its agent declares that itself instead of relying
    on a command-line flag. Each named variable's value is read from the host at
    run time; **only the names** are part of the definition — they fold into the
    content digest and are pinned on resume, while the **values** resolve from the
    host on every run and resume and are never written to the log, blobs, or
    traces. A secret value therefore never appears in the workflow file (list its
    name, not its value). Names must be valid environment-variable identifiers
    (`[A-Za-z_][A-Za-z0-9_]*`). `env:` forwards into agent (`uses:`) invocations
    only; it does not inject into `run:` steps. (Independently of `env:`, a `run:`
    step inherits the host environment on the native backend but not on docker —
    so do not rely on `env:` to reach a `run:` step.)

## Imports

`imports:` maps local import ids to relative `.awf.yaml` files. Import paths are
resolved relative to the declaring workflow file, must stay within that module
directory, are interpreted as slash paths, and must not traverse symlink path
components. Remote imports, absolute paths, backslashes, control characters, and
`..` escape are not supported. The resolved import graph is part of the
workflow definition digest: every imported workflow file, and each imported
workflow's own imports, assets, and Compose dependencies, are content-addressed
with the root definition. Resume against a changed imported workflow is a hard
error, just like drift in the root workflow.

**assets**
:   Optional. A map of stable asset ids to relative local file or directory
    paths. Asset ids use the same syntax as step ids. Values are resolved
    relative to the workflow file's directory and must stay inside that directory
    tree; symlinks are rejected. For example:

        assets:
          fixtures: ./fixtures
          schema: ./schemas/result.schema.json

    Asset bytes are part of the workflow definition digest. At run start the
    loader reads each asset, bounded by implementation limits for per-file bytes,
    total bytes, and total file count, and snapshots those bytes into the
    content-addressed blob store. Directories are snapshotted as their contained
    regular files in deterministic path order. On resume, AWF first verifies that
    the current workflow document and current asset bytes still match the
    recorded definition digest; accepted runs then stage assets from the recorded
    run-start snapshot, not by re-reading the live filesystem.

**skills**
:   Optional. A map of local skill corpora used by agent-step skill routing. Each
    corpus references a directory asset and is part of the definition digest
    through the existing asset snapshot:

        skills:
          project-skills:
            from: asset.skills_dir
            layout: skill_dirs
            router: bm25

    The v1 layout is `skill_dirs`: each child directory is one skill and must
    contain `SKILL.md`. The v1 router is `bm25`: a deterministic weighted
    full-text router over `SKILL.md`, relative file paths, and text-like nested
    files.

**agents**
:   Optional. A map of reusable agent **roles** (see **AGENTS**). Each role is a
    named, digest-pinned configuration of an existing adapter, referenced from a
    step as `uses: <role>`.

**containers**
:   Required. The infrastructure the workflow runs against (see **CONTAINERS**).

**tools**
:   Optional. A map of named tool definitions offered to `react:` steps (see
    **TOOLS**). Each tool has a `description`, an `input_schema`, and an `impl`
    that runs as a containerful step on the existing execution substrate.

**graph**
:   Required. An ordered list of nodes. Sequential composition is implicit:
    sibling nodes run in order.

# CONTAINERS

A declared container is a long-lived instance, created on first use and shared by
every step that names it, for the life of the run. This fits agentic work: an
agent operates in a workspace — it writes files one step reads the next — and a
lab (a database, a browser, a staging API) must stay up
across the generate/evaluate/repair cycle.

A container is backed by either a single digest-pinned image or a Compose project
— never both:

    containers:
      lab:                                  # a multi-service lab
        compose: ./lab/compose.yml          # every image inside is digest-pinned
        service: web                        # the service steps exec into by default
      scratch:                              # a single image
        image: oci://registry.example.com/runner@sha256:abc...   # a digest, not a tag
        resources: { cpu: 2, mem: 4Gi }

**image**
:   One of `image`/`compose`. A single OCI image, content-addressed by digest. A
    mutable tag is rejected, because it would break resume. The sole exception is
    a `map`'s per-element `image:` (see CONTROL FLOW, map): it is rendered from the
    worklist at each element's first boot, where it must itself be a `@sha256:`
    digest (a mutable tag fails that element), then pulled and digest-captured in
    the journal; a committed element is replayed-as-skipped on resume and never
    re-boots or re-resolves the reference. A container declared solely to receive a `map`'s
    `image:` carries `resources:` with no `image:`/`compose:`; it MUST NOT also
    declare a static `image:`/`compose:` (it would be silently overwritten —
    validator AWF1025).

**compose**
:   One of `image`/`compose`. A Compose file for a multi-service lab. Every
    `image:` inside it must be digest-pinned (the validator checks each `image:`
    is `@sha256:`-pinned; it does **not** verify that a service which `build:`s
    its image locally actually matches that digest, so a `build:`+`image:` service
    silently defeats reproducibility), and the file's bytes fold into the workflow
    definition digest.

**service**
:   Required with `compose`. The service that `run:`/`uses:` exec into by
    default. `container: lab:db` addresses another service in the same project.

**resources.cpu** / **resources.mem**
:   Used with `image`. vCPU and memory for a single-image container. For a
    Compose project, resources live per-service in the Compose file.

Compose is Docker's job, not AWF's. Networks, `depends_on`, `healthcheck`, and
multi-service wiring are expressed in the Compose file using Docker's own
machinery. AWF only validates digest-pinning, brings the project up run-scoped
(`up --wait`), routes exec to a service, and tears the project down at run end.

Backend selection is a CLI concern, but the workflow determines whether the
default `--backend auto` can use the native backend. `auto` selects native unless
Docker-only workflow features are present; static image-backed containers,
Compose containers, and runtime `map.image` are Docker-only, so they select
docker. When `auto` records native,
**awf run** warns that the run cannot be resumed until native resume is supported
and suggests **--backend docker** for resumable runs. An explicit
**--backend native** keeps the existing native behavior and does not print that
auto-selection warning. **awf resume** uses the concrete backend recorded in
`run.started`; if that backend is native, resume fails with the existing native
resume limitation and the same **--backend docker** guidance.

Readiness is re-established on every (re)creation, including resume. The runtime
guarantees a container is healthy before dispatching a step into it; it does not
define its own readiness mechanism. A single image becomes ready via its
entrypoint/CMD; a Compose project via healthchecks and `up --wait`. There is
deliberately no `setup` step and no per-step "re-run on resume" flag.

State lives in three places, handled three ways:

**Durable outputs**
:   A step's typed outputs and any declared `output_files` commit to a
    content-addressed artifact store. This is what survives a crash and what
    resume reads back.

**Infrastructure**
:   Reconstructed from the image/Compose recipe on every (re)creation. A rebuilt
    lab is *more* reproducible than a restored image — the property the digest
    pin already demands.

**Unmanaged in-container mutation**
:   In-container process or filesystem mutation that is neither a declared output
    nor reconstructable from the recipe is *not* preserved across a checkpoint.

For the one case the recipe cannot serve — an agent that mutated a workspace
expensively and nondeterministically, where a later step needs that state after
resume (a coding agent's evolving working tree) — a container can opt in to a
filesystem snapshot:

    workspace:
      image: oci://...@sha256:...
      snapshot: workspace        # capture a copy-on-write FS diff at each commit; restore on resume

The runtime then captures a copy-on-write diff (not a squashed commit) at each
commit boundary and restores it instead of rebuilding from the recipe. It is off
by default and scoped to mutable-workspace containers.

Two consequences to keep in mind:

- Parallel branches that mutate state need distinct containers / Compose
  projects; the validator rejects concurrent writers to one workspace.
- Loop and repair iterations accumulate state in the same container — usually
  what you want (the lab stays up), occasionally not (reset explicitly with a
  step).

# TOOLS

`tools:` is a top-level map (a sibling of `graph:` and `outputs:`) from tool name to a tool
definition. Tools are offered to a `react:` step's model; the model calls them by name.

    tools:
      <tool-name>:
        description: <string>          # required — sent to the model
        input_schema: <JSON Schema>    # required — the tool's parameters (the JSON-Schema floor applies)
        impl:                          # required — how the tool runs
          run: <command>               # run: only (no exec:); read structured args from {{ args_file }}
          container: <name>            # a containers:-declared name (NOT an inline image)
          timeout: <duration>          # optional
          output_files: { ... }        # optional (captured, but NOT surfaced to the model in v1)
          input_files: { ... }         # optional; static artifact/asset refs, staged before run
          retry: { ... }               # optional

The model's call arguments reach `impl` two ways: the full arguments JSON is staged into the
container and exposed as `{{ args_file }}`; top-level scalar fields are also bound as
`{{ args.<field> }}` (best-effort — absent if non-scalar or unparseable). Read structured arguments
from `{{ args_file }}`; never interpolate raw arguments into a shell command line.

The impl's `input_files` (static `step.<id>.files.<name>`, `input.files.<name>`, or `asset.<id>`
refs — never `{{ }}`) are staged into the container before `run`, alongside the per-call
`args_file`; a destination that collides with the `args_file` path fails the step. Each ref is
validated against every `react:` step that offers the tool, so a `step.<id>` producer must run
before each such react node.

Each `impl` runs as an ordinary containerful step on the existing execution substrate. The
container is a `containers:`-declared name, digest-pinned there like any step's image.

# AGENTS

A **role** is a reusable, digest-pinned configuration of an existing agent — a
name you bind once under top-level `agents:` and then reference from any number of
steps as `uses: <role>`. It lets a fleet of steps share one agent setup (the same
model, system prompt, and tools) without repeating it.

    agents:
      auditor:
        uses: anthropic/claude-code   # an EXISTING adapter (anthropic/claude-code, openai/codex, ...)
        model: <model>                # optional; an opaque with: key the base adapter reads
        system_prompt: <text>         # optional; opaque with: key
        with: { mcp_servers: [ ... ] }   # optional; opaque base-adapter config

A role resolves at run start to its base adapter (the `uses:` ref). Its
`model`, `system_prompt`, and `with:` are **defaults** a step may override. (A
role does **not** set a typed-output contract — `output_schema` is the step's
own, not the role's.)

**uses**
:   Required. An **existing** adapter ref (`anthropic/claude-code`,
    `openai/codex`, ...). A role wraps a registered base adapter; it does not
    define a new one. A role with an empty `uses:`, or a role **name** that
    contains `/` (the `<vendor>/<name>` form is reserved for adapter refs and
    would be ambiguous with one), is rejected (**AWF1033**).

**model** / **system_prompt**
:   Optional defaults. `model` and `system_prompt` are convenience fields the run
    folds into the role's `with:` as opaque keys — the base adapter reads them
    (AWF never interprets a `with:` key). A role does not carry an
    `output_schema`: the typed-output contract is the **step's** own
    `output_schema`.

**with**
:   Optional. Opaque base-adapter config, validated by the named adapter, never
    read by the core. This is where the shared **memory MCP handle** rides: carry
    `mcp_servers` (or the adapter's equivalent) on the role and every step using
    that role gets the same fleet-memory tool.

A step's own `with:` shallow-merges **on top** of the role's (a step key wins).
The merge is **key-blind** — AWF never interprets a `with:` key; it stays opaque
to the named adapter. The role names and values fold into the definition digest
and are pinned on resume, exactly like `env:`.

**Static role `with:` vs. templated step `with:` (format contract).** A role's
`with:` is **static**: it resolves once at run start, *before* any template scope
exists, so it is **never** `{{ }}`-substituted. A step's own `with:` **is**
substituted (`{{ run.id }}`, `{{ step.* }}`, ...) before it overlays on top of the
role's. So a per-run **scope id** that ties the whole fleet to one memory
namespace is supplied by the **step's** `with:` as a templated scalar — for
example `with: { scope: "{{ run.id }}" }` — which the key-blind overlay places on
top of the role's static `with:`. (Top-level `env:` is a host-var **name**
allowlist, not a value map, so it cannot carry a templated value — do not use
`env:` as a scope-id channel.)

# STEPS

A step node is exactly one of four black boxes; AWF runs it and does not look
inside. A node with more than one, or none, is invalid.

- Code step (`run:`) — a shell command.
- Agent step (`uses:`) — an external agent-runtime invocation.
- Signal step (`await:`) — block until an external signal.
- Call step (`call:`) — run an imported workflow.

## Code step (run)

    - id: <id>
      container: <name>
      run: <command>
      timeout: <dur>                 # optional; on expiry -> retryable_failure
      output_schema: { ... }         # optional; step writes JSON to $AWF_OUTPUT
      output_files: [<path>, ...]    # optional; bare list -> capture-only
      # output_files: { <name>: <path> }   # ...or a name->path/contract map -> named, referenceable
      input_files: { <dst>: step.<id>.files.<name> }   # or asset.<id>; optional
      idempotency_key: <template>    # optional; for effects outside the container
      retry: { ... }                 # optional

Implicit outputs are always `exit_code` and `stdout`. `output_schema` adds typed
fields (the step writes conforming JSON to the file named by `$AWF_OUTPUT`; the
runtime sets that variable but does **not** create its parent directory, so the
step must `mkdir -p "$(dirname "$AWF_OUTPUT")"` before writing — a missing file
is a `retryable_failure`, not a typed verdict);
`output_files` captures artifacts and `input_files` stages prior artifacts in
(see **Artifact channel** below). A nonzero exit is a `retryable_failure`
unless its code is declared permanent (see **OUTCOMES, RETRY, AND REPAIR**).

## Agent step (uses)

Delegates a task to a named agent runtime and captures a typed result. AWF
carries an opaque `with:` map whose schema the runtime owns and validates, so the
format never hard-codes one harness's options.

    - id: <id>
      container: <name>             # optional; required unless the runtime is containerless
      uses: <agent-runtime-ref>      # e.g. anthropic/claude-code, factory/droid, or block/goose
      continues: <id>                # optional; id of a prior agent turn this turn continues
      with: { ... }                  # opaque; validated by the runtime
      output_schema: { ... }         # required iff outputs are referenced downstream
      output_files: [<path>, ...]    # optional; or { <name>: <path|contract> } -> named
      input_files: { <label>: step.<id>.files.<name> }  # or asset.<id>; optional; label is an in-container path (container) or a logical name forwarded inline (containerless)
      skills:
        from: <corpus-id>
        query: <template>
        limit: <positive-int>
        into: <absolute-container-path>
      timeout: <dur>                 # optional
      idempotency_key: <template>    # optional
      retry: { ... }                 # optional

**uses**
:   Required. The runtime ref. `uses:` resolves **first** against a declared
    `agents:` role (see **AGENTS**), then against a registered base adapter ref.
    A `uses:` that matches neither a declared role nor a `<vendor>/<name>` adapter
    ref is rejected at validation (**AWF1034**). The identity *and version* are
    pinned at run start; a role is pinned as a first-class runtime (its resolved
    base version is drift-checked on resume).

**container**
:   Optional for agent steps. A runtime whose adapter is *containerless* — it
    performs its work without a container, for example via a direct network call
    — may omit `container:`; the runtime is then resolved and pinned with no
    container. A `container:` that is present must still resolve to a declared
    container. Code (`run:`) steps and non-containerless agent runtimes still
    require a container.

**with**
:   Required. Opaque runtime config (one runtime takes `{model, prompt, tools,
    max_turns}`; another takes `{models, budget}`). The core never reads its
    keys.

**continues**
:   Optional (`continues:` field). The `id` of an agent step that **dominates** this turn (an earlier
    step guaranteed to have run on every path reaching this one) and uses the
    **same** agent runtime. The engine prepends that step's conversation (its
    prior turns, verbatim) before this turn's prompt, so the model continues one
    thread. You may **not** inline `messages:` in `with:` — the conversation is
    engine-owned and durable. A target inside a `gate`/`map` this turn is
    *outside* of, or reachable only through **nested loops**, is rejected (it is
    not addressable); to gate a conversation, gate its **leaf** turn or place a
    whole sub-conversation inside one gate's `generate`. A step inside a gate's
    `evaluate:` block may not use `continues` (the evaluator judges in a fresh
    context). For map/parallel fan-out caching, branches must share
    `system_prompt` (the prefix cache matches from the start).

**output_schema**
:   Required iff a `step.<id>.<field>` of this step is referenced elsewhere.

Outputs are typed, never free text — this is what makes the judge work. When
`output_schema` is declared the runtime produces conforming output, via its
constrained/structured-output mode or schema-aligned parsing of the final message
(tolerant of fences, prose, and minor slips). If neither yields a conforming
value within the retry budget the step is a `retryable_failure`. References bind
only to typed fields, so `**verdict: pass**` versus `verdict: pass` can never
silently break a gate.

**input_files**
:   Optional. Stages prior artifacts for the step. The key semantics depend on
    the runtime mode:

    - **Container runtime** — the key is an **absolute in-container path**; the
      artifact bytes are staged (written into the container) at that path before
      the agent launches.
    - **Containerless model runtime** (e.g. `awf/llm`) — the key is a
      **logical label**; the artifact bytes are forwarded to the model as
      **inline message parts** (images and PDFs, per the adapter's and
      endpoint's support). The MIME type is sniffed from content, not inferred
      from the file name. Rasterize PDFs to images first only when targeting an
      image-only endpoint (e.g. Ollama).

    Both forms accept `step.<id>.files.<name>` or `asset.<id>` as the source.

**skills**
:   Optional. Routes a runtime query against a declared top-level skill corpus
    and stages only selected skill directories into the step container before the
    adapter launches. `query` is substituted using the normal step scope and must
    render to a string. `limit` must be positive and no more than 64. `into` must
    be an absolute clean container path, and it must not be `/`; selected files
    land under `<into>/<skill-id>/...`. A step using `skills:` must have a
    container; containerless skill delivery is not part of v1. `input_files`
    destinations must not overlap `into` in either direction.

Agent steps are atomic: one invocation is one checkpoint boundary, and resume
re-runs the whole step from its pre-step snapshot. The agent's internal loop is
its own business.

## Skill routing

Skill routing selects a small local skill library for one agent step.

The v1 `bm25` router builds a weighted token stream for each skill directory:
`SKILL.md` body tokens have weight 4, relative file-path tokens have weight 2,
and text-like nested file body tokens have weight 1. A nested file is text-like
when it is valid UTF-8, contains no NUL bytes, and at least 85% of its runes are
printable or whitespace. `SKILL.md` contributes only as weight-4 `SKILL.md` body
tokens plus the path tokens for `SKILL.md`; it is not also counted as a nested
text-like file.

Tokenization is deterministic: the tokenizer lowercases Unicode letters and
digits, treats any non-letter/non-digit as a separator, and emits non-empty
tokens. Relative file paths are tokenized the same way as text. Repeated query
tokens count once.

`bm25` uses `k1=1.2`, `b=0.75`, and the standard Robertson/Sparck Jones IDF
variant used by Lucene-style BM25:
`log(1 + (N - df + 0.5)/(df + 0.5))`, where `N` is the corpus size and `df` is
the number of skill documents containing the query token. Document length is the
length of the weighted token stream, so token repetition from weights affects
length. Results sort by score descending, then by skill id ascending, and AWF
returns only skills with a positive score. Scores must be finite JSON numbers.

If no skill scores above zero, AWF treats routing as a permanent step failure
before dispatch and before appending `skills.selected`. It never silently
delivers an empty skill directory set.

For a successful fresh selection, AWF appends and fsyncs
`skills.selected{library, library_digest, router, router_version, router_params,
selected[]}` before dispatch. `selected[]` records selected ids and scores. AWF
does not store the rendered query text or a query hash.

On resume, AWF reuses the recorded selected ids instead of routing again. Replay
validates the pinned run-start corpus, router name, router version, and router
params before staging the recorded skill directories.

Deterministic BM25 v1 is inspired by SkillRouter full-body retrieval. It is not a
neural encoder/reranker, and it is not the paper-level 80K registry behavior.

## Call Steps

A call step runs an imported workflow and exposes only that workflow's declared
exports.

    - id: <id>
      call: <import-id>
      input: { ... }                 # optional; default {}
      input_files: { <name>: step.<id>.files.<name> } # optional; or asset.<id>
      timeout: <dur>                 # optional
      retry: { ... }                 # optional

**call**
:   Required. Must reference an id declared in top-level `imports:`. Remote or
    dynamic workflow references are not supported.

**input**
:   Optional. Defaults to `{}`. Each value is evaluated with the same typed
    template rules as step input values, then the resulting object is validated
    against the imported workflow's top-level `input` schema. If the imported
    workflow has no `input` schema, only `{}` is valid.

**input_files**
:   Optional. Maps the child workflow's public file input name to a parent
    artifact reference. Each key must be declared by the imported workflow's
    top-level `input_files:` contract; each value is a static artifact reference
    such as `step.<id>.files.<name>` or `asset.<id>`.

        - id: analyze
          call: analyzer
          input_files:
            report: step.collect.files.report

Call steps may set only the common step fields that apply to a black-box
execution boundary: `id`, `call`, `input`, `input_files`, `timeout`, and
`retry`. They must not set `container`, `uses`, `run`, `await`, `with`,
`output_files`, or `idempotency_key`; those belong to the caller's normal step
execution surface or to the child workflow's own contract. A call step's
`input_files` binds parent artifacts to the child workflow's public file input
names.

The call product is addressable as `step.<id>.<field>` and
`step.<id>.files.<name>`, using only the imported workflow's declared
`output_schema`, `outputs:`, and workflow-level `output_files:` aliases. Imported
internals are private: callers cannot reference child steps, child containers,
or child artifacts except through those exports. Mechanical failures inside the
imported workflow propagate as the call node's mechanical outcome; quality
decisions remain the called workflow's own gate responsibility.

## Artifact channel (output_files, input_files)

`output_files` and `input_files` hand a file produced by one step to a *later*
step — across **distinct** containers, content-addressed and resume-safe. The
producer declares what it writes out; the consumer references it by name and the
runtime stages the bytes in before the consumer runs. This is the file-handoff
seam between black boxes: an agent writes a report in one workspace, a code step
verifies it in a clean one. Both fields appear on code (`run:`) and agent
(`uses:`) steps.

The name shape depends on the surface:

- Step input_files uses destination path -> artifact ref.
- Call input_files uses child public name -> artifact ref.
- Workflow input_files uses public name -> artifact contract.

Child steps consume inbound call artifacts through normal step staging:

    input_files:
      /work/report.json: input.files.report

Call input files are file-valued. A call input may bind a named `output_files`
artifact or a single-file asset. Directory assets remain valid only for normal
step `input_files` because those bindings provide a destination tree.

**output_files (three forms)**
:   A **bare list** of paths — `output_files: [/out/a, /out/b]` — captures each
    path into the artifact store on commit, capture-only and unchanged from
    earlier versions; those artifacts are durable but not referenceable by a
    later step. A **name->path map** — `output_files: { report: /out/r.md }` —
    is *named*: it captures the path **and** publishes a handle
    `step.<id>.files.<name>` that a consumer's `input_files` can reference. The
    handle name and the container path are independent; the consumer chooses its
    own destination path. The two forms are mutually exclusive per step (a step's
    `output_files` is either all bare or all named).

    A named entry may also be a **contract object** instead of a string path:

        output_files:
          summary:
            path: ./out/summary.json
            format: json
            schema:
              type: object
              required: [status]
              properties:
                status: { type: string }
          rows:
            path: ./out/rows.jsonl
            format: jsonl
            schema_ref: asset.row_schema

    Contract objects require `path`. `format`, when present, is `json` or
    `jsonl`. A contract with `schema` or `schema_ref` must declare `format`;
    `schema` and `schema_ref` are mutually exclusive. `schema_ref` names a
    top-level asset containing the schema bytes, using `asset.<id>`. An invalid
    captured artifact is a mechanical failure at capture time, before the step
    can produce `ok`. JSONL means UTF-8 text with exactly one valid JSON value
    per physical line; blank or whitespace-only lines are invalid, CRLF is
    accepted, and a final trailing newline is allowed.

    Both forms' **container paths are `{{ }}`-substituted** from the step's scope
    (exactly like `run:` and `idempotency_key`) before capture, so a path such as
    `/work/records/{{ input.cve_id }}.json` captures — and, for a named form, is
    referenced — under its substituted name. This is the path on the *producer's*
    side; the `input_files` reference itself stays a static
    `step.<id>.files.<name>` handle, never a `{{ }}` template (see below).

**input_files**
:   A map of *in-container destination path* -> *artifact reference* —
    `input_files: { /work/report.md: step.recon.files.report }`. The reference
    may also be `asset.<id>`, which stages the run-start snapshot of a top-level
    asset, or `input.files.<name>` inside a called workflow, which stages a file
    artifact bound by the parent call step. Before the step runs, the runtime
    resolves each reference to its
    committed, content-addressed blob and writes the bytes to the destination
    path inside this step's container, creating parent directories as needed and
    overwriting any existing file. Destination paths are `{{ }}`-substituted
    from the consumer step's scope before staging, so a path such as
    `/work/records/{{ input.cve_id }}.json` is valid after substitution. The
    right-hand side is a **static reference**, not a `{{ }}` template (like
    `container:`); the bytes themselves are opaque to the runtime.

    A `step.<id>.files.<name>` reference must name a **prior, in-scope** step
    that declared a *named* `output_files` artifact of that name, exactly as a
    `step.<id>.<field>` reference must name a declared output field. Scope
    reachability is mostly the
    same, with one file-specific exception: after a `gate` passes, a later
    `input_files` reference may point at a producer inside that gate, and the
    runtime resolves it to the accepted attempt's committed artifact. Scalar
    `step.<id>.<field>` references remain gate-scoped; this exception exists only
    for durable files. A producer inside a `map` body is still not referenceable
    from outside unless the map has a `reduce:` product. An `asset.<id>` reference
    must name a declared top-level asset. An `input.files.<name>` reference must
    name a public workflow `input_files:` contract entry that the parent call
    bound for this child run. Destination paths must be **absolute
    and clean after substitution** — no `..` segment — and distinct (overlapping
    parent/child destinations are undefined). A reference that fails any of these
    — undeclared producer, undeclared artifact name, undeclared asset, undeclared
    workflow file input, a templated right-hand side, or a non-absolute /
    `..`-containing destination — is rejected
    at validation (**AWF3007**).

    `input_files` **requires a container**: it is rejected on a *containerless*
    agent step (one whose runtime omits `container:`), since there is no container
    to stage into.

The handoff is crash-safe: because the reference resolves to a committed,
content-addressed artifact, resume re-stages the same bytes from the same blob
without re-running the producer (see **CHECKPOINTING AND RESUME**). Staging is
in-memory: each staged artifact materializes fully in memory (as `output_files`
capture does), so peak memory is roughly the sum of staged sizes times the
in-flight `map`/`parallel` width, retained across retry backoff. Large-artifact
streaming is out of scope; route big payloads as a single artifact, not many.

## Signal step (await)

    - id: approve
      await: <signal-name>
      where: 'candidate_id == "{{ <ref> }}"'   # optional; consume the signal whose payload matches
      timeout: <dur>                 # optional; on expiry -> retryable_failure (no payload)
      output_schema: { ... }         # optional; validates the delivered payload

Blocks until a signal of that name is delivered (for example, a human approval
before opening a PR). Signals are durable and buffered: journaled on receipt even
before the `await` is reached, consumed earliest-first per name, and never lost
across a restart. No container is needed. Deliver one with **awf signal** (see
**awf**(1)).

**where**
:   Optional. A **bounded boolean** expression selecting *which* buffered signal
    of that name to consume. Without `where:`, signals are consumed
    earliest-first per name (the default above). With `where:`, the engine
    consumes the **earliest-seq** buffered signal whose **payload satisfies the
    expression**; non-matching signals stay buffered for other awaits. This
    correlates an async/out-of-band signal to the right work item — for instance,
    a `map` body that awaits the `oob-hit` whose `candidate_id` equals its own
    item, regardless of arrival order.

    The expression is the same bounded boolean form as `if`/`loop`/`gate`
    conditions: comparisons plus `&&` / `||` / `!`, **no arithmetic**. `{{ … }}`
    slots inside it substitute from the surrounding scope (e.g. `{{ hyp.id }}`
    for a `map` item's `as` binding) **before** matching; **bare identifiers
    resolve against the delivered signal's JSON payload** (e.g. `candidate_id` is
    `payload.candidate_id`).

    **Quoting (required for string correlation values).** A `{{ … }}` slot
    substitutes its *rendered text* into the expression before the expression is
    parsed. To compare against a **string** value you MUST wrap the slot in
    literal double-quotes: `where: 'candidate_id == "{{ hyp.id }}"'`. With a
    string `hyp.id` of `a`, the unquoted form `candidate_id == {{ hyp.id }}`
    renders to `candidate_id == a`, where the bare `a` parses as a *payload
    reference* (an identifier), not the string literal `"a"` — so it never
    matches (and typically fails to resolve). The quoted form renders to
    `candidate_id == "a"`, a string-literal comparison, which is what you want.
    Numeric correlation values (e.g. `count == {{ n }}`) need no quotes — a bare
    number parses as a number literal. (This mirrors how any bounded-boolean
    field treats a bare identifier as a reference; see **TEMPLATING AND TYPED
    OUTPUTS**.)

    A signal whose payload is not a JSON object is skipped by the matcher (never
    consumed by a `where:`). If no buffered signal matches before `timeout`, the
    await is a `retryable_failure` exactly as if no signal had arrived. Because
    matching reads payload fields, pair `where:` with `output_schema:` when the
    payload shape matters. A `where:` clause that is not a valid bounded boolean
    expression is rejected at validation (**AWF1036**).

# CONTROL FLOW AND THE GATE

Control flow is author-defined and block-structured — composed by nesting, not by
declaring DAG edges. Data flows implicitly through the shared container and
explicitly through typed references (see **TEMPLATING AND TYPED OUTPUTS**).

## if

    - if: { cond: <expr>, then: [<node>...], else: [<node>...] }   # else optional

Branches on a typed condition. A false `cond` with no `else` is a no-op. Combined
with `skip`, this routes a stage out of a pipeline without nesting everything
after it.

## loop

    - loop: { until: <expr>, max_iters: <n>, body: [<node>...] }   # at least one of until/max_iters

`body` repeats; `until` is tested *after* each iteration (do-while), so it may
read what the body just produced. A reference to a step inside the loop resolves
to its most recent iteration. Use a `loop` for plain repetition with no judge
(polling, or a fixed worklist). For generate-and-judge, use a `gate`, not a bare
loop; for a data-driven worklist whose size is known only at runtime, use `map`.

## try

    - try: { do: [<node>...], catch: [<node>...], finally: [<node>...] }   # catch/finally optional

On a failure escaping `do`, runs `catch`. `finally` runs unconditionally —
including on cancellation — for app-level cleanup (close handles, post a status,
revoke a token). Container/Compose teardown is automatic at run end and on
cancellation, so `finally` is *not* needed for that. AWF has no separate
compensation primitive.

## parallel

    - parallel: [<node>, ...]

Children run concurrently; the node completes when all do. A child failing after
its retries cancels its siblings, then propagates. Branches that run steps must
target distinct containers / Compose projects — the validator enforces this.

`parallel` does not accept a `reduce:` key (its wire-form is a bare node array):
fan-in is supported on `map` only. A fixed cohort that needs a fan-in verdict is
expressible as a one-item-per-role `map` with `reduce: {quorum}`.

## gate

The flagship — TDD applied to a black-box step. A gate runs a *generator*, then an
*independent evaluator* (the test), and if the evaluator's bar is not met it
*repairs* — re-running the generator conditioned on the evaluator's feedback —
until the bar is met or attempts run out.

    - gate:
        generate: [<node>, ...]      # produces (and, on repair, revises) the artifact
        evaluate: [<node>, ...]      # the independent judge; verdict = the block's final typed output
        until: <expr>                # pass condition over the verdict
        max_attempts: <n>            # bound on generate->evaluate cycles

Semantics:

1. Run `generate`, then `evaluate`. The *verdict* is the typed output of
   `evaluate`'s last node. Test `until` over it.
2. `until` true — the gate *passes* and flow continues.
3. `until` false — *repair*: re-run `generate`, then `evaluate`, and re-test.
4. Attempts exhausted — the gate is `rejected`, which propagates like any failure
   to the nearest `try`/`catch` (or halts the run).

A verdict is not a crash. The gate distinguishes a *mechanical failure* of
`generate`/`evaluate` (crash, `timeout`, transport — handled by that step's own
`retry`) from a *verdict* (the evaluator ran and `until` is false). Only a verdict
repairs and consumes an attempt. If a step fails mechanically after its own
retries, the gate fails and propagates — you cannot repair a crash, and a broken
judge must never be read as a rejection. So `max_attempts` bounds quality cycles,
never flakiness.

A gate is not a `loop` with a check inside it; the runtime enforces the two
properties that make the pattern work:

**Enforced independence**
:   An LLM judge runs as a *fresh agent context* — a new session, never the
    generator's continued conversation — so it cannot be steered by the
    generator's reasoning; a deterministic/code judge is independent by
    construction. The judge does share the container filesystem (it must, to see
    the artifact), so a good check tests *behavior, not artifacts*: it runs the
    work and confirms the real effect — execute the tests and read the exit code,
    or query the database and count the rows — rather than trusting a status the
    generator wrote.

    The same caution applies to typed outputs: an evaluator may reference a
    generator step's output (commonly a path — `{{ step.<gen>.<path_field> }}` — to
    locate the artifact it must inspect), but it should use that to *test the
    artifact's behavior*, not to trust a self-reported status field the generator
    declared about its own work.

**Automatic feedback**
:   On every attempt after the first, the runtime makes the previous verdict
    available to `generate` — resolvable as `{{ evaluate.<field> }}` and injected
    into an agent generator's context — so regeneration is conditioned on the
    critique. The author does not wire this up. On the first attempt
    `evaluate.*` is empty. Because the verdict is fed into the next generator,
    an evaluator that inspects untrusted or adversarial input must keep raw
    input bytes out of the verdict's typed fields (route them through
    `output_files` instead) — otherwise the verdict becomes an injection channel
    into the generator.

Constraints: `generate` must be non-empty (a gate with no generator cannot
repair); the final node of `evaluate` must declare `output_schema` (the verdict
`until` reads); `max_attempts` is required (stochastic generators can loop
forever). A gate nests anywhere a node can appear, including inside another gate's
`generate`.

Human escalation is a pattern, not a primitive: wrap the gate in `try`/`catch`
with an `await` in the `catch` to put a human in the loop after the repair budget
is spent.

## skip

    - skip: <reason>          # optional reason, recorded in the trace

Cleanly terminates the *nearest enclosing scope* — the current `loop`/`gate`
iteration, a `parallel` branch, or (if none) the *run* — as `ok`, after running
any `finally` blocks it unwinds through. Inside a `parallel` branch it ends only
*that branch* (siblings keep running). It is how a stage bails without nesting the
remainder: "triage found no source -> `skip`; move on." `skip` is `continue`-like,
not `break`: it ends the current iteration/branch, not a whole loop.

## map

    - map:
        id: <id>                     # optional; named aggregate product
        over: <expr>                 # a typed array, size known only at runtime
        as: <name>                   # each element bound as {{ <name>.<...> }} and {{ <name>.index }}
        container: <name>            # per-item container/compose instance (one per element)
        image: <template>            # optional; the per-element container's image, resolved at runtime
        concurrency: <n>             # max elements in flight at once
        min_success: <ratio|n>       # optional; fan-in succeeds if at least this many do (default: all)
        body: [<node>...]
        reduce:                      # optional; fan-IN — collapse the N branch outputs to one
          quorum: <n|ratio>          #   built-in: succeeds iff at least this many branches pass `over`
          over: <field>              #   the per-branch boolean output field quorum counts
          # — OR an author reducer (declare exactly one of quorum: or run:) —
          run: <command>             #   a shell reducer (a CODE step)
          container: <name>          #   REQUIRED on a run: reducer — the declared container it runs in
          output_schema: <json-schema>     #   the reduced node's typed output
          output_files: { <name>: <path> } #   the reduced node's artifacts
        prune:                       # optional; frontier — cancel result-blind losers as items commit
          score: <field>             #   a NUMERIC field the body's last step declares in output_schema
          keep: top(<k>)             #   keep the k highest scorers; prune the rest
          # — OR (declare exactly one of keep: or stop_when:) —
          stop_when: "<bool-expr>"   #   over best.score; once true, prune everything still running

Data-driven expansion when the worklist size is known only at runtime — a crawl
finds N pages, a query returns N records. Each element runs `body` in its *own*
container instance (the distinct-container rule applied per element), up to
`concurrency` at a time. `min_success` lets the fan-in tolerate partial failure
instead of cancelling every sibling on the first one. Use `parallel` for a
static, author-known set of distinct branches; use `map` for a runtime-sized set
of identical ones.

`id` names the map's aggregate product. Step ids and map aggregate ids share one
namespace: duplicate step/map ids fail validation. Aggregate output ids must not
duplicate sibling step ids where a downstream `step.<id>` reference would be
ambiguous.

`image` supplies the per-element container's image from the worklist instead of
a static `containers:` declaration. Unlike a top-level image it MAY be a template
(e.g. `{{ <as>.image }}`) that the map learns only from `over` — the one place an
image is not known before the run. At first boot of each element the runtime
records the content digest of the image that booted into that element's journal
entry; on resume, committed elements are replayed from the journal (their bodies
do not re-execute and their containers are not re-created), so a reference that
has since moved cannot change a resumed element. An element whose rendered image
cannot be booted fails that element only — committed as `item_failed` with
`reason: image_unavailable`, counted against `min_success`, never the whole map.
A `map.image` template that fails to render, or renders to an empty string, is a
deterministic definition error: it fails the whole map as `permanent_failure`
(like an unrenderable `over`), not a tolerated item. So is an element whose
per-element spec the backend rejects — a malformed `resources:` (`mem`/`cpu`), or
a host config the daemon refuses: an invalid spec is the author's pinned
definition, not run-data, so it fails the whole map rather than being tolerated.
Only a valid reference that cannot be pulled or booted — or a rendered reference
that is not a `@sha256:` digest — is the tolerated `image_unavailable`. The
template source text
folds into the definition digest like every other field; the resolved digest is
run state, not definition. The container named by `container:` supplies the
per-element handle and any resources; with `image:` it carries `resources:`
alone and MUST NOT also declare a static `image:`/`compose:` (a static pin would
be silently overwritten per-element — rejected by the validator, AWF1025).

NOTE: the docker backend boots a `map` `image:`; the native backend does not (it
advertises no runtime-image capability, so `awf run`/`resume` rejects such a
workflow at run start on native). On docker the rendered reference MUST be a
`@sha256:` content digest — a mutable tag is rejected — because the booted bytes
must be content-addressed for resume to be reproducible.

A `map`'s `image:` is rendered from worklist data a previous step produced —
which may be agent-authored. The rendered reference is therefore not part of the
trusted, validator-pinned definition: the runtime boots whatever it resolves to.
Treat the producing step as the trust boundary. The docker backend requires the
rendered reference to be an `@sha256:` content digest, so a fabricated or mistyped
mutable tag is rejected before any pull — failing that element rather than pulling
arbitrary moving bytes. This is enforced at first boot (the earliest the reference
is known), **not** as a static-validation guarantee; an optional allowlist of
permitted registries remains planned.

A later step reads a `map`'s per-item results in aggregate with a `step.<id>`
reference to the map aggregate id, or to a step inside the body when the map has
no reducer, evaluated from outside the map (see TEMPLATING AND TYPED OUTPUTS).

`reduce:` collapses the N fanned-out branch results into ONE. The reduced output
*replaces* the map's per-item array: a downstream `step.<map-id>.<field>` then
resolves to the reducer's typed output, and `step.<map-id>.files.<name>` to the
reducer's named artifacts. The aggregate stays engine-internal — with `reduce:`
it never escapes as an array, so the array-only-in-`over:` rule (AWF5004) is
unaffected. Existing body-step references inside the reducer remain valid so the
reducer can read what the map body produced. Declare **exactly one** of `quorum:`
or `run:` (**AWF1035**). `reduce:` is supported on `map` only (parallel does not
accept it — see *parallel*).

`quorum: k` (the debate / cohort case) succeeds iff at least `k` branches produced
a true `over` field; the reduced output is `{passed, votes, agree}`. `quorum`
generalizes `min_success`: `min_success` is quorum over the mechanical success
predicate. There are no named-threshold keywords; a quorum is always the numeric
`k` (an int count or a fraction). Conceptually, "any" is quorum(1), a "majority"
is quorum(⌈N/2⌉), and "unanimous" is quorum(N) — but each is written as that
numeric `k`, never as the word. A `quorum` ratio reuses the `min_success` int-or-
fraction form. Declaring both `min_success` and `reduce: {quorum}` on one node is
rejected (**AWF5006**), as is a `quorum` whose `over:` names a field no body step
declares in its `output_schema` (**AWF5006**). A `quorum` reduce that is not met
ends the node as `retryable_failure`, exactly like an unmet `min_success`.

An author `run:` reducer (the merge / dedupe case) is a code step, so it runs in
its **required `container:`** — a `run:` reducer with no `container:` is rejected
(**AWF1035**), and a `container:` that resolves to no declared container is
**AWF1009**, the same rule every code step obeys. (The reference design's §3.2a
`run:` example omits `container:` for brevity; that is shorthand, not the format
contract — a `run:` reducer always declares one.) Before it runs, the engine
stages into that container every branch's named `output_files` artifact (under
`/work/.awf/branch-<N>/<name>`) plus a canonical-JSON manifest of all branches'
typed outputs at `/work/.awf/aggregate.json` — deterministic, index-ordered, and
committed-branches-only — via the same content-addressed delivery the artifact
channel uses (see *Artifact channel*). The reducer reads them and writes its
declared `output_files` and `$AWF_OUTPUT`, which become the reduced node's
artifacts and typed output. If a reducer `output_files` path templates a body
aggregate such as `{{ step.collect.name }}`, the aggregate renders as canonical
JSON; the resulting container path is literal JSON text, not a sanitized file
name. Prefer scalar fields or a fixed output path for reducer artifacts.

Example named aggregate with reducer artifacts:

    steps:
      - map:
          id: version_universe
          over: ${{ step.versions.items }}
          as: version
          do:
            - id: collect
              run: ./collect.sh
              output_files:
                files: ./out/files.jsonl
          reduce:
            id: merge
            run: ./merge.sh
            output_files:
              files: ./out/files.jsonl

`prune:` turns a result-blind fan-out into a result-aware *frontier search*. As
items commit, the engine reads a typed `score` per item and cancels the losers
still in flight or not yet started — a hypothesis search that does not pay for the
branches it has already beaten. It is an optional clause on `map` only; `parallel`
has no `prune:` (see *parallel*). Declare a required `score:` and **exactly one**
of `keep: top(<k>)` or `stop_when: "<bool-expr>"` — neither, both, an empty
`score`, or a `keep` that is not `top(<positive int>)` is rejected by the
validator (**AWF1037**).

`score:` names a **numeric** field the body's **last step** declares in its
`output_schema`. The engine reads it from the committed item as a typed number —
never parsed from text; a `score:` that does not name a numeric field of that
schema is rejected (**AWF5008**).

`keep: top(k)`: as items complete, the engine keeps the `k` highest-scoring items
and prunes the rest — in-flight items are cancelled, not-yet-started items never
start. Ties beyond `k` are broken by **item index (lowest index wins)**, so the
survivor set is deterministic.

`stop_when: "{{ best.score >= 0.9 }}"`: a bounded boolean expression over
**`best.score`** — the running best score so far. Once it is true, every
still-running and not-yet-started item is pruned. It is the same bounded evaluator
as `loop`/`gate` `until` — no arithmetic, calls, or loops (templating is not a
language).

Interaction with `min_success`: pruned items are removed from **both** the
numerator and the denominator — they are neither passes nor the "all" baseline. A
`map` with `min_success` unset (= all) plus `prune:` succeeds when every
**non-pruned** item passed; a fraction is measured against the non-pruned set
(`min_success: 0.5` over 10 items where 6 were pruned requires 2 of the 4
survivors, not 5 of 10).

Resume: a prune map commits its whole per-item disposition (each item's
`item_passed` / `item_pruned` / `item_failed` status) **atomically** as a single
durable record once the frontier settles; on resume that record is **replayed
verbatim** — surviving items' bodies are not re-run and the prune is not
re-decided. The committed journal is authoritative for which items survived; the
frontier is never re-derived from a partial score set, because items can commit in
a different order across runs and a re-derived `top(k)` tie-break or first-firing
`stop_when` could pick a different survivor set. A crash before the disposition
commits leaves **no partial frontier**: the whole map re-runs from its already-
committed per-item bodies and the frontier is decided once, cleanly.

`prune:` and `reduce:` compose: the survivors are the input to a `reduce:` fold —
search, then collapse the winners to one.

Out of scope: `prune:` on `parallel` — a `parallel` branch has no durable
per-branch status record, so a pruned branch could not survive resume safely;
`parallel` has no `prune:` surface in the format. Growable membership (a runtime
`enqueue` of new items) is also out of scope — the item set is fixed by `over:`
and stays static and digest-pinned.

## compose

    - compose:
        as: <name>                         # block-scoped container handle
        from: step.<id>.files.<name>       # generated Compose artifact
        service: <template>                # default service to exec into
        body: [<node>...]

Promotes a Compose file produced earlier in the run into an AWF-managed Compose
project for the duration of `body`. The `from:` value is a **static named
artifact reference** with the same rules as `input_files`: it must name a prior,
in-scope step's named `output_files` artifact and is rejected at validation if it
does not (**AWF3007**). The artifact bytes themselves are not known until run
time, so AWF validates those exact committed bytes just before promotion:
malformed YAML / Compose load failure (**AWF3004**), mutable or missing service
images (**AWF3003**), and file-following directives `include:`, `extends:`, and
`label_file:` (**AWF3005**) fail before Docker create.

`as:` creates a block-scoped container handle name. Steps inside `body` may use
`container: <as>` to exec into the rendered default `service`, or
`container: <as>:<service>` to override the service for that step. The default
service and any service override must name services present in the generated
Compose project; missing services fail the compose block before Docker create.
The scoped handle is valid only inside `body`; using it outside the block is a
normal unresolved container reference (**AWF1009**). `as:` must be static and may
not collide with top-level containers or outer scoped handles (**AWF1038**).

The runtime brings the generated project up with the backend's normal Compose
readiness (`up --wait` on Docker), runs `body`, then tears the project down on
block exit. Promotion and readiness failures are ordinary workflow failures: wrap
the block in `try`/`catch` to emit an application-level result such as
`cannot_build_lab`. `finally` keeps its existing cleanup meaning; container and
Compose teardown remain AWF-owned.

On resume, the producer step is replayed from its committed journal entry, the
same artifact bytes are promoted again, completed body steps are skipped, and the
unfinished body frontier continues. Project names are derived from the run id and
the block's runtime path plus `as:` (including map item path segments when
nested in a `map`), so two blocks using the same `as:` in different scopes do
not collide at the Docker project layer. The native backend
does not advertise runtime-Compose support; `awf run --backend native` rejects a
workflow containing `compose:` before execution.

## awf/llm

`awf/llm` is the built-in containerless adapter for direct LLM calls (OpenAI-compatible endpoints
and the native Gemini REST API). It requires no container. The relevant `with:` keys for agent
steps and `react:` are:

    with:
      provider: gemini | openai | openai_compat  # required; selects the call path
      model: <model-id>                           # required
      base_url: <url>                             # optional; override endpoint
      api_key_env: <env-var>                      # optional; name of the API-key env var
      system_prompt: <string>                     # optional; a system / developer message
      prompt: <template>                          # required for agent steps; omit in react:
      gemini_cache:                               # optional; Gemini explicit CachedContent
        mode: explicit                            # only non-trivial value; "off" or omitted = disabled
        ttl: "600s"                               # optional; default 3600s — TTL for the cache object

**gemini_cache**
:   Optional. Requires `provider: gemini` and at least one `input_files` document on the step.
    When `mode: explicit`, the adapter uploads the document(s) once per `awf run` as a
    Gemini `CachedContent` object and references it by name on every `:generateContent` call,
    omitting the inline document and `systemInstruction` (both are baked into the cache).
    Native structured output (`output_schema`) and thread continuation (`continues:`) work unchanged.

    **Reuse scope.** Cache names are in-process only: keyed by `model + system_prompt + document
    bytes` (content-addressed), alive for one `awf run`, and never journaled or resumed. On resume,
    the document is re-uploaded and a fresh cache object is created.

    **system_prompt and cross-step sharing.** Because `system_prompt` is baked into the cache and
    forms part of the key, a gate whose `generate` and `evaluate` steps set *different*
    `system_prompt` values get *per-role* caches (the document is still cached once per role, so
    read savings still apply). To share a single document cache across both roles — one upload for
    the entire gate — leave `system_prompt` empty and embed role-specific instructions in the user
    `prompt` instead.

    **Cost.** Cache-read savings (`cachedContentTokenCount`) are reflected in AWF's derived cost.
    Gemini also bills for cache **storage** (per-token-per-hour for the full TTL, regardless of
    how many reads occur); AWF does not include that storage cost in its pricing breakdown — verify
    totals on the Google Cloud console. Set `ttl` to approximately the gate's expected wall-clock
    duration to avoid paying for unused cache lifetime.

    **Minimum size.** The document must exceed the model's minimum cacheable token count (~2048 for
    Gemini 2.5, ~4096 for 3.x). A smaller document causes `CachedContent` creation to fail with a
    400 permanent error; retry will not help — use a larger document or disable explicit caching.

    When `gemini_cache` is absent or `mode: off`, the document is sent inline on every call and
    Gemini's free *implicit* prefix caching may apply (best-effort, document-first order assumed).

## react

`react:` is a control node that runs a model + tools loop on the `awf/llm` path. It is the only
node that drives an engine-mediated tool loop; CLI agents (claude/codex/droid/goose) stay
black-box and cannot use it.

    - react:
        id: <node-id>               # required — addresses the node's output ({{ <id>.* }} / awf outputs --step <id>)
        with:                       # the awf/llm config, minus `prompt`
          uses: awf/llm
          model: <model>
          base_url: <url>
          system_prompt: <string>
        prompt: <template>          # required — the initial user turn
        tools: [<name>, ...]        # required, >=1 — subset of top-level tools: this step offers
        max_turns: <int>            # optional, default 8 — one turn = one model call (+ its tools)
        output_schema: <JSON Schema># optional — the typed final answer (enforced on natural stop only)

Each turn: the model is called with `tools` attached; if it requests tools, each is dispatched as
its `impl` step and the results are fed back; the loop repeats until the model stops or `max_turns`
is reached.

**Output contract.** The node's output always carries a reserved top-level `stop_reason` sibling
(`"stop"` | `"max_turns"`). Reference it as `{{ <id>.stop_reason }}` and the answer fields as
`{{ <id>.<field> }}`. On natural stop (`stop_reason: "stop"`) the answer is validated against
`output_schema` if declared. On `max_turns` the loop stops without dispatching the final round's
tools, `output_schema` is **not** enforced, and the output is `{ stop_reason: "max_turns", text: <last assistant text> }`. `output_schema` may not declare a property named `stop_reason`.

**Tool failures the model sees** (not step failures): a tool's non-zero exit feeds its exit code +
stdout back as the tool result; an unknown/hallucinated tool name feeds back an error. The
model-facing tool result is capped (large output is truncated, the full output kept in the run's
artifacts); non-UTF-8 output is referenced by size, not inlined.

**Scope.** `react:` requires an `awf/llm` (containerless, threaded) adapter; v1 is OpenAI-compat
only (a `structured_output: ollama_format` config is rejected). A top-level `react:` is referenceable
via `{{ <id>.* }}`; a `react:` nested in `loop`/`gate`/`map` is readable via `awf outputs --step`
only.

# OUTCOMES, RETRY, AND REPAIR

Step outcomes are *mechanical only* — quality is the gate's job, not an outcome
class. Every step ends as exactly one of:

**ok**
:   Ran cleanly (exit 0 / schema-valid / signal delivered). Not retryable.

**retryable_failure**
:   Transient: launch or transport error, `timeout`, a nonzero exit not declared
    permanent, or unparseable output. Retryable per policy.

**permanent_failure**
:   An agent refusal or policy block, or an exit code in
    `non_retryable_exit_codes`. Not retryable.

A fourth disposition exists only inside a pruned `map`. When a `map` declares
`prune:` (see *map*), an item the frontier discards is recorded as **`pruned`** —
neither `ok` nor a failure. A `pruned` item does not count toward `min_success`,
does not raise a typed error, and does not trip an enclosing `try`/`catch`; it is
a deliberate, mechanical cancellation by the coordinator, not a quality judgment
(quality is still the gate's job). `pruned` is the *only* status outside `ok` /
`retryable_failure` / `permanent_failure`, and it is confined to the `prune:`
clause — every step outside a pruned `map` still ends as exactly one of the three
above.

Retry — transient recovery, applied to every step by default:

    retry: { attempts: 3, backoff: exp, initial: 1s, max: 60s, non_retryable_exit_codes: [78] }

Repair — quality recovery — is the gate, a separate axis. A step can be retried
for flakiness *and* sit inside a gate that repairs it for quality; the two
compose. Retry re-runs an *identical* step after a transient fault, with no
feedback; repair regenerates against the judge's critique.

Propagation: a step that exhausts retries as a failure, or a gate that exhausts
attempts as `rejected`, raises a typed error to the nearest enclosing
`try`/`catch` (a `catch` may match the kind), cancelling parallel siblings on the
way; uncaught, it halts the run.

# TEMPLATING AND TYPED OUTPUTS

Templating does exactly two things; it is not a programming language.

Substitution fills references before a command runs:

    {{ run.id }}   {{ input.<field> }}
    {{ step.<id>.exit_code }}   {{ step.<id>.stdout }}   {{ step.<id>.<field> }}
    {{ evaluate.<field> }}      # inside a gate's generate: the latest verdict; empty on attempt 1

`exit_code` and `stdout` are strings; `<field>` references resolve to *typed
values* from the producer's `output_schema`, never raw text. `evaluate.<field>`
is the typed output of the enclosing gate's `evaluate` block, supplied
automatically on repair attempts. Values over the runtime's inline limit are
rejected at resolution (pass large data as an `output_files` artifact).

`{{` is reserved in every templated field. To write a literal `{{` — a prompt
that teaches templating, or text that merely contains the sequence — escape it
as `\{{`: the backslash is consumed and a literal `{{` is emitted, and the
region is **not** parsed as a reference. This is the only escape; `\` is special
only immediately before `{{` and is otherwise literal (so `\\{{` yields a literal
`\` followed by a literal `{{`). An unescaped `{{` always begins a reference and
must close with `}}`.

A `step.<id>.<field>` reference resolves the named step's typed output wherever
the step sits, subject to scope. `try` and `parallel` introduce no multiplicity,
so a step inside them is referenceable from anywhere, exactly like a top-level
step. A step inside a `loop` resolves to its most recent iteration (above). A
step inside a `gate` or a `map` is referenceable *only from within the same scope
instance* — the same gate attempt, or the same map item — because from outside
there is no single attempt or item to resolve to; a cross-scope reference is
rejected at validation. Read a gate's product through `{{ evaluate.<field> }}`.

A `step.<map-id>` reference to a map aggregate, evaluated from *outside* that
map, reads the map product. When the map has `reduce:`, `step.<map-id>.<field>`
binds the reducer's typed output fields and `step.<map-id>.files.<name>` binds
the reducer's named artifacts. Body-step references remain valid inside the
reducer, so the reducer can read the branch outputs it is collapsing.

When the map has no `reduce:`, the aggregate product may expose a compact array
of the final body step's typed output. A legacy `step.<id>` reference to a step
inside a `map` body, evaluated from outside that map, reads that step's per-item
outputs in the same compact aggregate form. The runtime lifts the typed output to
an array, in item-index order:

- `step.<id>` resolves to the array of that step's whole typed outputs — one
  element per item, each element the full `output_schema` object.
- `step.<id>.<field>` resolves to the array of just that `<field>`.

The array is **compact**: it holds one element only for items where the step
actually committed. Items the step never ran for — `if`-filtered, `skip`ped, or
failed before it ran — are simply absent, so the array's length is at most the
worklist size *N* (and an empty `map`, `over: []`, aggregates to `[]`). There are
no nulls; to carry the original-item position through a compacted aggregate, have
the step write it into its own typed output under a field name *other* than
`index` (in a map body, `{{ <as>.index }}` is the reserved item-position
accessor, so an output field literally named `index` cannot be read back).

Because substitution renders only scalars, an aggregate array cannot fill a `{{ }}`
slot in a shell host, a prompt, or a condition — that is rejected at validation
(**AWF5004**). A map without `reduce:` may expose the compact array of the final
body step's typed output for another `map`'s `over:`, the array-native sink: map
A produces N typed outputs, map B fans out over them. This is the map→map
chaining primitive (see EXAMPLE).

Aggregation in v1 is defined only for the single-map case: the producing step is
enclosed by exactly one `map`, with no `gate` between them and no `loop`
multiplying the path. A producer nested in two or more maps, or wrapped in a
gate, is still rejected as not-yet-defined (**AWF5002**).

Substitution into a shell host (`run:`, `idempotency_key:`) is verbatim and
pre-shell: AWF inserts the resolved value as-is and does **not** quote or escape
it. Use those slots for trusted scalars — ids, counts, enums, flags, `input`
fields. Do not interpolate free-text *agent* output into a shell host; backticks
or `$(...)` in agent-written text are then executed by the shell. Route free-text
or untrusted data through an `output_files` artifact and read it from a file
inside the command. (Composites are rejected mechanically; a free-text `string`
passes both validation and resolution, so keeping it out of shell hosts is the
author's contract. Agent `with:` prompts are not shell hosts.)

Condition evaluation, for `if.cond`, `loop.until`, and `gate.until`, is a bounded
evaluator over references, literals, comparisons (`== != < <= > >=`), and boolean
operators (`&& || !`). No arithmetic, calls, or loops.

Schemas (`input` and every `output_schema`) are JSON Schema 2020-12. For agent
outputs AWF defines a deliberately conservative cross-backend floor: objects with
all properties `required` and `additionalProperties: false`, scalar types,
`enum`, arrays, and bounded nesting; no `oneOf`, `not`, or numeric/string-length
range keywords (`minimum`/`maximum`/`minLength`/`pattern`/...), which no major
constrained-decoding backend enforces. The all-properties-`required` and
`additionalProperties: false` rules are required. Schemas outside the floor are
validated post-hoc, not constraint-enforced.

## Workflow Exports

Imported workflows return explicit typed outputs and named artifact aliases.
`output_schema` declares the exported typed object, `outputs:` binds each field
to an in-scope typed reference, and top-level `output_files:` maps public artifact
names to named artifacts produced inside the workflow.

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

The evaluated `outputs:` object must satisfy `output_schema`. Missing required
fields, extra fields when the schema disallows them, and type/schema mismatches
follow normal JSON Schema validation. Each workflow-level `output_files:` alias
must resolve to an in-scope named artifact (`step.<id>.files.<name>` or another
valid aggregate artifact reference); aliases cannot expose bare capture-only
artifacts or arbitrary paths.

# CHECKPOINTING AND RESUME

AWF persists progress so a re-run does not redo expensive stages — *not* to
provide distributed exactly-once durability. The durable unit is a
content-addressed artifact, never a live container's process state.

**Commit**
:   The only way a step is recorded complete: its typed outputs and any declared
    `output_files` are written to the artifact store, then a journal entry
    pointing at them is appended (content-address-then-pointer-swap, so a "done"
    record never references a missing artifact). For a `snapshot: workspace`
    container, a copy-on-write FS diff is captured in the same commit.

**Resume**
:   Folds the journal, then: recreates each live container from its image/Compose
    recipe (readiness re-runs via the entrypoint or `up --wait`; a
    `snapshot: workspace` container restores its last committed diff instead);
    *replays committed steps from the journal* — recorded outputs and
    `output_files` are reused, not recomputed; and re-executes only the
    *uncommitted frontier* — the in-flight step on each active branch. A
    deterministic (code) replay is exact; an interrupted agent step may differ on
    re-run, which is correct — its work was never committed.

**Pinning**
:   The workflow definition (by digest, including the resolved import graph,
    assets, and any Compose files) and each resolved agent-runtime identity and
    version are recorded at run start. Resume against a changed definition or
    runtime is a hard error: a changed definition shifts step addressing; a
    changed runtime changes behavior. Imported workflow drift is definition
    drift and hard-errors on resume.
    A `map`'s runtime-resolved element image is recorded not at run start but at
    each element's first boot (the earliest point it is known) and folded into
    that element's journal entry; the definition still pins the template *text*,
    so a changed workflow is still a hard error, while the resolved per-element
    digest is run state.

**Pause**
:   Halts dispatch at the next commit boundary, marks the run `paused`
    (non-terminal, resumable), and — unlike cancellation — leaves containers up
    for inspection. This is the breakpoint mechanism; there is no breakpoint node.

**Cancellation**
:   Interrupts in-flight steps, runs enclosing `finally` blocks, tears down
    containers/projects, and marks the run terminal (not resumable).

Step addressing, used for resume and traces, names step nodes by `id`, call
children by `<call-id>.workflow.<child-path>`, and control nodes positionally,
joined from the root: `try[0].catch`, `if[1].then`, `loop[0].body.iter-3`,
`gate[0].attempt-2.generate`, `parallel[2]`, `map[0].item-3`,
`recon_result.workflow.extract`. The runtime computes every address, including
call paths, through the single `engine/path` function; implementations must not
construct journal keys or `awf.node.path` values ad hoc.

# EXTERNAL EFFECTS AND IDEMPOTENCY

A step with effects *outside* its container (open a PR, send mail, charge a card)
can be re-run by retry or resume. The mechanism is `idempotency_key`: a stable
template the external system uses to dedupe; the runtime passes the resolved key
to the step on every attempt. Cleanup is `try`/`finally`; there is no
compensation primitive.

AWF can only mediate effects it can see. Effects an agent performs *autonomously
through its own tools* (an `mcp://` call, a network `exec`) are at-least-once and
outside the guarantee. For exactly-once there, model the side-effecting action as
a code step (so the runtime mediates the key) or thread a key into the agent via
`with:`.

# EXAMPLE

A CVE triage -> exploit -> PR pipeline. A multi-service lab stays up across
stages; the gate wraps the exploit in an independent validator with a
benign-payload oracle and repairs on failure; a hard reject is caught and turned
into a clean exit; a human approves before the PR; the PR is idempotent; the lab
is torn down with the run.

    workflow: cve-pipeline
    version: 1
    input:
      type: object
      required: [cve_id]
      properties: { cve_id: { type: string } }

    containers:
      lab:                                  # services: vulnerable, patched, db, capture
        compose: ./lab/compose.yml          # readiness via compose healthchecks + up --wait
        service: vulnerable

    graph:
      - id: triage
        container: lab
        uses: anthropic/claude-code
        with: { skill: cve-triage, cve: "{{ input.cve_id }}" }
        output_schema:
          type: object
          additionalProperties: false
          required: [web_exploitable, has_source]
          properties:
            web_exploitable: { type: boolean }
            has_source:      { type: boolean }

      # skip-and-exit guards keep the main line flat
      - if:
          cond: "{{ !(step.triage.web_exploitable && step.triage.has_source) }}"
          then: [ - skip: "not web-exploitable or no source" ]

      - try:
          do:
            - gate:                          # exploit, judged independently, repaired on failure
                generate:
                  - id: exploit              # repair attempts auto-receive the prior verdict
                    container: lab
                    uses: anthropic/claude-code
                    with: { skill: cve-exploit }
                evaluate:                     # multi-step independent judge
                  - id: run_oracle            # deterministic: exploit on vuln + patched + benign payload
                    container: lab
                    run: ./validate.sh "{{ input.cve_id }}"
                    output_files: [/out/oracle.har]
                    output_schema:
                      type: object
                      additionalProperties: false
                      required: [verified, detections, false_positives, feedback]
                      properties:
                        verified:        { type: boolean }
                        detections:      { type: integer }
                        false_positives: { type: integer }
                        feedback:        { type: string }
                until: "{{ evaluate.verified && evaluate.detections == 5 && evaluate.false_positives == 0 }}"
                max_attempts: 5

            - id: approve                    # human gate before an external effect
              await: human_review
              timeout: 24h
              output_schema:
                type: object
                required: [approved]
                properties: { approved: { type: boolean } }

            - if:
                cond: "{{ !step.approve.approved }}"
                then: [ - skip: "human rejected the exploit" ]

            - id: open_pr
              container: lab
              run: ./open-pr.sh "{{ input.cve_id }}"
              idempotency_key: "{{ input.cve_id }}:pr"

          catch:                             # gate exhausted max_attempts -> exit cleanly
            - skip: "no validated exploit after repair budget"

A map->map chain. Map A scans each input host and produces a typed `finding`;
map B fans out over A's aggregated findings — `over: "{{ step.scan }}"` is the
index-ordered array of A's per-item `scan` outputs, each element bound as `f`.

    workflow: scan-then-verify
    version: 1
    input:
      type: object
      required: [hosts]
      properties:
        hosts: { type: array, items: { type: string } }

    containers:
      lab:
        image: oci://example.com/scanner@sha256:0000000000000000000000000000000000000000000000000000000000000000

    graph:
      - map:                                 # map A: one scan per host
          over: "{{ input.hosts }}"
          as: h
          container: lab
          concurrency: 4
          body:
            - id: scan
              container: lab
              run: ./scan.sh "{{ h }}"
              output_schema:
                type: object
                additionalProperties: false
                required: [finding]
                properties:
                  finding: { type: string }

      - map:                                 # map B: one verify per aggregated finding
          over: "{{ step.scan }}"            # []scan-output, in item-index order
          as: f
          container: lab
          concurrency: 4
          body:
            - id: verify
              container: lab
              run: ./verify.sh "{{ f.finding }}"

# SEE ALSO

**awf**(1), and the project README for an introduction to AWF.
