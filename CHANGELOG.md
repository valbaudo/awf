# Changelog

All notable changes to `awf` are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and `awf` aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While `awf` is pre-1.0, the workflow format and CLI may still change between
minor versions. The workflow-format version (`version: 1` in a workflow file)
is tracked independently of the `awf` tool version.

## [Unreleased]

## [0.4.1] - 2026-07-08

A patch release fixing a native-backend timeout deadlock.

### Fixed

- **Native-backend timeout deadlock (leaked agent process).** On the native
  backend a step's real workload runs as a grandchild (under `sh -c`, and the
  sandbox trampoline). A timeout SIGKILLed only the direct child, so the
  grandchild survived holding the step's stdout/stderr pipe write-ends open — the
  reader goroutines never saw EOF, no step outcome was ever produced, and the
  whole run deadlocked, with the agent process orphaned and still holding its
  network connections. The native backend now runs each step in its own process
  group and kills the whole group on timeout (with a `WaitDelay` backstop),
  reaping the workload and surfacing the timeout as a retryable failure that
  feeds the existing retry / `recovery: continue` path — matching the Docker
  backend's existing process-tree reaping.

## [0.4.0] - 2026-07-08

A workflow-format release adding input-parameterizable agent roles: a reusable
role's `model`/`system_prompt`/top-level-string `with:` may reference
`{{ input.* }}`, resolved against the owning module's run input at step
execution, so one `--input` steers a whole fleet and forwards across a `call:`
boundary into a child workflow's own role. The workflow-format version
(`version: 1`) is unchanged.

### Added

- **Input-parameterizable agent roles.** A role's `model`/`system_prompt`/top-level
  string `with:` values may reference `{{ input.* }}`, resolved against the owning
  module's run input at step execution — so one `awf run --input model=…` steers a
  whole fleet, and a child workflow's own role reads a model forwarded via
  `call: input:`. Guarded by AWF1067 (`input.*` only; no nested templates).
  Behavior change: a role `with:` value that previously carried a literal
  non-`input` `{{ … }}` (e.g. `{{ run.id }}`) — which used to pass validation
  and reach the adapter as literal text — now fails validation with AWF1067.

## [0.3.0] - 2026-07-07

A workflow-format release. It renames several author-facing keys (migrate per
the BREAKING list below), adds container-less `run:` steps and native stall
detection with retry-as-continue, and tightens validation. The workflow-format
version (`version: 1`) is unchanged, but existing workflows must apply the key
migrations to keep validating.

### BREAKING (v0.3.0 format)

- **`reduce`'s quorum `over:` is renamed to `field:`.** The per-branch boolean
  field a `quorum` counts is now declared as `reduce: { quorum: <k>, field: <field> }`.
  Migration: rename `over:` to `field:` under `reduce.quorum` only — a `map`'s
  own `over:` (the fan-out worklist) is unrelated and unchanged.
- **The top-level workflow `input:` schema is renamed to `input_schema:`.**
  Migration: rename the workflow's top-level `input:` key to `input_schema:` —
  `{{ input.<field> }}` template references and a `call` step's own `input:`
  (the instance binding passed to a subworkflow) are unrelated and unchanged.
- **`prune`'s `keep: top(<k>)` is now `keep: <k>`.** The function-call-shaped
  wrapper is removed since top-k was always the only mode. Migration: replace
  `keep: top(<k>)` with the plain integer `keep: <k>`.
- **A signal `where:` clause now requires the `{{ }}` envelope, and correlates
  against the payload via a `signal.<field>` root.** The old bare-identifier
  form (an implicit substitute-into-string-then-parse against the payload,
  with a string-quoting hazard) is removed. Migration: wrap the clause in
  `{{ }}` and prefix payload fields with `signal.` — e.g.
  `where: 'candidate_id == "{{ hyp.id }}"'` becomes
  `where: "{{ signal.candidate_id == hyp.id }}"`. Every other root (`input.*`,
  `step.*`, `run.*`, an `as:` binding, …) resolves against the surrounding
  engine scope exactly as in `if`/`loop`/`gate` conditions, and the value is
  compared as typed data — no more quoting workaround for strings.

### Added

- **`output_artifact` is no longer containerless-only.** Any agent step that
  declares `output_schema` — container-backed or containerless — may now also
  declare `output_artifact: <name>` to publish its typed output as a
  content-addressed artifact. The requires-`output_schema` and
  mutually-exclusive-with-`output_files` rules (`AWF3014`) are unchanged.
- **`map`'s `over:` accepts a literal YAML sequence.** In addition to a
  `{{ }}` expression evaluated at runtime, `over:` may now be an author-fixed
  literal sequence (e.g. `over: [a, b, c]`) — a static, digest-pinned
  parameter sweep known before the run, as opposed to a runtime-sized
  worklist.
- **Container-less `run:` steps (bare shell).** A `run:` code step may now omit
  `container:`; it executes host-side under the native sandbox (auto-selected
  when the workflow declares no image), so a hello-world needs no image or
  digest procurement. A bare `run:` is rejected under `--backend docker`
  (`AWF1065`) — declare a `container:` or run native.
- **`map`'s `concurrency:` is optional (default 1, serial).** Omitting it runs
  the map serially; an explicit value `<= 0` is now rejected (`AWF1012`).
- **Native stall detection + retry-as-continue.** An idle watchdog cancels an
  agent step that goes silent longer than `timeout.idle` (distinct from the
  wall-clock `timeout`), turning a wedged agent into a retryable failure. The
  `openai/codex` live adapter forwards codex's reasoning-summary heartbeat and
  gets a generous default idle (~300s); every other adapter is opt-in, and
  `AWF3016` warns when `idle:` is set on an adapter that surfaces no liveness
  signal. On a stall, a persistent-session step resumes the *same* conversation
  thread — `retry: { recovery: continue }`, the default for session adapters —
  instead of restarting from scratch; `recovery: restart` is the escape hatch
  (`AWF1064` rejects any other value at validation time).
- **`awf validate` strictly rejects unknown workflow/step keys (`AWF1062`).** A
  stray or typo'd key anywhere in a workflow document — previously silently
  tolerated — is now a hard validation error.
- **`awf validate` strictly rejects bare-integer durations (`AWF1063`).** A
  `timeout` or `retry.initial`/`retry.max` value must be a quoted duration
  string (e.g. `"300s"`, not `300`) — a bare integer previously parsed as
  nanoseconds with no error, causing the step to time out instantly.

### Changed

- **Breaking (native backend):** run workdirs moved from `work/` to `work/<run-id>/`
  to isolate concurrent native runs. A native run started before this change
  cannot be resumed after upgrading (its workdir path moved). Docker runs are
  unaffected (already run-id namespaced). Start affected native runs fresh.

## [0.2.0] - 2026-07-01

New workflow-format capabilities for typed-output artifacts, conditional-branch
optionality, and nested output addressing. Backward-compatible — existing
workflows are unaffected and the workflow-format version (`version: 1`) is
unchanged.

### Added

- **`output_artifact:` on containerless agent steps.** A containerless `awf/llm`
  step can declare `output_artifact: <name>` to publish its validated
  `output_schema` object as a first-class, content-addressed artifact
  `step.<id>.files.<name>`. The dispatcher serializes the typed output to
  canonical JSON (byte-identical to the step's own typed-output blob — one blob,
  two references), so it flows through the existing artifact machinery: a
  deterministic gate evaluator reads it via `input_files`, and it forwards out of
  a gate to a workflow-level `output_files:` alias. Validated by `AWF3014`
  (containerless-only; requires `output_schema`; mutually exclusive with
  `output_files`). `react` is excluded in this release.
- **`if`-branch optionality.** A reference from outside an `if` to a step whose
  branch was *not* taken now resolves to a distinct ABSENT sentinel (`AWF4006`)
  instead of failing the run. In `outputs:` — and symmetrically in a workflow
  `output_files:` alias — the bound key is simply omitted; a `required`
  `output_schema` field still fails on omission. Omission composes across a
  sub-workflow `call`: a child that omits an optional output makes the parent's
  binding omit too. Referencing an absent step anywhere else (a `run:`
  substitution, a gate `until`, another step's input) remains an author error.
- **`first_of:` output selection.** A workflow output may bind
  `first_of: [ <ref>, <ref>, ... ]` to select the first present (non-absent)
  reference — the "read whichever `if` branch ran" pattern — as a structured
  directive, without adding an operator to the templating language.
- **`awf outputs --step` reads nested runtime paths.** `--step` now accepts a
  step's full runtime address verbatim (e.g.
  `gate[0].attempt-2.generate.<id>`, `map[0].item-3.<id>`,
  `loop[0].body.iter-3.<id>`), honoring the documented promise. The caller names
  the instance; a path that names no committed step (including a step under a
  non-taken `if` branch, or a path missing its suffix) is a read failure
  (exit 1), not a usage error.

### Changed

- **`AWF3012` warning wording.** The conditional-scope-output validation warning
  now describes the new omit / required-fail semantics — an output bound to a
  non-taken `if` branch is omitted (and only *required* schema fields then fail),
  rather than claiming `awf outputs` will error.

## [0.1.3] - 2026-06-26

Fixes concurrent `anthropic/claude-code` runs on the `native` backend that failed
when 2+ ran in parallel. No workflow-format or CLI changes. (Supersedes the `v0.1.2`
tag, whose release build failed a formatting check and published nothing.)

### Fixed

- **Concurrent native Claude runs no longer contend on claude-code's version lock.**
  0.1.1 gave each run its own `CLAUDE_CONFIG_DIR`, but claude-code keeps a per-version
  single-instance lock at `$XDG_STATE_HOME/claude/locks/<version>.lock` — *outside* the
  config dir. On the `native` backend every run inherits the shared host `$HOME` (and
  therefore `$XDG_STATE_HOME`), so two or more concurrent runs of the same claude version
  contended on the same lock and the loser exited non-zero. Both `anthropic/claude-code`
  and `anthropic/claude-code-session` now also relocate `XDG_STATE_HOME` and
  `XDG_CACHE_HOME` to per-run directories alongside `CLAUDE_CONFIG_DIR`, isolating the
  lock and cache per run. `HOME` and `XDG_DATA_HOME` are intentionally left shared
  (claude's versioned binary lives under `XDG_DATA_HOME` and must stay resolvable).

## [0.1.1] - 2026-06-26

Adds a persistent-session Claude Code adapter with per-run configuration
isolation, fixes a data race in the Claude adapters, and refreshes dependencies.
No workflow-format or CLI-flag changes — `awf` workflows and commands from 0.1.0
run unchanged.

### Added

- **`anthropic/claude-code-session` adapter.** A persistent-session variant of
  `anthropic/claude-code`: instead of discarding Claude Code's on-disk session
  after each turn it reuses it. When the step commits, `awf` captures the
  session's `projects/` directory as a content-addressed artifact; before the step
  launches again it restores that subtree and resumes the same session
  (`claude --resume`). A re-executed step — for example after `awf resume` —
  continues from Claude's own session state, including internal state a
  re-assembled message log cannot reconstruct. Generator-only: a persistent-session
  runtime is rejected as a gate evaluator, so the judge always starts fresh.
  Documented in `awf-workflow(5)`.
- **Per-run Claude configuration isolation.** Both `anthropic/claude-code` and
  `anthropic/claude-code-session` now run each `awf run` with its own
  `CLAUDE_CONFIG_DIR` under the per-run `.awf` staging root, so concurrent runs on
  the `native` backend no longer collide on a shared `~/.claude` (config, project
  registry, session journal, telemetry). Non-essential Claude Code traffic
  (telemetry, auto-updater) is disabled for unattended agent runs.

### Fixed

- **Data race in the Claude adapters.** Concurrent `Launch` calls no longer mutate
  the adapter's shared environment map — the per-invocation `AWF_IDEMPOTENCY_KEY`
  and `CLAUDE_CONFIG_DIR` writes now go to a fresh copy. Affected both
  `anthropic/claude-code` and `anthropic/claude-code-session`.
- `native` backend: the Landlock sandbox now grants `/proc`, `/sys`, and `/dev`,
  so confined steps that need them start correctly.

### Changed

- Bumped OpenTelemetry to 1.44, `openai-go` to 3.41, and `actions/checkout` to v7.
- `SECURITY.md` documents why the Docker-toolchain advisories cannot yet be patched
  (no fixed upstream release).

## [0.1.0] - 2026-06-24

First public release. A single-binary runtime for agentic workflows with an
independent acceptance gate and content-addressed checkpoint/resume.

### Added

- **The gate.** Engine-enforced `generate` → `evaluate` → `repair` loops with a
  bounded `until` condition over the evaluator's typed output. The evaluator
  runs in a fresh context (independence) and the prior verdict is fed into the
  next generate attempt (feedback). A crash is not a verdict — only a real
  evaluation with a false `until` consumes a repair attempt.
- **Black-box agent adapters.** Wrap external agent CLIs as steps via `uses:`:
  `anthropic/claude-code`, `factory/droid`, `block/goose`, `openai/codex`, and
  `openai/codex-live`, plus `awf/llm` for direct OpenAI-compatible HTTP (Ollama,
  vLLM, llama.cpp, LM Studio, LiteLLM, and similar gateways). The per-step
  `with:` map is opaque to the engine and validated only by the named adapter.
- **Typed outputs.** Downstream steps bind to validated `output_schema` fields
  rather than raw text.
- **Checkpoint/resume.** Typed outputs and declared `output_files` commit to a
  content-addressed artifact store before the append-only journal pointer moves.
  Resume folds the journal: committed steps are replayed from artifacts and only
  the uncommitted frontier re-executes. `awf resume --from` re-runs from a chosen
  committed node.
- **Pinned replay.** A structural change — topology, imported files, assets,
  container digests, or resolved runtime versions — hard-fails a resume; an edit
  confined to step bodies reuses unchanged committed steps and re-runs from the
  first change.
- **Execution backends.** `native` host processes with per-run OS-sandbox
  write-confinement (bubblewrap or Landlock on Linux, `sandbox-exec` on macOS;
  fail-closed), `docker` with digest-pinned images and Compose projects, and an
  in-memory `fake` backend for conformance tests.
- **Control flow.** Map/reduce fan-out and fan-in, an engine-journaled tool loop
  (`tools:` + `react:`), imported subworkflows, keyed `await` signals, and native
  skill routing.
- **Observability.** Trace export, journal-fold run status, and run inspection
  without putting observability in the execution path.
- **Cost accounting.** Token-to-dollar pricing derived from a reviewed, embedded
  rate table.
- **CLI.** `awf validate`, `run`, `resume`, `ls`, and `version`, with
  `--backend`, `--output/-o`, `--input`/`--input-file(s)`, and `AWF_STATE_DIR`.
- **Documentation.** `awf(1)` command reference and `awf-workflow(5)`
  workflow-format reference (the stable format contract).

[0.2.0]: https://github.com/valbaudo/awf/releases/tag/v0.2.0
[0.1.3]: https://github.com/valbaudo/awf/releases/tag/v0.1.3
[0.1.1]: https://github.com/valbaudo/awf/releases/tag/v0.1.1
[0.1.0]: https://github.com/valbaudo/awf/releases/tag/v0.1.0
