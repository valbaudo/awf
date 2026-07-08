AWF 1 "May 2026" "AWF" "AWF Manual"
===================================

# NAME

awf - orchestrate black-box agent CLIs and shell commands as gated, checkpointed workflows

# SYNOPSIS

**awf** _command_ [_arguments_]

**awf** **validate** [**-o**|**--output** _text_|_json_] _path_

**awf** **run** [**--input** _json_|**--input-file** _path_] [**--input-files** _name=path_]... [**--run-id** _id_] [**--state-dir** _dir_] [**--backend** _auto_|_fake_|_docker_|_native_] [**--agent-env** _csv_] _path_

**awf** **resume** [**--state-dir** _dir_] [**--from** _step_] _run-id_ _path_

**awf** **signal** [**--payload** _json_] [**--state-dir** _dir_] _run-id_ _name_

**awf** **pause** [**--reason** _text_] [**--state-dir** _dir_] _run-id_

**awf** **cancel** [**--reason** _text_] [**--state-dir** _dir_] _run-id_

**awf** **ls** [**--state-dir** _dir_] [**--output** _text_|_json_]

**awf** **inspect** _run-id_ [**--state-dir** _dir_] [**--fold** _statuses_] [**--depth** _n_] [**--output** _text_|_json_] [**--tokens**]

**awf** **trace** _run-id_ [**--state-dir** _dir_] [**--otlp** _endpoint_] [**--capture-content**] [**--output** _otel_|_json_]

**awf** **outputs** _run-id_ [**--workflow** _path_|**--step** _node-id_] [**--state-dir** _dir_]

**awf** **graph** _path_ [**--run** _id_] [**--state-dir** _dir_] [**--output** _json_]

**awf** **ui** _path_ [**--state-dir** _dir_] [**--port** _n_] [**--open**]

**awf** **version** [**-o**|**--output** _text_|_json_]

**awf** [**--version**]

**awf** **help**

# DESCRIPTION

**awf** is a runtime for *agentic pipelines*: author-defined control flow whose
steps are black-box agent CLIs (such as Claude Code) and shell commands, run
against long-lived containers — a digest-pinned image or a Compose project that
is created once and kept up for the life of the run.

Its defining feature is the **gate**: an *independent* judge that evaluates each
stage and drives a bounded repair loop, so an agent never marks its own
homework. This is "TDD for agentic workflows" — you write the acceptance check,
the runtime runs it, and the stage advances only when the check passes.
Expensive, nondeterministic work survives crashes and context resets through
content-addressed checkpoint/resume: a committed step's outputs are replayed,
never recomputed.

A workflow is a single YAML document; its format is documented in
**awf-workflow**(5). **awf** loads that document, validates it, and executes its
graph — gating stages and checkpointing progress under _state-dir_ (default
`./.awf`).

# OPTIONS

Options may appear before or after the positional operands of a command, in any
order (`awf run wf.yaml --backend docker` and `awf run --backend docker wf.yaml`
are equivalent). A bare `--` terminates option parsing: every argument after it is
treated as an operand even if it begins with `-`. Long options are written
`--name` (a single dash such as `-name` is not accepted); single-character
shorthands, where defined, use one dash (`-h`).

# COMMANDS

## awf validate _path_

Parse _path_, validate it against the AWF format (**awf-workflow**(5)), and print
the workflow's content-addressed definition digest. Validation collects *all*
diagnostics in one pass; any error-severity diagnostic yields a non-zero exit
(see **EXIT STATUS**). This command performs no container, network, or agent I/O.

**-o**, **--output** _text_|_json_
:   Output format. _text_ (default) is human-readable. _json_ emits a
    machine-readable object carrying the digest and each diagnostic's stable
    namespaced `code` (`AWF1xxx` structural, `AWF2xxx` schema-floor, `AWF3xxx`
    loader) — an API for tooling, never renumbered. **--format** is a
    deprecated alias for **--output** (it still works but prints a notice to
    standard error).

## awf run _path_

Mint a run id, validate _path_, bring each declared container to readiness from
its image or Compose recipe, and execute the graph. Code-step output streams to
standard output as a live tap, while agent-step progress — assistant text,
reasoning, tool calls and results — streams to standard error (plain when piped
or under `NO_COLOR`). The final line on standard output reports the run id and
terminal outcome (for example `run 1a2b3c4d: ok`). Run state is written under
_state-dir_ — a per-run journal and a shared content-addressed blob store (see
**FILES**).

**--input** _json_
:   Run parameters as a JSON object, validated against the workflow's `input`
    schema. It is an error to pass **--input** when the workflow declares no
    `input` schema.

**--input-file** _path_
:   Read the same run-input JSON from a file instead of inline; `--input-file -`
    reads it from standard input. Validated against the workflow's `input` schema
    exactly as **--input**, and mutually exclusive with it (supplying both is an
    error). Prefer this for input carrying secrets, which leak through the process
    table and shell history when passed inline via **--input**. The `-` form
    supplies the run INPUT only — reading the WORKFLOW itself from stdin
    (`awf run -`) is not supported, as the loader confines workflows to real files
    on disk.

**--input-files** _name=path_
:   Bind one top-level workflow input file. Repeatable — supply the flag once per
    name declared in the workflow's `input_files:` map (`--input-files
    document=doc.txt --input-files image=pic.png`). For back-compat a single
    occurrence may instead carry a comma-separated list (`--input-files
    a=x,b=y`); because that lone value is split on commas, a _path_ that itself
    contains a comma must be supplied via the repeated form (two or more flags),
    where values are taken literally. (A workflow that declares a *single*
    `input_files:` name whose path contains a comma cannot be supplied this way —
    the repeated form needs two or more distinct names — so give that file a
    comma-free path.) Each file's bytes are content-addressed on run start and
    resolvable inside the workflow as `input.files.<name>`. Every declared name
    must be supplied, every supplied name must be declared, and no name may be
    supplied more than once (a duplicate is a hard error, never last-wins); any
    mismatch is reported before any state is written. Not re-supplied on
    **awf resume** — the file refs are folded from the durable `run.started`
    journal entry.

**--run-id** _id_
:   Use _id_ as the run id instead of minting a fresh random one. A testing and
    scripting aid; run ids are otherwise unique and unpredictable.

**--state-dir** _dir_
:   Base directory for the `runs/` journals and the shared `blobs/` store
    (default `./.awf`).

**--backend** _auto_|_fake_|_docker_|_native_
:   Where steps execute (default _auto_). _docker_ runs them in real containers
    and Compose projects with full isolation; _native_ runs them as host
    processes with no container boundary, write-confined by an OS sandbox (see
    **CONTAINERS** in **awf-workflow**(5)); _fake_ is an in-memory backend for
    tests.
    _auto_ selects _native_ unless the workflow uses Docker-only features such as
    static image-backed containers, Compose-mode containers, or runtime map
    images, in which case it selects _docker_ for a pinned, reproducible
    baseline. **awf run** records the selected concrete backend in `run.started`;
    **awf resume** uses that recorded backend and does not re-run auto-selection.
    All three concrete backends — _docker_, _native_, and _fake_ — are resumable.

    When _auto_ selects _native_, **awf run** prints:

        awf run: auto-selected native backend (no Docker-only features). Resume restores snapshot: workspace workdirs but does not pin the host base environment; use --backend docker for a pinned baseline.

    An explicit **--backend native** runs static image-mode and
    `snapshot: workspace` workflows directly on the host, *ignoring* the declared
    container image — the image is not pulled and there is no container boundary,
    though each step is still write-confined by an OS sandbox (see **CONTAINERS**
    in **awf-workflow**(5)). When a workflow declares an image, native prints:

        awf run: --backend native ignores declared container image(s); steps run on the host.

    Explicit native still **rejects** Compose-mode containers, runtime Compose,
    and runtime map images — those have no host equivalent — with guidance to use
    **--backend docker**. _auto_ never selects native for any of those; it routes
    them (and image-mode and `snapshot: workspace`) to _docker_ instead.

**--agent-env** _csv_
:   Comma-separated allowlist of environment-variable *names* forwarded into
    agent runtime CLIs (default
    `ANTHROPIC_API_KEY,ANTHROPIC_AUTH_TOKEN,CLAUDE_CODE_OAUTH_TOKEN,FACTORY_API_KEY,GOOSE_PROVIDER,GOOSE_MODEL,OPENAI_API_KEY,CODEX_HOME,GEMINI_API_KEY`).
    The same allowlist applies to every registered adapter, including
    `uses: anthropic/claude-code`, `uses: factory/droid`, `uses: block/goose`,
    `uses: openai/codex`, and `uses: awf/llm`. **GEMINI_API_KEY** is the default
    `api_key_env` for an `awf/llm` step with `with: provider: gemini` (see
    **awf-workflow**(5)); it must appear on this allowlist (or the workflow
    `env:` field) or the step fails at launch with no credential.
    Names not on the list are not passed through. A workflow can extend this
    allowlist from inside its definition with the top-level `env:` field (see
    **awf-workflow**(5)); names declared there are forwarded in addition to this
    flag, on both **run** and **resume**. See **ENVIRONMENT**. Before dispatch,
    **awf run** also prints a non-fatal stderr warning when an agent step's adapter
    has *none* of its known credential environment variables set — an early hint
    that the step will otherwise fail at launch.

    The `uses: openai/codex-live` adapter is registered as a built-in live
    adapter. It uses Codex app-server sessions, the same runtime-resolution,
    version-pinning, environment allowlist, live event, **awf trace**, and UI
    projection paths as ordinary agent steps, and provider-owned transcripts
    stay outside AWF blobs. There is no separate **awf live** command.
    `uses: block/goose-live` remains reserved for a future ACP adapter.
    `uses: anthropic/claude-code-live` remains deferred until a PTY proof spike
    proves turn-boundary detection, permission handling, transcript
    correlation, prompt injection, and reconnect behavior.

## awf resume [--from _step_] _run-id_ _path_

Re-enter an interrupted run, or any run whose last outcome is not `ok`,
re-running the uncommitted frontier. **awf** folds
the run's journal, then checks the on-disk _path_ against the recorded digests
*and* every resolved agent-runtime version. A changed structural digest or
runtime version is a hard error, never silently adapted; an edit confined to step
bodies instead engages per-node reuse (below). It recreates containers from their recipe and continues
in a new epoch. Committed steps are replayed from the journal (their recorded
outputs reused, not recomputed); only the uncommitted frontier re-executes. The
backend is read back from the journal, so no **--backend** flag is given and
_auto_ is not re-evaluated on resume. Any run whose last
outcome is not `ok` (`permanent_failure`, `rejected`, `retryable_failure`, or
`cancelled`) is resumed with no flag; a one-line non-fatal note prints to
stderr because the uncommitted frontier — and its side effects — re-runs.
Pinning is relaxed only for an edit confined to step bodies: with an unchanged
structural digest (topology, env, containers, assets, agents, imports, Compose
files, node set), AWF reuses unchanged committed code steps and re-runs from the
first changed node — per-node verifying-trace reuse; agent, react, map, signal,
and call steps always re-run (see **awf-workflow**(5)). A changed structural
digest, runtime-version drift, an addressing shift, or a pre-per-node log is
still a hard error — use **resume --from** for the explicit fenced bypass. A run
that finished `ok` is a no-op.

**Native backend resume** mirrors Docker: committed steps are replayed from the
journal, `snapshot: workspace` workdirs are restored from their last committed
archive, and other containers are recreated fresh. Resume of a native run prints a
one-line caveat to stderr:

        awf resume: native backend — committed work is replayed and snapshot: workspace workdirs are restored, but the host base environment is not pinned; shell-step tooling runs against the current host.

Checkpoint integrity is preserved (committed replay plus the digest and
runtime-version pins above), but the host baseline is not pinned: native does
not pull or pin the declared image, so shell-step tooling runs against whatever
is on the current host. Use **--backend docker** for a fully reproducible
baseline.

**--state-dir** _dir_
:   Base directory holding the run (default `./.awf`).

**--from** _step_
:   Re-run from a committed node, or from the trailing **failed** node of a run
    that stopped on a failure (named by a runtime-path prefix, e.g. a top-level
    step id or `parallel[0].<step>`; the failed frontier node may be named by its
    exact path or its bare trailing id). Invalidates that node plus every
    node after its top-level ancestor and re-runs them against the *current*
    definition; everything before is replayed. **Bypasses pinning** (digest +
    runtime drift) — a debug-mode exception; the operator owns the correctness
    of what is replayed, and the
    re-run set (incl. its at-least-once side effects) is printed before running.
    v1 supports a top-level node or a parallel branch; a node inside a
    call/loop/gate/map-body is refused.

## awf signal _run-id_ _name_

Deliver a named signal, with an optional typed payload, to a run's `await` step.
Signals are durably journaled on receipt and buffered per name, so a signal sent
*before* its `await` step is reached is consumed when the step runs — it is
never lost across a restart.

**--payload** _json_
:   Typed payload, validated against the `await` step's `output_schema` when one
    is declared.

**--state-dir** _dir_
:   Base directory holding the run (default `./.awf`).

## awf pause _run-id_

Request a pause: **awf** halts dispatch at the next commit boundary and marks the
run `paused` — a non-terminal, resumable state. Unlike **cancel**, containers are
left *up*, so an operator can inspect the live workspace, the committed
artifacts, and the trace before running **resume**. This is the breakpoint
mechanism; there is no breakpoint node in the format itself.

**--reason** _text_
:   Operator note recorded in the journal.

**--state-dir** _dir_
:   Base directory holding the run (default `./.awf`).

A **--before** _node-path_ flag is reserved for stopping when execution reaches a
specific node, but is not yet implemented.

## awf cancel _run-id_

Terminally cancel a run: interrupt in-flight steps, run any enclosing `finally`
blocks, tear down all containers and Compose projects, and append a terminal
`cancelled` marker. A cancelled run can be re-entered with **resume** (its
uncommitted frontier re-runs); the terminal marker is not a resume barrier.

**--reason** _text_
:   Operator note recorded in the journal.

**--state-dir** _dir_
:   Base directory holding the run (default `./.awf`).

## awf ls

List the runs under _state-dir_/`runs/`, one per line, each with a status derived
by folding its journal: `running`, `paused`, `finished`, `failed`, `resumable`
(failed transiently; re-drivable with `awf resume`), `cancelled`, or `crashed`
(started, no terminal event, and no live process holding the run's lock). `failed`
covers permanent failures and rejections; `resumable` distinguishes runs that
ended with `retryable_failure` and can be continued. This command only reads
state; it executes nothing.

**--state-dir** _dir_
:   Base directory holding `runs/` and `blobs/` (default `./.awf`).

**-o**, **--output** _text_|_json_
:   Output format. _text_ (default) prints one run per line; _json_ emits a
    machine-readable array.

## awf inspect _run-id_

Render a run's addressing tree as a text tree, folded by status: `ok` subtrees
collapse, while `failed`, `rejected`, and `incomplete` subtrees expand, so a
failing branch stands out. Completed steps include their recorded elapsed time.
This reads the journal offline and executes nothing.

**--fold** _statuses_
:   Comma-separated list of node outcomes to collapse (default `ok`).

**--depth** _n_
:   Maximum tree depth to render (default: unlimited).

**-o**, **--output** _text_|_json_
:   _text_ (default) is the folded tree; _json_ emits the underlying span
    projection.

**--tokens**
:   Annotate each step with its input/output token counts, wherever the journal
    recorded them.

**--state-dir** _dir_
:   Base directory holding the run (default `./.awf`).

AWF does **not** offer Temporal-style deterministic replay: resume folds the
journal and re-runs only the uncommitted frontier, with no author-code
determinism contract.

## awf trace _run-id_

Project a run's journal into OpenTelemetry spans — one span per addressing-tree
node, mirroring the resume tree — and export them. Step spans include
`awf.node.duration_ms`, derived from the journal's `node.started` and terminal
event timestamps. By default the spans are written to standard output (a
zero-infrastructure local exporter); with **--otlp** they are sent to a collector
instead. This reads the journal offline and executes nothing.

**--otlp** _endpoint_
:   Export to an OTLP/HTTP collector at _host:port_ (plaintext) instead of
    standard output.

**--capture-content**
:   Also attach agent I/O (prompts, agent output, stdout) and typed-output
    values to the spans. Off by default. Combined with **--otlp** this transmits
    that content — including anything sensitive embedded in prompts — to the
    collector.

**-o**, **--output** _otel_|_json_
:   _otel_ (default) exports spans; _json_ dumps the span projection as JSON, for
    download or scripting, instead of exporting.

**--state-dir** _dir_
:   Base directory holding the run (default `./.awf`).

## awf outputs _run-id_

Read a completed run's typed outputs as JSON (pretty-printed). Read-only. Pass
exactly one of **--workflow** or **--step**; passing both or neither is a usage
error (exit 2).

**--workflow** _path_
:   Evaluate that workflow's top-level `outputs:` contract. The file is
    re-loaded and its content-addressed digest is checked against the run's
    pinned WorkflowDigest — a mismatch is refused (exit 2), exactly as
    **awf resume** refuses workflow drift. When the digest matches, the
    `outputs:` block is evaluated and emitted as a JSON object on standard
    output. If the workflow declares no `outputs:` block the command exits 2.

**--step** _path_
:   Emit one committed step's typed output, read directly from the log and blob
    store. No workflow file is needed. _path_ is a step's runtime address: a
    top-level node id, or a full nested runtime path exactly as it appears in
    the addressing tree — e.g. `gate[0].attempt-2.generate.extract`,
    `map[0].item-3.scan`, `loop[0].body.iter-3.summarize`. Unlike a
    `{{ step.<id> }}` template reference (which obeys scope multiplicity),
    **--step** performs no instance resolution: the caller names the
    attempt/item/iteration. A path that names no committed step — including a
    step under a non-taken `if` branch, or a path missing its
    attempt-/item-/iter- suffix — is a read failure (exit 1), not a usage
    error. Map aggregates and sub-workflow results are read via **--workflow**.

**--state-dir** _dir_
:   Base directory holding `runs/` and `blobs/` (default `./.awf`).

Exit codes for **awf outputs** differ from the global convention: the exit
reflects the *read*, not the run's outcome.

**0**
:   Output emitted successfully. The referenced step or workflow outputs were
    committed and could be produced.

**1**
:   The output could not be produced: the referenced step did not commit a
    typed output, or the workflow fails validation.

**2**
:   Bad invocation: both or neither of **--workflow**/**--step** given; a
    digest mismatch between the supplied file and the run's pinned
    WorkflowDigest; run not found; or no `outputs:` block declared.

Note: a workflow whose `outputs:` binds a step inside a transparent
conditional scope (an `if` branch or `loop` body) produces an **awf
validate** warning — the output may not be producible if that branch was
not taken. When an `if` branch was not taken, the bound output key is
**omitted** from the emitted JSON and the command exits 0; omission is not
an error. If `output_schema` marks the omitted field `required`, the
schema check then fails and the command exits 1 instead. Binding a gate-
or map-internal step is a hard validation error (**awf validate** rejects
it as exit 1). For the full set of binding rules — every valid reference
form and every rejection with its code — see the *Output binding — what
binds and what doesn't* section of **awf-workflow**(5). Use **awf ls** or
check the run's terminal status to determine whether the run itself
succeeded before reading outputs.

## awf graph _path_

Project a workflow into a node/edge graph and print it as JSON — the contract a
visual graph tool consumes. Edges are `control` (execution order) and `data`
(producer → consumer, derived from `{{ … }}` references). Without **--run** it
emits the static template graph; with **--run** _id_ it adds runtime instance
nodes (one per map item, gate attempt, and loop iteration, plus their children,
each `node_class: instance` pointing at its template via `instance_of`) and a
`run_overlay` mapping each node path to its state (`running`, `completed`,
`failed`, or `skipped`). Reads the workflow (and, with **--run**, the journal)
offline and executes nothing.

**--run** _id_
:   Overlay state from this run. Run ids are explicit (there is no `latest`); list
    them with **awf ls**. Omit for the static graph.

**--state-dir** _dir_
:   Base directory holding runs (default `./.awf`).

**-o**, **--output** _json_
:   Output format (only `json` today).

## awf ui _path_

Serve a local, read-only web UI that renders the workflow graph and overlays run
state. Binds **127.0.0.1** only — no authentication, no remote exposure — on an
ephemeral port unless **--port** is given, prints the URL, and serves until
interrupted. Open the URL and pick a run: its node states stream live (a node
lights up as it starts, completes, fails, or is skipped) over Server-Sent Events,
so a running workflow updates in place with no reload; **Refresh** forces a re-read.
The run list distinguishes running from crashed runs via the run-liveness lock. The
graph is rendered from the same projection as **awf graph**.

The list is scoped by the workflow's **workflow:** identifier, not the file's
content digest, so every run of the workflow stays listed even after you edit the
file. A run that executed against a different version of the file is flagged
*other version* in the picker. Each run is rendered against the definition it
actually ran — snapshotted at run start — so its graph reflects the structure that
executed, not the file currently on disk. A run recorded before definition
snapshots existed instead renders against the loaded file; a run from before the
workflow carried a recorded identifier is listed only when its digest matches the
loaded file. The UI is read-only and never affects resume or pinning, which always
re-check the live file.

**--state-dir** _dir_
:   Base directory holding runs (default `./.awf`).

**--port** _n_
:   Port to bind on 127.0.0.1 (default 0, an ephemeral port).

**--open**
:   Open the printed URL in the default browser (best-effort).

## awf version

Print the build identity on one line and exit `0`:

```
awf <version> (commit <12-char-sha>[+dirty], built <vcs.time>, <go-version>)
```

_version_ is `(devel)` for an ordinary build, or the tag baked in at release time via
`-ldflags "-X github.com/valbaudo/awf/cli.version=<tag>"`. The commit, build time, and dirty
flag come from the VCS metadata Go records in the binary; a `go run` build with no such
metadata collapses to `awf <version> (commit unknown, <go-version>)` rather than erroring.

**-o**, **--output** _text_|_json_
:   Output format (default `text`). `json` emits
    `{"version","commit","dirty","build_time","go_version"}` — the full untruncated commit, and
    empty strings where a field is unknown.

The top-level **awf --version** flag prints the same one-line text form.

## awf help

Print usage and exit. **-h** and **--help** are accepted as aliases.

# EXIT STATUS

**0**
:   Success — validation produced no error-severity diagnostics, or a run
    terminated `ok`.

**1**
:   `validate` found one or more error-severity diagnostics, or `run` completed
    but the run terminated as a failure (`retryable_failure`, `permanent_failure`,
    or `rejected` — an uncaught gate that exhausted its repair budget).

**2**
:   Usage error — bad arguments, an unreadable file, or a loader-stage failure
    (parse error, path escape, or a missing Compose file). Also covers
    precondition refusals on input the user controls: a run-id collision, a
    workflow digest mismatch, an agent-runtime pin drift, a terminal-state
    resume refusal, or a backend capability mismatch.

**3**
:   Environment/setup failure that AWF owns — not your input: opening the blob
    store, constructing the container backend or daemon client (e.g. the Docker
    daemon is down), creating or restoring a container, the run-dir or log-file
    I/O, opening the live-home, or acquiring the run-lock (including a run already
    active in another process). The split is "whose artifact failed": code `2`
    means your input is wrong; code `3` means AWF's environment is broken, so CI
    can retry a transient infra failure without masking a real usage error.

# ENVIRONMENT

**AWF_STATE_DIR**
:   Default base directory for `runs/` and `blobs/` when a command is invoked
    without **--state-dir**. The precedence is explicit **--state-dir** >
    **AWF_STATE_DIR** > `./.awf`, so a flag always wins and the env var only
    supplies the default. Honored by every subcommand that takes **--state-dir**
    (run, resume, signal, pause, cancel, ls, inspect, trace, outputs, graph, ui).

**AWF_STAGING_ROOT**
:   Set by the engine (not by the operator) inside a `reduce:` step's container so
    a `run:` reducer can locate the staged per-item manifests and branch
    artifacts. Backend-dependent: `/work/.awf` on _docker_, `.awf`
    (workdir-relative) on _native_. Reference it as `$AWF_STAGING_ROOT` rather than
    a literal path to stay portable across backends (see **awf-workflow**(5)).

**ANTHROPIC_API_KEY**, **ANTHROPIC_AUTH_TOKEN**, **CLAUDE_CODE_OAUTH_TOKEN**
:   Authentication for the `anthropic/claude-code` agent runtime. **awf** does not
    read these itself; it forwards those named in **--agent-env** (all three by
    default) into the agent invocation. The runtime defaults to a minimal *bare*
    mode that accepts only `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`; to
    authenticate with a subscription `CLAUDE_CODE_OAUTH_TOKEN` the agent step must
    set `bare: false`, otherwise the token is ignored and config validation fails.

**FACTORY_API_KEY** (factory/droid)
:   API key for the `factory/droid` agent runtime (`fk-…`). **awf** does not read
    this itself; it forwards the value into the droid invocation when the name
    appears in **--agent-env** (included in the default allowlist). Required for
    `uses: factory/droid`; absent key is a hard error at run start — **unless** the
    step runs in BYOK mode (see `with: base_url` below), in which case the named
    `api_key_env` var carries auth and **FACTORY_API_KEY** is neither required nor
    forwarded.

    **Bring-your-own-key (BYOK).** A droid step can target any custom
    OpenAI-compatible (or Anthropic) endpoint instead of Factory's gateway. Setting
    `with: base_url` enables BYOK: the adapter writes a one-entry `customModels`
    settings file into the container and references the model as `custom:<model>`.
    The literal API key never enters the workflow — it is read from a named host
    env var at runtime. For example:

        uses: factory/droid
        with:
          base_url: https://litellm.internal/v1
          api_key_env: LITELLM_KEY      # forward via env: or --agent-env
          provider: generic-chat-completion-api
          model: claude-sonnet-4

    The relevant `with:` keys:

    **with: base_url** (optional)
    :   A custom OpenAI-compatible endpoint. When set, droid runs in BYOK mode
        against it and **FACTORY_API_KEY** is not required. When omitted, the step
        uses Factory's default gateway and needs **FACTORY_API_KEY**.

    **with: api_key_env** (required when `base_url` is set)
    :   Name of a host env var holding the API key. Forward it via **--agent-env**
        or the workflow `env:` field — the named var must be on the forwarded
        allowlist, or it is a permanent config error. The literal key never appears
        in the workflow; droid expands a `${NAME}` placeholder from its own process
        env at runtime.

    **with: provider** (optional, default `generic-chat-completion-api`)
    :   Selects the wire protocol the endpoint speaks. One of:
        `generic-chat-completion-api` (OpenAI Chat Completions — e.g.
        LiteLLM/OpenRouter/Ollama), `openai` (OpenAI Responses API), or `anthropic`
        (Anthropic Messages API). A mismatched provider fails the call.

    **with: tls_insecure** (optional, boolean)
    :   When `true`, skip TLS certificate verification for the droid process (sets
        `NODE_TLS_REJECT_UNAUTHORIZED=0`). Default `false`. Use only for internal or
        self-signed endpoints — disabling verification exposes the connection to
        interception. Prefer the secure alternative: trust the gateway's CA via
        `NODE_EXTRA_CA_CERTS` (or `NODE_USE_SYSTEM_CA=1`), forwarded with the
        workflow `env:` field on native or baked into the image on docker. (droid is
        Bun-compiled, so CA-bundle support can be version-dependent.)

    `model` stays a plain identifier in both modes; it must be a non-empty,
    whitespace-free string.

**GOOSE_PROVIDER**, **GOOSE_MODEL**
:   Select the provider and model for the `block/goose` agent runtime. **awf**
    forwards those named in **--agent-env** (both in the default allowlist) into the
    `goose run` invocation. Auth is *provider-conditional*: when **GOOSE_PROVIDER**
    is set, an `anthropic` provider requires **ANTHROPIC_API_KEY** and an `openai`
    provider requires **OPENAI_API_KEY** on the allowlist (a missing key is a config
    error at run start). The default `claude-code` provider needs no awf-supplied
    key; its authentication is handled by goose's configured provider inside the
    image (for `claude-code`, an authenticated `claude` CLI). When **GOOSE_PROVIDER**
    is unset, awf does not gate the key — goose selects the provider from its own
    config inside the image.

**OPENAI_API_KEY**, **CODEX_HOME**
:   Authentication for the `openai/codex` agent runtime. **awf** does not read
    these itself; it forwards those named in **--agent-env** (both included in the
    default allowlist) into each `codex exec` invocation. Codex supports two auth
    modes: an `OPENAI_API_KEY` env var, or ChatGPT-OAuth via an `auth.json`
    provisioned into the runner image under `CODEX_HOME` (default `~/.codex`). The
    adapter cannot validate auth statically — a missing or invalid credential
    surfaces as a failed run (a `turn.failed` event with the API error message).

    The adapter always passes **--ephemeral** (no session persistence, preserving
    gate independence) and **--skip-git-repo-check**. By default it also passes
    **--dangerously-bypass-approvals-and-sandbox**, treating the AWF container as
    the isolation boundary. A `sandbox:` `with:` key (`read-only` |
    `workspace-write` | `danger-full-access`) opts into codex's internal sandbox
    instead; the bypass flag is then absent.

    **OpenAI structured-output floor.** An `output_schema` for an `openai/codex`
    step must be `additionalProperties: false` with every property listed under
    `required` (recursively), or codex fails it permanently at runtime. **awf
    validate** emits **AWF2002** for under-constrained schemas today (a warning, not
    an error). Treat **AWF2002** as **blocking** for codex steps, or expect a hard
    `ErrInvalidConfig` (permanent failure) at launch time.

    **Tool/MCP and --output-schema caveat.** When codex runs with tools or MCP
    servers active (via the workspace `AGENTS.md` or codex config inside the
    container image), it can silently drop **--output-schema** and return a
    non-JSON final message. The adapter fails the step loudly
    (`ErrUnparseableOutput` → retryable → gate repair), never silently mis-binding.
    Keep codex steps' container workspaces free of MCP and tool config when relying
    on `output_schema`, or expect repair churn.

    **Streaming granularity.** Unlike `anthropic/claude-code` and `block/goose`,
    which stream the answer token-by-token, `openai/codex`'s live output is
    event-granular: tool calls and reasoning steps appear as they happen, but the
    final answer text arrives in one block when the message completes. `codex exec
    --json` emits no token deltas; its streaming JSON-RPC interface is not
    reachable through AWF's exec seam. The typed output is functionally identical;
    only the live terminal UX differs.

**OPENAI_API_KEY** (awf/llm)
:   API key for the `awf/llm` adapter — the first non-CLI, containerless
    adapter. **awf** does not read this itself; it forwards the name when it
    appears in **--agent-env** (included in the default allowlist, deduplicated
    with the goose/codex entries). For a local model such as Ollama, set
    `OPENAI_API_KEY` to any non-empty placeholder (e.g. `ollama`); the value is
    sent as an `Authorization: Bearer` header, which Ollama ignores.

    The `awf/llm` adapter issues a single streaming HTTP call against any
    OpenAI-compatible Chat Completions endpoint. It is containerless: no
    `container:` block is needed in the workflow. Config lives entirely in the
    `with:` map:

    **with: model** (required)
    :   Model identifier (e.g. `gpt-4o`, `llama3.1`, `mistral`). Passed
        verbatim to the endpoint.

    **with: prompt** (required)
    :   The user message. Template references (`{{ step.X.field }}`) are
        resolved before the call. The adapter prepends any gate feedback as a
        `<previous verdict>` block on repair attempts, and appends the
        `output_schema` directive when one is declared.

    **with: base_url** (optional)
    :   The base URL of the endpoint (default `https://api.openai.com/v1`).
        **Footgun:** omitting `base_url` silently routes the call to OpenAI
        with the configured key. Set it explicitly when targeting a local or
        private endpoint. For Ollama's OpenAI-compat path use
        `http://localhost:11434/v1` (or `http://host.docker.internal:11434/v1`
        from inside a container). For the Ollama native path use
        `http://localhost:11434` (or `http://host.docker.internal:11434`) and
        set `structured_output: ollama_format`. vLLM, llama.cpp, LM Studio, and
        LiteLLM/Bifrost gateways all work with the OpenAI-compat path.

    **with: api_key_env** (optional)
    :   Name of the env var holding the API key (default `OPENAI_API_KEY`). The
        named var must be present in **--agent-env**; an absent key is a
        permanent config error, and a present-but-invalid key (HTTP 401/403)
        fails **permanent** at call time rather than retrying. Quota or budget
        exhaustion (`insufficient_quota`, "budget exceeded") is likewise
        **permanent** — the step fails fast instead of burning the retry budget,
        while an ordinary rate-limit stays retryable. On a retryable fault the
        adapter forwards the provider's `Retry-After` / `retry-after-ms` and
        `x-should-retry` headers to the retry loop, so a rate-limited step waits
        the server's stated window rather than the default backoff curve.

    **with: system_prompt** (optional)
    :   Text prepended as a system message before the user prompt.

    **with: temperature** (optional)
    :   Sampling temperature (a number). Sent to the endpoint only when
        explicitly set. **Omit for reasoning models** (o1/o3/gpt-5 and
        derivatives) — they reject `temperature` with a 400 error (permanent
        failure). For local backends using `structured_output: ollama_format`,
        `temperature: 0` is recommended for reproducible grammar-constrained
        output.

    **with: max_tokens** (optional)
    :   Maximum tokens to generate (an integer). Maps to
        `max_completion_tokens` for OpenAI-compatible endpoints and to
        `options.num_predict` for the Ollama native path. Sent only when set.

    **with: structured_output** (optional)
    :   How the adapter signals the output schema to the model. One of:
        `response_format` (default) — uses the OpenAI `response_format`
        JSON-schema parameter; `ollama_format` — uses Ollama's native
        `/api/chat` `format` field (requires an Ollama-native `base_url` such
        as `http://host.docker.internal:11434`); `off` — sends no schema
        signal (the schema is still injected into the prompt, and the adapter
        parses the model's final message).

        **Strict-schema floor.** When `structured_output: response_format` is
        set against OpenAI, the schema must be `additionalProperties: false`
        with all properties listed under `required` (recursively), or OpenAI
        rejects it. **awf validate** emits **AWF2002** for under-constrained
        schemas (a warning today). Treat **AWF2002** as **blocking** for
        `awf/llm` steps using `response_format`.

    **with: tls_insecure** (optional, boolean)
    :   When `true`, skip TLS certificate verification for the endpoint.
        Default `false`. Use only for internal or self-signed endpoints —
        disabling verification exposes the connection to interception.

    **Network stance.** Proxies need no adapter config: Go's default HTTP
    transport honors `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY`
    automatically. Anthropic-direct (Claude API) requires a gateway that
    presents an OpenAI-compatible interface in v1.

    **Streaming.** The adapter always streams token-by-token deltas (one event
    per chunk). Backends without streaming support are unsupported in v1.

**GEMINI_API_KEY** (awf/llm)
:   API key for an `awf/llm` step with `with: provider: gemini` — the default
    `api_key_env` when the step omits `api_key_env` (see **awf-workflow**(5)).
    **awf** does not read this itself; it forwards the name when it appears in
    **--agent-env** (included in the default allowlist). An `awf/llm` step
    using `provider: gemini` with no `GEMINI_API_KEY` set (and no overriding
    `with: api_key_env`) fails at launch with no credential.

**DOCKER_HOST**, **DOCKER_TLS_VERIFY**, **DOCKER_CERT_PATH**
:   Honored by the _docker_ backend through the standard Docker client
    environment.

Secrets are a stopgap: any injected secret is passed as container environment at
exec time only, never written to the journal, and redacted from traces.

**Droid opsec.** The droid adapter sets **OTEL_SDK_DISABLED**=**true** and
**OTEL_CUSTOMER_ENABLED**=**false** in the container to suppress Factory
telemetry. However, droid's **cloudSessionSync** feature — which mirrors session
content to Factory's web app — is on by default and has no environment-variable
knob; operators running sensitive workflows must disable it at the image level by
writing `{"general":{"cloudSessionSync":false}}` to
_~/.factory/settings.json_ inside the container image. The adapter cannot write
that file.

**Goose opsec.** The goose adapter sets **GOOSE_MODE**=**auto** (full tool
autonomy, appropriate for the isolated container), **GOOSE_DISABLE_KEYRING**=**1**,
and **GOOSE_TELEMETRY_ENABLED**=**false**, and redirects **XDG_DATA_HOME** and
**XDG_STATE_HOME** to an ephemeral in-container path (_/tmp/awf-goose_). The
redirect is load-bearing: goose writes the full cleartext session transcript and
logs under those directories even with `--no-session`, so without it they would
land in the operator's home directory. The adapter always passes `--no-session`
and never reuses a prior goose session.

**Codex opsec.** The codex adapter always passes **--ephemeral** (no session
persistence) and **--skip-git-repo-check**. By default it adds
**--dangerously-bypass-approvals-and-sandbox** — the AWF container is the
isolation boundary; codex's internal sandbox is redundant and its approval prompts
would block a non-interactive run. A `sandbox:` `with:` key opts into codex's
internal sandbox and removes the bypass flag. The schema temp file
(`/tmp/awf-codex-schema.json`) is written by the wrapping shell before codex
starts and read by codex's own process — verified to work under
**--sandbox read-only** (codex grants full-filesystem read; the write happens
outside codex's process boundary). The adapter enables no MCP servers.

**Droid model IDs.** The `factory/droid` `model` with-key is passed verbatim to
droid's `--model`. IDs are provider-prefixed and versioned per family — e.g.
`claude-sonnet-4-6` (Claude uses dashes), `gpt-5.5` (GPT/Gemini use dots),
`gemini-3.5-flash` — and default to `claude-opus-4-8`. **awf** keeps no model
list of its own (it drifts per droid release); an unknown ID is rejected at step
launch as a permanent config error carrying droid's available-models list. Run
`droid exec --model x` to print the IDs the installed droid accepts.

# FILES

The _state-dir_ is `./.awf` by default, overridable per-invocation with
**--state-dir** or for a shell session with the **AWF_STATE_DIR** environment
variable (an explicit flag wins; see **ENVIRONMENT**).

_state-dir_/runs/_run-id_/log
:   The run's append-only journal — the authoritative record folded on resume
    and projected into traces.

_state-dir_/blobs/
:   Shared content-addressed blob store: typed outputs, captured `output_files`,
    signal payloads, and streamed output chunks, deduplicated across runs.

_state-dir_ defaults to `./.awf`.

# EXAMPLES

Validate a workflow and print its digest:

    awf validate ./pipeline.yaml

Run against Docker with input, so the run can later be resumed:

    awf run --backend docker --input '{"repo":"acme/api"}' ./pipeline.yaml

Resume an interrupted run (same workflow file):

    awf resume 1a2b3c4d ./pipeline.yaml

Approve a waiting human-review step:

    awf signal 1a2b3c4d human_review --payload '{"approved":true}'

Pause a run to inspect the live lab, then continue:

    awf pause 1a2b3c4d
    awf resume 1a2b3c4d ./pipeline.yaml

# SEE ALSO

**awf-workflow**(5)

# FORMAT VERSION

The workflow format that **awf** executes is documented in **awf-workflow**(5);
this release implements format version 1. See the project README for an
introduction to AWF.
