# Changelog

All notable changes to `awf` are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and `awf` aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While `awf` is pre-1.0, the workflow format and CLI may still change between
minor versions. The workflow-format version (`version: 1` in a workflow file)
is tracked independently of the `awf` tool version.

## [0.1.0] - 2026-06-23

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

[0.1.0]: https://github.com/valbaudo/awf/releases/tag/v0.1.0
