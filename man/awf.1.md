AWF 1 "May 2026" "AWF" "AWF Manual"
===================================

# NAME

awf - orchestrate black-box agent CLIs and shell commands as gated, checkpointed workflows

# SYNOPSIS

**awf** _command_ [_arguments_]

**awf** **validate** [**--format** _text_|_json_] _path_

**awf** **run** [**--input** _json_] [**--run-id** _id_] [**--state-dir** _dir_] [**--backend** _fake_|_docker_|_native_] [**--agent-env** _csv_] _path_

**awf** **resume** [**--state-dir** _dir_] _run-id_ _path_

**awf** **signal** [**--payload** _json_] [**--state-dir** _dir_] _run-id_ _name_

**awf** **pause** [**--reason** _text_] [**--state-dir** _dir_] _run-id_

**awf** **cancel** [**--reason** _text_] [**--state-dir** _dir_] _run-id_

**awf** **ls** [**--state-dir** _dir_] [**--output** _text_|_json_]

**awf** **inspect** _run-id_ [**--state-dir** _dir_] [**--fold** _statuses_] [**--depth** _n_] [**--output** _text_|_json_]

**awf** **trace** _run-id_ [**--state-dir** _dir_] [**--otlp** _endpoint_] [**--capture-content**] [**--output** _otel_|_json_]

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

# COMMANDS

## awf validate _path_

Parse _path_, validate it against the AWF format (**awf-workflow**(5)), and print
the workflow's content-addressed definition digest. Validation collects *all*
diagnostics in one pass; any error-severity diagnostic yields a non-zero exit
(see **EXIT STATUS**). This command performs no container, network, or agent I/O.

**--format** _text_|_json_
:   Output format. _text_ (default) is human-readable. _json_ emits a
    machine-readable object carrying the digest and each diagnostic's stable
    namespaced `code` (`AWF1xxx` structural, `AWF2xxx` schema-floor, `AWF3xxx`
    loader) — an API for tooling, never renumbered.

## awf run _path_

Mint a run id, validate _path_, bring each declared container to readiness from
its image or Compose recipe, and execute the graph. Step and agent progress
streams to standard output as a live tap; the final line reports the run id and
terminal outcome (for example `run 1a2b3c4d: ok`). Run state is written under
_state-dir_ — a per-run journal and a shared content-addressed blob store (see
**FILES**).

**--input** _json_
:   Run parameters as a JSON object, validated against the workflow's `input`
    schema. It is an error to pass **--input** when the workflow declares no
    `input` schema.

**--run-id** _id_
:   Use _id_ as the run id instead of minting a fresh random one. A testing and
    scripting aid; run ids are otherwise unique and unpredictable.

**--state-dir** _dir_
:   Base directory for the `runs/` journals and the shared `blobs/` store
    (default `./.awf`).

**--backend** _fake_|_docker_|_native_
:   Where steps execute (default _native_). _docker_ runs them in real
    containers and Compose projects with full isolation; _native_ runs them as
    host processes with no isolation; _fake_ is an in-memory backend for tests.
    Compose-mode containers require _docker_. Only _docker_ and _fake_ runs are
    resumable — a _native_ run cannot be resumed.

**--agent-env** _csv_
:   Comma-separated allowlist of environment-variable *names* forwarded into
    `uses: anthropic/claude-code` invocations (default
    `ANTHROPIC_API_KEY,ANTHROPIC_AUTH_TOKEN,CLAUDE_CODE_OAUTH_TOKEN`). Names not
    on the list are not passed through. See **ENVIRONMENT**.

## awf resume _run-id_ _path_

Re-enter an interrupted run. **awf** folds the run's journal, then verifies that
the on-disk _path_ still hashes to the recorded definition digest *and* that
every resolved agent-runtime version still matches — any drift is a hard error,
never silently adapted. It recreates containers from their recipe and continues
in a new epoch. Committed steps are replayed from the journal (their recorded
outputs reused, not recomputed); only the uncommitted frontier re-executes. The
backend is read back from the journal, so no **--backend** flag is given. Runs
made with the _native_ backend are not resumable.

**--state-dir** _dir_
:   Base directory holding the run (default `./.awf`).

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
`cancelled` marker. A cancelled run is *not* resumable — **resume** refuses
afterward.

**--reason** _text_
:   Operator note recorded in the journal.

**--state-dir** _dir_
:   Base directory holding the run (default `./.awf`).

## awf ls

List the runs under _state-dir_/`runs/`, one per line, each with a status derived
by folding its journal: `running`, `paused`, `finished`, `failed`, `cancelled`,
or `crashed` (started, no terminal event, and no live process holding the run's
lock). This command only reads state; it executes nothing.

**--state-dir** _dir_
:   Base directory holding `runs/` and `blobs/` (default `./.awf`).

**--output** _text_|_json_
:   Output format. _text_ (default) prints one run per line; _json_ emits a
    machine-readable array.

## awf inspect _run-id_

Render a run's addressing tree as a text tree, folded by status: `ok` subtrees
collapse, while `failed`, `rejected`, and `incomplete` subtrees expand, so a
failing branch stands out. This reads the journal offline and executes nothing.

**--fold** _statuses_
:   Comma-separated list of node outcomes to collapse (default `ok`).

**--depth** _n_
:   Maximum tree depth to render (default: unlimited).

**--output** _text_|_json_
:   _text_ (default) is the folded tree; _json_ emits the underlying span
    projection.

**--state-dir** _dir_
:   Base directory holding the run (default `./.awf`).

AWF does **not** offer Temporal-style deterministic replay: resume folds the
journal and re-runs only the uncommitted frontier, with no author-code
determinism contract.

## awf trace _run-id_

Project a run's journal into OpenTelemetry spans — one span per addressing-tree
node, mirroring the resume tree — and export them. By default the spans are
written to standard output (a zero-infrastructure local exporter); with
**--otlp** they are sent to a collector instead. This reads the journal offline
and executes nothing.

**--otlp** _endpoint_
:   Export to an OTLP/HTTP collector at _host:port_ (plaintext) instead of
    standard output.

**--capture-content**
:   Also attach agent I/O (prompts, agent output, stdout) and typed-output
    values to the spans. Off by default. Combined with **--otlp** this transmits
    that content — including anything sensitive embedded in prompts — to the
    collector.

**--output** _otel_|_json_
:   _otel_ (default) exports spans; _json_ dumps the span projection as JSON, for
    download or scripting, instead of exporting.

**--state-dir** _dir_
:   Base directory holding the run (default `./.awf`).

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
    (parse error, path escape, or a missing Compose file).

# ENVIRONMENT

**ANTHROPIC_API_KEY**, **ANTHROPIC_AUTH_TOKEN**, **CLAUDE_CODE_OAUTH_TOKEN**
:   Authentication for the `anthropic/claude-code` agent runtime. **awf** does not
    read these itself; it forwards those named in **--agent-env** (all three by
    default) into the agent invocation. The runtime defaults to a minimal *bare*
    mode that accepts only `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`; to
    authenticate with a subscription `CLAUDE_CODE_OAUTH_TOKEN` the agent step must
    set `bare: false`, otherwise the token is ignored and config validation fails.

**DOCKER_HOST**, **DOCKER_TLS_VERIFY**, **DOCKER_CERT_PATH**
:   Honored by the _docker_ backend through the standard Docker client
    environment.

Secrets are a stopgap: any injected secret is passed as container environment at
exec time only, never written to the journal, and redacted from traces.

# FILES

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
