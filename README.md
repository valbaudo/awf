# awf

[![CI](https://github.com/valbaudo/awf/actions/workflows/ci.yml/badge.svg)](https://github.com/valbaudo/awf/actions/workflows/ci.yml) [![Release](https://img.shields.io/github/v/release/valbaudo/awf?sort=semver)](https://github.com/valbaudo/awf/releases) [![Go Report Card](https://goreportcard.com/badge/github.com/valbaudo/awf)](https://goreportcard.com/report/github.com/valbaudo/awf) [![Go Reference](https://pkg.go.dev/badge/github.com/valbaudo/awf.svg)](https://pkg.go.dev/github.com/valbaudo/awf) [![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/) [![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**Run agents you don't babysit, and trust the result.**

awf is a single-binary runtime for agentic workflows that need a real acceptance
gate: run the agent, check the result independently, repair from the critique,
and resume safely after crashes without redoing committed work.

It is built for workflows where a model saying "done" is not good enough:
coding agents, research agents, data-migration agents, support-reply agents,
security triage agents, or any pipeline where each stage should advance only
after an external check passes.

[CLI reference](man/awf.1.md) | [Workflow format](man/awf-workflow.5.md)

## Why AWF Exists

Agentic workflows are useful because they let models do open-ended work. They
are risky for the same reason: the step can report success without actually
meeting the requirement.

AWF makes the acceptance check part of the runtime.

```mermaid
flowchart LR
    generate["generate<br/>agent or command"]
    evaluate["evaluate<br/>fresh judge or deterministic check"]
    pass{"passes?"}
    next["next stage"]
    repair["repair with critique"]

    generate --> evaluate --> pass
    pass -->|"yes"| next
    pass -->|"no"| repair --> generate
```

The central primitive is the **gate**: a generate block, an independent evaluate
block, an `until` condition over the evaluator's typed output, and a bounded
repair loop. The evaluator runs in a fresh context, so the generator never marks
its own homework. A crash is not a verdict; only a real evaluation with a false
`until` consumes a repair attempt.

## What You Get

- **Independent gates**: engine-enforced generate -> evaluate -> repair loops,
  with the prior verdict automatically fed into the next generate attempt.
- **Black-box agents**: wrap existing CLIs such as Claude Code, Factory droid,
  Block Goose, OpenAI Codex, or use `awf/llm` for direct OpenAI-compatible HTTP.
- **Input-parameterizable roles**: a reusable `agents:` role's `model` /
  `system_prompt` may reference `{{ input.* }}`, so one `--input` steers a
  whole fleet — and forwards across a `call:` boundary into a child's own role.
- **Typed outputs**: downstream steps bind to validated `output_schema` fields,
  not fragile free text.
- **Checkpoint/resume**: step outputs and declared files commit to a
  content-addressed artifact store before the journal pointer moves.
- **Stall recovery**: an idle watchdog catches a wedged agent (silent longer than
  `timeout.idle`, distinct from slow-but-thinking) and turns it into a retryable
  failure; on a stall a persistent-session step resumes the *same* conversation
  thread (`recovery: continue`) instead of restarting. Default-on for the codex
  live adapter, which streams a reasoning heartbeat; opt-in elsewhere.
- **Pinned replay**: a structural change — topology, imported files, assets,
  container digests, or resolved runtime versions — hard-fails the resume; an edit
  confined to step bodies instead reuses the unchanged committed steps and re-runs
  from the first change.
- **Real workspaces**: run against long-lived native processes, digest-pinned
  containers, or Compose labs; Docker handles networking, healthchecks, and
  multi-service wiring.
- **Traceable runs**: inspect runs, fold status from the log, and export traces
  without putting observability in the execution path.

## A First Workflow

The smallest AWF workflow needs four things: a format version, a graph, a step
id, and a command. No container image, no agent credentials, no Ollama server:

```yaml
version: 1
graph:
  - id: hello
    run: echo "hello from awf"
```

Save it as `hello.yaml` and run it:

```sh
awf run hello.yaml
```

```
awf run: auto-selected native backend (no Docker-only features). Resume restores snapshot: workspace workdirs from a full workdir archive but does not pin the host base environment; use --backend docker for a pinned baseline.
hello from awf
run 1a2b3c4d: ok
```

(The run id on the last line is minted per run, so yours will differ.)

The step declares no `container:`, so `auto` backend selection finds no
Docker-only feature to route to and picks `native`: the command runs directly
on the host, write-confined to its own per-step host workspace when an OS
sandbox is usable (bubblewrap or Landlock on Linux, `sandbox-exec` on macOS), and its
output is committed to the run's journal — the same checkpoint path a
Docker-backed step uses, just with no container boundary. `--backend docker`
refuses a bare `run:` step outright (AWF1065): there is no image to run it in,
so let `auto` decide, or pass `--backend native` yourself.

Real workflows are rarely one step, and running a command is not what makes
AWF different from a shell script. The primitive that does is the **gate**: an
independent check that decides whether to advance. The next section shows one.

## A Gated Workflow

This workflow asks a model to write a release note, then has an independent
judge approve it or send feedback into the next repair attempt. Unlike the
hello-world above, it needs a running OpenAI-compatible endpoint (Ollama here)
and an `OPENAI_API_KEY` env var — see Quickstart below for the exact commands
to run it.

```yaml
workflow: gated-release-note
version: 1

graph:
  - gate:
      generate:
        - id: draft
          uses: awf/llm
          with:
            base_url: http://localhost:11434/v1
            model: llama3.1
            api_key_env: OPENAI_API_KEY
            system_prompt: "You write concise release notes."
            prompt: |
              Write a three-sentence release note for a new `awf run --json` flag.
              Prior review feedback, if any: {{ evaluate.feedback }}
          output_schema:
            type: object
            additionalProperties: false
            required: [release_note]
            properties:
              release_note: { type: string }

      evaluate:
        - id: judge
          uses: awf/llm
          with:
            base_url: http://localhost:11434/v1
            model: llama3.1
            api_key_env: OPENAI_API_KEY
            system_prompt: "You are a strict release-note reviewer."
            prompt: |
              Review this release note:

              {{ step.draft.release_note }}

              Approve it only if it is accurate, specific, and exactly three sentences.
          output_schema:
            type: object
            additionalProperties: false
            required: [approved, feedback]
            properties:
              approved: { type: boolean }
              feedback: { type: string }

      until: "{{ evaluate.approved }}"
      max_attempts: 3
```

That same shape works for higher-stakes tasks: generate a patch and run tests,
draft a customer reply and judge it against account data, triage a CVE and check
the exploitability claim, or migrate data and verify the target state.

## Install

### Prebuilt binary (recommended)

Download the archive for your platform from the
[Releases page](https://github.com/valbaudo/awf/releases), verify it against the
published checksums, and put `awf` on your `PATH`. Prebuilt binaries are
published for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`.

```sh
VERSION=0.5.2
OS=linux ARCH=amd64        # or: OS=darwin ARCH=arm64
BASE="https://github.com/valbaudo/awf/releases/download/v${VERSION}"

curl -LO "${BASE}/awf_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -LO "${BASE}/awf_${VERSION}_checksums.txt"
shasum -a 256 -c "awf_${VERSION}_checksums.txt" --ignore-missing

tar -xzf "awf_${VERSION}_${OS}_${ARCH}.tar.gz"
sudo install "awf_${VERSION}_${OS}_${ARCH}/awf" /usr/local/bin/awf
awf version
```

The `sudo` above applies only to installing the binary into `/usr/local/bin`.
Do not prefix `awf run`, `resume`, `signal`, `pause`, or `cancel` with `sudo`,
`doas`, or `pkexec`. AWF writes `.awf` as the invoking user and refuses a
foreign-owned state root with path and owner guidance. Read-only commands never
write state and accept an absent blob store. AWF rejects elevation provenance,
not UID 0 itself: genuine root and container sessions remain allowed when they
own the state root. If elevated execution previously created `.awf`, use its
owning account or select a new user-owned directory with `--state-dir`.

Release archives are built by GitHub Actions and carry a [build-provenance
attestation](https://docs.github.com/actions/security-guides/using-artifact-attestations-to-establish-provenance-for-builds).
You can verify one with the GitHub CLI:

```sh
gh attestation verify "awf_${VERSION}_${OS}_${ARCH}.tar.gz" --repo valbaudo/awf
```

### Go install

With a Go 1.26+ toolchain:

```sh
go install github.com/valbaudo/awf/cmd/awf@latest
```

## Quickstart

To build from source instead — the path for contributors:

```sh
git clone https://github.com/valbaudo/awf.git
cd awf
make build
```

Validate a workflow without running agents, containers, or network I/O:

```sh
bin/awf validate examples/awf-llm-ollama/workflow.yaml
```

Run the local Ollama example after starting an OpenAI-compatible Ollama server
and forwarding an API-key env var. Ollama can ignore the bearer value; AWF still
requires the named env var to be present because adapters never inline secrets
into workflow files.

```sh
export OPENAI_API_KEY=ollama
bin/awf run examples/awf-llm-ollama/workflow.yaml
```

To exercise a Claude Code subscription through a typed, deterministic gate
without Docker or Ollama, follow the [Claude Code gated readiness
example](examples/claude-code-gated/README.md). It uses the published v0.5.1
binary, a `claude setup-token` credential handoff, and the explicit native
backend.

Use Docker when you want resumable runs or workflows that need isolated
containers/Compose labs:

```sh
bin/awf run --backend docker path/to/workflow.yaml
bin/awf resume <run-id> path/to/workflow.yaml
```

Useful development checks:

```sh
make lint test        # pre-commit bar
make build            # build ./bin/awf
make integ            # Docker/native integration suite; no live API spend
```

### Cutting a release (maintainers)

Releases are tag-triggered. Push an annotated, signed `vMAJOR.MINOR.PATCH` tag;
GitHub Actions then re-runs the full gate, builds the four platform archives,
and publishes the GitHub Release with checksums and a build-provenance
attestation:

```sh
git tag -s v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

Published version tags are immutable: fix a bad release with the next patch tag
(`v0.1.1`), never by moving or deleting a tag. See
[CONTRIBUTING.md](CONTRIBUTING.md) for the full contributor bar.

## How AWF Is Different

| Concern | Common agent framework shape | AWF shape |
| --- | --- | --- |
| Model execution | Framework calls the model in-process | Runtime wraps external CLIs or one HTTP LLM call as black boxes |
| Quality check | Author wires evaluator nodes or post-hoc evals | Engine owns the independent gate and repair loop |
| State | Mutable snapshots or app-managed memory | Append-only journal plus content-addressed artifacts |
| Resume | Recompute, restore app state, or apply latest definition | Replay committed steps, rerun only the uncommitted frontier |
| Drift | Often latest-wins | Definition digest and runtime versions are pinned; drift is a hard error |
| Infra | Usually in-process or platform-owned workers | Single-host native/Docker/Compose workspaces, rebuilt from pinned recipes |

AWF is not trying to be a distributed scheduler, a durable-execution platform,
or a general agent-team framework. It is a narrow runtime for single-host,
checkpointed agentic pipelines where every meaningful stage has to pass an
independent check before the workflow can move on.

## Core Concepts

- **Workflow**: a YAML document with input schema, optional assets/imports,
  execution infrastructure, and a graph of steps/control flow. The resolved
  document is content-addressed at run start. Its `version: 1` field is the
  workflow-format version and is independent of the `awf` tool version — `awf
  v0.1.0` implements workflow format version 1.
- **Step**: a `run:` command, `uses:` agent invocation, `await` signal, or
  imported workflow call. Steps can produce typed JSON outputs and named output
  files.
- **Gate**: a control node that runs generate steps, evaluates them
  independently, checks `until`, and repairs with feedback until the condition
  passes or `max_attempts` is exhausted.
- **Commit**: the durable unit of progress. AWF writes typed outputs, declared
  files, and optional workspace snapshots to the blob store first; only then
  does it append the completed journal event.
- **Resume**: a fold over the journal. Completed steps are replayed from
  committed artifacts; only the uncommitted frontier re-executes. A structural or
  runtime-version change stops the resume instead of silently adapting; an edit
  confined to step bodies reuses unchanged committed steps and re-runs from the
  first change (per-node verifying-trace reuse).

## Adapters and Backends

Agent steps name a runtime with `uses:`. The step's `with:` map is opaque to the
engine; only the named adapter validates and interprets it.

Built-in adapters:

- `anthropic/claude-code` — see the [gated native readiness
  example](examples/claude-code-gated/README.md)
- `factory/droid`
- `block/goose`
- `openai/codex`
- `awf/llm` for OpenAI-compatible Chat Completions endpoints, including local
  Ollama, vLLM, llama.cpp, LM Studio, LiteLLM, and Bifrost-style gateways
- `openai/codex-live` for Codex app-server-backed live sessions

The `openai/codex-live` adapter uses the same `uses:` resolution, runtime
pinning, live event stream, trace, and UI surfaces as other agent steps. It
stores provider session metadata under the live home and keeps raw live
transcripts provider-owned. `block/goose-live` remains a reserved implementation
track ref, and `anthropic/claude-code-live` remains deferred behind a PTY proof
spike.

Execution backends:

- `native`: host processes, fastest path; no container boundary. AWF selects the first functionally usable write-confinement launcher (bubblewrap then Landlock on Linux, `sandbox-exec` on macOS). If none is usable, native runs without confinement and prints a loud stderr warning; use `--backend docker` when confinement is required. Native is resumable (`snapshot: workspace` workdirs are restored on resume). Explicit `--backend native` runs image-mode workflows on the host, ignoring the declared image; the host base environment is not pinned — use `--backend docker` for a fully reproducible baseline
- `docker`: digest-pinned images and Compose projects, resumable
- `fake`: in-memory backend for conformance tests

See [awf(1)](man/awf.1.md) for adapter environment variables, streaming notes,
security caveats, and CLI flags.

## Documentation

- [awf(1)](man/awf.1.md): command reference, flags, exit status, environment,
  tracing, and examples.
- [awf-workflow(5)](man/awf-workflow.5.md): the workflow-format reference and
  stable contract for fields, control flow, templating, typed outputs, and
  checkpoint/resume.
- [COMPATIBILITY.md](COMPATIBILITY.md): the machine-contract stability policy —
  the Contract v1 stability ladder, the plumbing/porcelain split, and schema versioning.
- [examples/](examples/): runnable examples for `awf/llm`, droid BYOK, and
  engine-owned conversation threads.

## Further Reading

These are the sources AWF actually draws from. The list is intentionally narrow.

### Runtime foundations

- Anthropic, [Building Effective Agents][anthropic]: simple, composable
  workflows; prompt chaining with gates; evaluator-optimizer loops; and the
  "use the simplest architecture that works" bias behind AWF's small core.
- Anthropic, [Effective Context Engineering for AI Agents][contexteng]:
  context as a finite resource; AWF's evaluator runs in a fresh context, while
  explicit typed outputs and artifacts carry the state that should survive.
- Anthropic, [How we built our multi-agent research system][multiagent]:
  shaped AWF's shipped coordinator primitives: artifact handoff, fan-out/fan-in,
  reusable agent roles, engine-owned conversation threads, and keyed signals.
  AWF coordinates black-box agents rather than implementing Anthropic's dynamic
  subagent planner.
- Huang et al., [Large Language Models Cannot Self-Correct Reasoning Yet][selfcorrect]:
  one reason AWF makes evaluation external to the generator.
- Xu et al., [Pride and Prejudice: LLM Amplifies Self-Bias in Self-Refinement][selfbias]:
  why self-refinement is not the same as independent review.
- Panickssery et al., [LLM Evaluators Recognize and Favor Their Own Generations][selfpref]:
  why model self-preference matters when designing a judge.
- Zheng et al., [SkillRouter: Skill Routing for LLM Agents at Scale][skillrouter]:
  the foundation for AWF's native skill-routing primitive. AWF adopts the
  paper's lesson that routing needs the full skill body, not only names or
  frontmatter; BM25 is the default plug-and-play router so local skill libraries
  start useful immediately, while model-backed routing remains a stronger option
  when teams want to trade cost and complexity for accuracy.

[anthropic]: https://www.anthropic.com/engineering/building-effective-agents
[contexteng]: https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents
[multiagent]: https://www.anthropic.com/engineering/multi-agent-research-system
[skillrouter]: https://arxiv.org/abs/2603.22455
[selfcorrect]: https://arxiv.org/abs/2310.01798
[selfbias]: https://arxiv.org/abs/2402.11436
[selfpref]: https://arxiv.org/abs/2404.13076
