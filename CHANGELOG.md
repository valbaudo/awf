# Changelog

All notable changes to `awf` are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and `awf` aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While `awf` is pre-1.0, the workflow format and CLI may still change between
minor versions. The workflow-format version (`version: 1` in a workflow file)
is tracked independently of the `awf` tool version.

## [0.1.2] - 2026-06-26

Fixes concurrent `anthropic/claude-code` runs on the `native` backend that failed
when 2+ ran in parallel. No workflow-format or CLI changes.

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

[0.1.2]: https://github.com/valbaudo/awf/releases/tag/v0.1.2
[0.1.1]: https://github.com/valbaudo/awf/releases/tag/v0.1.1
[0.1.0]: https://github.com/valbaudo/awf/releases/tag/v0.1.0
