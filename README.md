# AWF: Agentic Workflow Format

A runtime for **agentic pipelines**: you write author-defined control flow whose steps are
black-box agent CLIs (such as Anthropic's Claude Code or Factory's droid) and shell commands, run
against long-lived containers, with an independent judge (the **gate**) checking every stage. It is
one Go binary, `awf`.

Think of it as **TDD for agent workflows**: you write the acceptance check, the runtime runs it,
and a stage advances only when the check passes. The agent never marks its own homework.

## Why it exists

Nondeterministic agent steps report success they didn't achieve. A coding agent says the tests pass
when they actually error out; a data job reports a clean migration after quietly dropping rows; a
research step cites a source that doesn't say what it claims. The agent usually can't catch this
itself: Huang et al. found that large language models ["cannot self-correct reasoning
yet"][selfcorrect] without external feedback, and sometimes get *worse* when they try.

So correctness has to be checked from the outside. Anthropic's ["Building Effective
Agents"][anthropic] describes the **evaluator-optimizer** workflow: one model generates, a second
evaluates and feeds critique back in a loop. AWF makes that pattern a first-class primitive (the
gate) and adds the guarantee a self-grading loop can't have: the evaluator is *structurally
independent* of the generator (a fresh context, or a deterministic check). The verdict is never the
generator's own self-report. That matters: models favor their own outputs [selfpref], and
self-refinement *amplifies* that bias instead of fixing it [selfbias], so an independent critic
(ideally a different model) checks the work far more reliably than the generator can check itself.

Everything else in AWF exists to serve that check: long-lived containers (the test needs a real
system to run against), typed outputs (so the check reads validated fields, not fragile text), and
content-addressed checkpoint/resume (so an expensive agent run is never redone after a crash).

## How it compares

Most agentic runtimes converge on the same shape: the framework calls the model **in-process**, run
state is a **mutable snapshot**, quality is whatever check the author wires up, and a changed
definition is silently applied on the next run. AWF makes a different set of bets — it treats the
agent as a **black box it never runs**, makes an **independent judge** the engine's job, commits
**content-addressed artifacts**, and **hard-fails** rather than silently adapt when the definition or
runtime drifts.

This is positioning across categories, not a scorecard. A few rows are deliberately *different
categories* — included for orientation, not as head-to-head peers (see the notes below).

| | How the agent runs | Independent runtime gate | Durable resume | On definition drift | Infra |
| --- | --- | --- | --- | --- | --- |
| **AWF** | wraps any agent CLI as a black box (`claude-code`, `droid`, `codex`) or one HTTP LLM call, in a container | **engine-enforced** — a fresh-context judge repairs against its critique; a crash never counts as a rejection | **content-addressed** — replays committed steps, re-runs only the frontier | **hard-fails** — definition digest + resolved runtime version are pinned | long-lived digest-pinned containers/compose; single-host |
| Claude Code `/workflows` | Claude subagents, in-process (Claude-only) | none enforced — quality is a pattern Claude *codes into the script* | none across sessions — resume is session-only, lost on exit | latest-wins | in-process, single-host |
| LangGraph | in-process Python nodes; calls the model directly (any provider) | none enforced — hand-wired evaluator nodes | state snapshot per super-step, thread-keyed (not content-addressed) | latest-wins ("latest graph applied to every thread") | in-process (the Platform packages the orchestrator in Docker) |
| CrewAI | in-process role-based crews + flows | none independent — a task guardrail uses the *same* agent's LLM, in-band | `@persist` state snapshot (SQLite) | latest-wins | in-process |
| OpenAI Agents SDK *(Swarm successor)* | in-process agent loop + handoffs | none — tripwire guardrails halt, they don't repair | none native — Sessions are memory; durability delegated to Temporal | n/a (delegated) | in-process |
| Pydantic AI | in-process typed agent loop | none in-graph — Pydantic Evals is offline/CI | delegated to Temporal/DBOS (replay / DB-checkpoint) | delegated to the host | in-process (+ Temporal/DBOS) |
| Temporal *(durable-execution yardstick)* | model-agnostic infra; the agent/LLM call lives in an Activity | none | event-sourced deterministic replay — the gold standard | hard-fails on nondeterminism; opt-in versioning APIs | **distributed** (service + worker fleet) |
| Eval / judge tools *(promptfoo, LangSmith, Braintrust)* | n/a — they grade outputs, they don't orchestrate work | independent judges exist — but offline eval, CI-merge gates, or online monitoring, never in-graph | n/a | n/a (they detect regressions across versions) | CLI / CI / SaaS |
| DAG orchestrators *(Airflow, Prefect, Dagster)* | n/a — the model lives in your task code | none (data-quality checks ≠ an LLM judge) | task-state snapshot in a metadata DB (task-granular) | mostly recompute or version-record | distributed scheduler + workers |

A few caveats so the table reads fairly:

- **Temporal is the yardstick, not a target.** AWF is single-host and explicitly does *not* offer
  distributed, durable-execution-grade exactly-once guarantees; it checkpoints to *skip* finished
  work, not to distribute it. Temporal's event-sourced replay is the reference standard for "durable
  resume" — AWF's content-addressed commit is a different mechanism with a narrower scope, not a
  larger one.
- **Eval tools and DAG orchestrators are different jobs.** Independent LLM judges already exist
  (promptfoo, DeepEval, LangSmith, Braintrust) — but they run *offline* in CI or *monitor* live
  traffic; Braintrust will even block a PR merge below a score threshold. None of them sit *inside* a
  running workflow deciding whether the next stage executes and feeding the critique back into a
  regenerate loop. That in-graph generate → independent-evaluate → repair loop is what AWF's gate is.
  Likewise, AWF is not a data-pipeline scheduler.
- **Closest on a single axis.** On *drift*, Restate (immutable versioned deployments + fail-loud on
  mismatch) and DBOS (app-version pinning) are the nearest external analogs to AWF's pin-and-fail
  invariant — they just pin in-process code rather than a workflow definition plus a container
  digest. On *typed outputs*, Pydantic AI is the closest mirror.

The through-line: every other runtime here calls the model itself and trusts the author (or an
offline tool) to check the result. AWF wraps the model as a black box, and makes checking it the
engine's job.

## Getting started

Requirements: Go 1.26+, and Docker (optional) for the isolated container backend.

```sh
make build      # builds ./bin/awf
make man        # optional: build the man pages (then: man ./man/awf.1, man ./man/awf-workflow.5)
```

## A first workflow

Three stages that compose into one task: answer a customer support ticket safely. An agent gathers
the customer's account context, a second agent drafts a reply, and an *independent* LLM judge checks
that reply against the account and your policy before it is ever sent. The judge is the gate, so the
agent that wrote the reply never decides whether it is safe to send.

```yaml
workflow: support-reply
version: 1

input:
  type: object
  required: [ticket_id]
  properties:
    ticket_id: { type: string }

containers:
  desk:
    image: oci://registry.example.com/support-runner@sha256:...   # digest-pinned, not a tag

graph:
  # stage 1: gather the customer's account + order context and the relevant policy
  - id: gather
    container: desk
    uses: anthropic/claude-code
    with: { skill: support-context, ticket: "{{ input.ticket_id }}" }
    output_files: [/out/context.json]
    output_schema:
      type: object
      additionalProperties: false
      required: [context_path]
      properties: { context_path: { type: string } }

  # stage 2: draft a reply, judged for accuracy + policy by an independent LLM, repaired until it passes
  - gate:
      generate:
        - id: draft
          container: desk
          uses: anthropic/claude-code
          with: { skill: support-reply, context: "{{ step.gather.context_path }}" }
          output_files: [/out/reply.md]
          output_schema:
            type: object
            additionalProperties: false
            required: [reply_path]
            properties: { reply_path: { type: string } }
      evaluate:
        # fresh context: sees the customer context and the draft, never the writer's reasoning
        - id: judge
          container: desk
          uses: anthropic/claude-code
          with: { skill: support-reply-judge, context: "{{ step.gather.context_path }}", reply: "{{ step.draft.reply_path }}" }
          output_schema:
            type: object
            additionalProperties: false
            required: [accurate, on_policy, promises_unentitled_refund, answers_the_question, on_brand_tone, feedback]
            properties:
              accurate:                   { type: boolean }
              on_policy:                  { type: boolean }
              promises_unentitled_refund: { type: boolean }
              answers_the_question:       { type: boolean }
              on_brand_tone:              { type: boolean }
              feedback:                   { type: string }
      until: "{{ evaluate.accurate && evaluate.on_policy && !evaluate.promises_unentitled_refund && evaluate.answers_the_question && evaluate.on_brand_tone }}"
      max_attempts: 4

  # stage 3: post the approved reply to the help desk, idempotently (never double-send)
  - id: send
    container: desk
    run: ./send-reply.sh "{{ input.ticket_id }}" /out/reply.md
    idempotency_key: "{{ input.ticket_id }}:reply"
```

The data flow (three stages, with the gate's repair loop in the middle):

```mermaid
flowchart TD
    ticket["input: support ticket"]
    gather["stage 1 gather<br/>(claude-code agent)"]
    ctx[("customer context + policy")]
    draft["stage 2 generate: draft reply<br/>(claude-code agent)"]
    judge["evaluate: independent judge<br/>(fresh context: context + draft only)"]
    verdict{"until: accurate and on-policy and no<br/>unentitled-refund and answers the<br/>question and on-brand tone?"}
    send["stage 3 send: post reply<br/>(idempotency_key)"]
    sent(["reply posted to the customer"])

    ticket --> gather
    gather --> ctx
    ctx --> draft
    ctx --> judge
    draft --> judge
    judge --> verdict
    verdict -->|"true"| send
    verdict -->|"false: feedback fed back (up to max_attempts)"| draft
    send --> sent
```

Run it:

```sh
bin/awf validate ./support-reply.yaml              # parse + check it is well-formed
bin/awf run --backend docker ./support-reply.yaml  # execute; --backend docker so it can resume
```

Why this is more than a "have an agent answer tickets" script: the middle stage is an independent
LLM judge that re-reads the draft against the customer's actual account and your policy, making the
call no keyword check can (is every claim accurate, does it quietly promise a refund the customer is
not entitled to, does it answer the question, is the tone on-brand). The agent that wrote the reply
never decides whether it is safe to send; a fresh-context judge does, and on a fail its findings
feed back so the next draft is conditioned on the critique, up to `max_attempts`. The three stages
compose through typed outputs and the shared workspace, the approved reply is posted idempotently so
a retry never double-sends, and each committed stage is checkpointed so a crash never re-pays for
finished agent work.

### Two ways turns share data

AWF gives an agent step two distinct channels for what a later turn sees:

- **Typed references** — `output_schema` makes a step's result a validated,
  structured value; a later step binds a field with `{{ step.draft.summary }}`.
  References are inspectable, type-checked, and the only thing a gate's `until:`
  can read. Use them for facts that flow forward.
- **Engine-owned conversation threads** — `continues: <prior-step-id>` re-sends
  the prior turns' verbatim `(user, assistant)` exchanges to the model. The
  thread is assembled by the engine from each prior turn's committed transcript;
  the author never inlines a `messages:` array, and `with:` stays opaque. The
  thread is generator-only: it is **not** a bindable reference and is invisible
  to `until:` and templates. Use it when the model needs the actual prior
  dialogue, not just extracted fields.

The two compose: a gated turn can both `continues:` a prior turn and read the
evaluator's typed feedback. See `examples/llm-conversation*` for linear, gated,
and fan-out chains. (Branches that fan out and share a `system_prompt` re-send a
byte-identical prefix, which server-side prefix caching reuses.) `continues:` is
`awf/llm`-first; each thread is committed to the content-addressed log and
replayed verbatim on resume.

## Supported agents

An agent step names its runtime with `uses:`; that runtime's per-step config lives in the opaque
`with:` map, which only the named adapter reads and validates. Three black-box CLI agents ship behind
the uniform adapter seam today, and you can mix them within one workflow — e.g. draft with one and
have a *different* model judge it, which makes the gate's independence stronger:

- **`anthropic/claude-code`** — Anthropic's Claude Code. The reference adapter.
- **`factory/droid`** — Factory's [droid](https://docs.factory.ai/cli/droid-exec/overview) (its
  `droid exec` non-interactive mode).
- **`openai/codex`** — OpenAI's `codex` CLI (`codex exec`). The **first native-schema** non-Claude
  adapter: typed output is constrained API-side via `codex exec --output-schema` (OpenAI Responses
  structured output), so the adapter never parses free text for the answer. Live output is
  **event-granular** — tool calls and reasoning steps appear as they happen, but the final answer text
  arrives in one block (see the `awf(1)` streaming note). Conforms to conformance Bucket 14.
- **`awf/llm`** — The first non-CLI, **containerless** adapter. Instead of wrapping a black-box
  CLI in a container, it issues a single streaming HTTP call directly against any OpenAI-compatible
  Chat Completions endpoint — OpenAI, Ollama, vLLM, llama.cpp, LM Studio, LiteLLM/Bifrost gateways
  — by config alone, with no `containers:` block needed. OpenAI-compatible HTTP is the lingua franca:
  the same `with:` surface reaches cloud models and local models (e.g. `ollama_format` for
  Ollama-native, `response_format` for OpenAI-compat). Token deltas stream live, character-by-character,
  like every other adapter. See [`examples/awf-llm-ollama/`](examples/awf-llm-ollama/) for a runnable
  local-model bundle and the `awf(1)` ENVIRONMENT section for the full `with:` reference.

droid `model` IDs are provider-prefixed and versioned per family — e.g. `claude-sonnet-4-6` (Claude
uses dashes), `gpt-5.5` (GPT/Gemini use dots), `gemini-3.5-flash`; the default is `claude-opus-4-8`.
AWF doesn't keep its own model list (it drifts per droid release), so an unknown ID fails the step at
launch with droid's available-models list in the error — run `droid exec --model x` to print the set
your installed droid accepts.

droid's **bring-your-own-key (BYOK)** models work too — any provider (OpenRouter, Fireworks, a
self-hosted LiteLLM/vLLM gateway, local Ollama, or a native Anthropic/OpenAI endpoint). You declare
the endpoint in the step's `with:` (`base_url` / `api_key_env` / `provider`, plus a `tls_insecure`
escape hatch); the adapter writes a per-invocation `--settings` file from those keys, so one image
serves every provider with **no** baked config. The literal key never enters the workflow —
`api_key_env` names a host var (forward it via the workflow `env:` field or `--agent-env`). No
`FACTORY_API_KEY` is needed in BYOK mode. See the `awf(1)` ENVIRONMENT section for the full key
reference and [`examples/droid-byok/`](examples/droid-byok/) for a runnable, provider-agnostic bundle.

All four launch one fresh invocation per step (so the gate's evaluator stays structurally
independent — no session reuse), stream their events live, validate `with:` strictly, and bind
typed outputs to the step's `output_schema`.

| Capability | `anthropic/claude-code` | `factory/droid` | `openai/codex` | `awf/llm` |
| --- | --- | --- | --- | --- |
| Maturity | reference adapter | supported | supported | supported |
| Containerless (no `container:` needed) | ❌ | ❌ | ❌ | ✅ |
| Live streaming (realtime events) | ✅ | ✅ | ✅ event-granular <sup>4</sup> | ✅ token-by-token |
| Typed outputs (`output_schema`) | ✅ native (`--json-schema`) | ✅ layer-2 <sup>1</sup> | ✅ native (`--output-schema`) <sup>5</sup> | ✅ layer-2 <sup>6</sup> |
| Gate repair (critique fed back) | ✅ | ✅ | ✅ | ✅ |
| Session independence (no reuse) | ✅ | ✅ | ✅ (`--ephemeral`) | ✅ (stateless HTTP) |
| Token-usage metrics | ✅ | ✅ | ✅ | ✅ |
| Cost (USD) reporting | ✅ (`total_cost_usd`) | ❌ tokens only <sup>2</sup> | ❌ tokens only | ❌ tokens only |
| Model selection | ✅ `model` | ✅ `model` | ✅ `model` | ✅ `model` |
| Reasoning effort | ➖ | ✅ `reasoning_effort` | ✅ `reasoning_effort` | ➖ |
| Autonomy / sandbox level | ➖ | ✅ `autonomy` | ✅ `sandbox` | ➖ |
| System-prompt append | ✅ `system_prompt` | ✅ `system_prompt` | ➖ | ✅ `system_prompt` |
| Tool gating | ✅ `allowed_tools` | ✅ `enabled_tools` / `disabled_tools` | ➖ | ➖ |
| Budget cap | ✅ `max_budget_usd` | ➖ | ➖ | ➖ |
| Local model support | ➖ | ✅ BYOK | ➖ | ✅ Ollama/vLLM/llama.cpp/LM Studio |
| Real-binary conformance | ✅ native + Docker (14a + gate-e2e 14c) | ⚠️ native (14a verified e2e); compose gate-e2e (14c) deferred <sup>3</sup> | ✅ native (14a, `AWF_CODEX_LIVE`) | fake transport (fake-RoundTripper; no network required) |
| Auth env var(s) | `ANTHROPIC_API_KEY` · `ANTHROPIC_AUTH_TOKEN` · `CLAUDE_CODE_OAUTH_TOKEN` | `FACTORY_API_KEY` | `OPENAI_API_KEY` · `CODEX_HOME` | `OPENAI_API_KEY` (or custom via `api_key_env`) |

Legend: ✅ full · ⚠️ partial · ➖ not applicable · ❌ not supported.

1. droid has no native schema flag, so the adapter injects the `output_schema` into the prompt and
   parses/validates droid's final JSON. The single-agent typed-output round-trip is **verified
   end-to-end against the real `droid` binary**; output that doesn't match the schema is a retryable
   failure the gate repairs.
2. droid reports token counts but no dollar figure, so `MetricCost` is left zero.
3. Bucket 14a (typed-output round-trip) and live streaming are verified end-to-end against the real
   `droid` binary; bucket 14c (gate repair end-to-end under a compose lab) needs a compose image with
   `droid` installed and is deferred. The gate itself is engine-enforced and adapter-agnostic, so it
   works for both runtimes regardless.
4. codex's `exec --json` is event-granular, not token-granular — tool calls and reasoning steps appear
   as they happen, but the final answer text arrives in one block. The typed output is functionally
   identical to the other adapters; only the live terminal UX differs.
5. codex's `--output-schema` constrains the model's response via OpenAI Responses structured output
   (API-side), so the answer text is always conforming JSON. The adapter never falls back to layer-2
   free-text parsing. An `output_schema` for a codex step must be `additionalProperties: false` with
   all properties `required` (recursively), or codex fails it permanently — treat `awf validate`'s
   **AWF2002** warning as blocking for codex steps.
6. `awf/llm` uses `structured_output: response_format` (OpenAI strict JSON schema) as the default
   optimization and `ollama_format` (Ollama native `/api/chat` `format` field) for Ollama-native
   endpoints. Layer-2 parsing is the fallback and the final contract: the engine always re-validates
   the parsed output against `output_schema`. An `output_schema` for `awf/llm` with
   `structured_output: response_format` against OpenAI must be `additionalProperties: false` with all
   properties `required` (recursively) — treat **AWF2002** as blocking for those steps.

During a run, agent steps stream a readable live view to stderr: assistant text
in full, reasoning dimmed, tool calls as one-liners, and tool results collapsed
to a status + size with a head **and tail** preview (the full output is preserved
in the run journal, not the terminal). Output is plain (no color) when piped or
under `NO_COLOR`.

> **droid opsec.** The adapter disables Factory telemetry (`OTEL_SDK_DISABLED` /
> `OTEL_CUSTOMER_ENABLED`) inside the container, but droid's `cloudSessionSync` mirrors session
> content to Factory's web app and is **on by default with no env switch** — for sensitive
> workflows, disable it in the container image's `~/.factory/settings.json`
> (`{"general":{"cloudSessionSync":false}}`). See the `awf(1)` ENVIRONMENT section.

> **codex opsec.** The adapter always passes `--ephemeral` (no session persistence) and
> `--skip-git-repo-check`. By default it also passes `--dangerously-bypass-approvals-and-sandbox`
> (the AWF container is the isolation boundary; codex's approval prompts would block a
> non-interactive run). A `sandbox:` `with:` key opts into codex's internal sandbox and removes the
> bypass flag. The adapter enables no MCP servers. See the `awf(1)` ENVIRONMENT section for the
> `output_schema` structured-output floor and the MCP/tool caveat.

> **awf/llm network stance.** Omitting `base_url` silently routes to `https://api.openai.com/v1`
> — set it explicitly when targeting a local or private endpoint. `tls_insecure: true` skips TLS
> certificate verification; use only for internal/self-signed endpoints and accept the interception
> risk. Proxies need no config: Go's HTTP transport honors `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`
> automatically. The API key is sent as an `Authorization: Bearer` header and never written to the
> journal.

## Documentation

- **`awf(1)`** ([`man/awf.1.md`](man/awf.1.md)): the command reference, covering what each command
  does, its flags, exit codes, and examples.
- **`awf-workflow(5)`** ([`man/awf-workflow.5.md`](man/awf-workflow.5.md)): the workflow-format
  reference, covering top-level fields, the three step types, control flow and the gate, templating,
  and checkpoint/resume.
- [`docs/runtime-design.md`](docs/runtime-design.md): the implementation design.

## Further reading

- Anthropic, [*Building Effective Agents*][anthropic] (2024): the evaluator-optimizer pattern AWF
  builds in as the gate.
- Huang et al., [*Large Language Models Cannot Self-Correct Reasoning Yet*][selfcorrect] (ICLR
  2024): why the evaluator has to be independent of the generator.
- Xu et al., [*Pride and Prejudice: LLM Amplifies Self-Bias in Self-Refinement*][selfbias] (2024):
  self-critique amplifies a model's own bias; external feedback is what reduces it.
- Panickssery et al., [*LLM Evaluators Recognize and Favor Their Own Generations*][selfpref] (2024):
  models recognize and prefer their own outputs, so an independent (ideally different-model) judge
  is a less biased check.

[anthropic]: https://www.anthropic.com/research/building-effective-agents
[selfcorrect]: https://arxiv.org/abs/2310.01798
[selfbias]: https://arxiv.org/abs/2402.11436
[selfpref]: https://arxiv.org/abs/2404.13076
