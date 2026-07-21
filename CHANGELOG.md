# Changelog

All notable changes to `awf` are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and `awf` aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While `awf` is pre-1.0, surfaces outside the machine contract may still change
between minor versions. The machine-facing contract — the `awf run` / `awf outputs`
interface, the exit codes, and the workflow format with its JSON Schema — is a
versioned stability promise; see [COMPATIBILITY.md](COMPATIBILITY.md). The
workflow-format version (`version: 1` in a workflow file) is tracked independently
of the `awf` tool version.

## [Unreleased]

## [0.6.0] - 2026-07-21

Ownership of the host↔container boundary. Artifacts a step produces can now be
materialized onto the host by name, agent CLIs stay runnable and stay logged in
under the native sandbox, and the documentation stops describing an MCP input
channel that never existed.

### Added

- **`awf outputs --dest <dir>`.** Materializes a committed step's `output_files`
  onto the host, written by the `awf` process itself and therefore owned by the
  invoking user — no root-owned writeback, no `chown` pass. Each declared
  container path is mirrored under `<dir>` with its leading separator stripped,
  confined by `os.Root`: a `..` component or a symlink that would escape the
  destination is refused, while an absolute container path is contained rather
  than rejected. Every destination is resolved before the first byte is written,
  so a colliding or unusable path fails the command without leaving a partial
  tree.

### Fixed

- **Agent token refresh survives the native sandbox.** Credential directories
  were re-bound read-only, so an OAuth refresh written during a run was
  discarded when the sandbox tore down its mount namespace — a silent logout
  days later. The per-agent directories (`~/.claude`, `$CODEX_HOME` or
  `~/.codex`, `~/.factory`, `~/.config/goose`) are now writable for agent
  invocations; the bare `~/.config` catch-all stays read-only, so a step never
  gains write access to the wider XDG tree.
- **Agent CLIs are executable under the native sandbox.** Read+execute is now
  granted for `~/.local` (where `~/.local/bin` entries and the `~/.local/lib`
  or `~/.local/share` targets they symlink to live) and `/opt` (where node and
  npm global prefixes land, including the real binary behind a
  `/usr/local/bin` shim). Without them a step died with an opaque exit `126`
  or `127` instead of a named error.
- **The ignored-image warning names the remedy.** `--backend native` on a
  workflow that declares a container image now points at `--backend auto`, the
  default that routes such a workflow to Docker, instead of only stating that
  the image was ignored.

### Security

- The writable credential grant refuses any candidate that is the home
  directory or an ancestor of it. `$CODEX_HOME` is environment-derived, and a
  broad value would otherwise have been re-bound read-write over the sandbox's
  `$HOME` tmpfs, restoring the real home — `~/.ssh` included — inside the
  sandbox.
- The writable credential and tool-prefix grants apply only to an agent adapter
  invoking its own CLI. A workflow `run:` step receives neither, and keeps
  exactly the read-only credential baseline it had before.

### Changed

- **MCP is documented as what it is.** The format reference advertised
  `with: { mcp_servers: ... }` as an input channel, and the runtime design
  claimed the Claude adapter mapped `mcp://` refs to a harness config. No
  adapter ever accepted such a key — each validates `with:` against a strict
  allowlist and rejects an unknown key at run start — and no `--mcp-config` was
  ever emitted. The claims are removed rather than the rejection. In their place
  the reference documents the arrangement that already works: run an
  HTTP-transport MCP server as a digest-pinned Compose service and pass the
  agent its URL, inheriting pinning, health-gated readiness, reconstruction from
  the recipe on resume, and teardown with the run. `with:` remains opaque and
  adapter-validated; no format field was added or removed.
- **Compose bind-mount ownership is documented.** `awf` never bind-mounts a host
  directory, so root-owned files on the host can only originate in an author's
  own Compose file. The manual now records the fixes at that source — `user:` on
  the service, a rootless engine, or an idmapped mount — and why those are
  preferable to `:U` or `--userns-remap`.
- **The sanctioned agent-CLI install prefixes are documented** under
  `--backend native`, with the exit-`126` failure they explain.

## [0.5.2] - 2026-07-15

A documentation-only patch release adding a clean-room Claude Code subscription
readiness path. There are no runtime or workflow-format changes.

### Added

- **Claude Code gated readiness example.** A public workflow invokes the host
  Claude Code CLI as a black box from one AWF agent step, validates its typed
  output, and passes it through an independent deterministic gate. Generator
  and evaluator retries plus the gate attempt limit are each set to one, so AWF
  performs no orchestration-level reruns. The accompanying guide documents
  private `claude setup-token` handoff, the explicit native backend, the
  macOS-safe absolute state directory, gate-scoped output inspection, cleanup,
  and troubleshooting.

## [0.5.1] - 2026-07-08

A patch release that clears resolvable dependency advisories. No functional
changes to the tool.

### Security

- **Dependency bumps clearing 11 of 20 Dependabot advisories**, including the
  sole critical. Go: `containerd/v2` 2.1.5 → 2.1.9 (5 advisories) and
  `in-toto-golang` 0.9.0 → 0.11.0 (1). UI dev tooling: `vite` 5 → 6.4.3 and
  `vitest` 2 → 3.2.6 (5, including the critical `vitest` UI-server file-read;
  these packages are build-time only and never ship in the `awf` binary). The
  nine remaining advisories are all in the Docker/Moby client chain and are
  either unfixed upstream or blocked on the Docker v29 / `moby/moby` migration
  that `docker/compose` has not yet adopted; [SECURITY.md](SECURITY.md) tracks
  each one and why it is still present.

## [0.5.0] - 2026-07-08

A minor release adding the Contract-v1 machine-contract stability policy and
richer live visibility for the `openai/codex-live` agent adapter.

### Added

- **Machine-contract stability policy** ([COMPATIBILITY.md](COMPATIBILITY.md)):
  the `awf run` / `awf outputs` interface, the exit codes, and the workflow format
  plus a published JSON Schema (`schema/workflow.v1.schema.json`) are now a
  versioned **Contract v1**, decoupled from the binary's `0.x` version. Adds a
  consolidated output-binding-rules section to `awf-workflow(5)` and a
  `COMPATIBILITY` section to `awf(1)`.
- **Live event visibility for `openai/codex-live`.** The adapter now maps its
  app-server events onto the shared display vocabulary instead of the raw
  `[kind] {json}` fallback, and fills the previously-empty durable per-event
  journal record: assistant answers stream as prose, reasoning renders dimmed,
  shell commands surface as tool call/result lines (command, elided output,
  line/byte counts, exit status), and a failed turn surfaces a live error. A
  streamed answer that also arrives as a completed item no longer prints twice.

### Fixed

- **`openai/codex-live` misread every successful app-server response as an
  error.** A nil `*RPCError` was copied into the `error`-typed response field,
  producing a non-nil (typed-nil) interface, so each successful JSON-RPC call
  returned a bogus `"<nil>"` error and `turn/start` could nil-panic — the
  adapter could not complete a turn. Success responses now carry a true nil
  error.

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
