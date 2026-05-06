# AWF — Agentic Workflow Format

A runtime for **agentic pipelines**: you write author-defined control flow whose steps are
black-box agent CLIs (such as Claude Code) and shell commands, run against long-lived containers,
with an independent judge — the **gate** — checking every stage. It is one Go binary, `awf`.

Think of it as **TDD for agent workflows**: you write the acceptance check, the runtime runs it,
and a stage advances only when the check passes — the agent never marks its own homework.

## Why it exists

Nondeterministic agent steps report success they didn't achieve. A coding agent says the tests pass
when they actually error out; a data job reports a clean migration after quietly dropping rows; a
research step cites a source that doesn't say what it claims. The agent usually can't catch this
itself: Huang et al. found that large language models ["cannot self-correct reasoning
yet"][selfcorrect] without external feedback — and sometimes get *worse* when they try.

So correctness has to be checked from the outside. Anthropic's ["Building Effective
Agents"][anthropic] describes the **evaluator-optimizer** workflow — one model generates, a second
evaluates and feeds critique back in a loop. AWF makes that pattern a first-class primitive (the
gate) and adds the guarantee a self-grading loop can't have: the evaluator is *structurally
independent* of the generator (a fresh context, or a deterministic check). The verdict is never the
generator's own self-report.

Everything else in AWF exists to serve that check: long-lived containers (the test needs a real
system to run against), typed outputs (so the check reads validated fields, not fragile text), and
content-addressed checkpoint/resume (so an expensive agent run is never redone after a crash).

## Getting started

Requirements: Go 1.26+, and Docker if you want the isolated container backend.

```sh
make build                              # builds ./bin/awf
bin/awf validate ./pipeline.yaml        # parse + validate, print the definition digest
bin/awf run --backend docker ./pipeline.yaml   # run it (use --backend docker to allow resume)
```

Read the manual:

```sh
make man                 # generate the man pages from man/*.md
man ./man/awf.1          # the command reference
man ./man/awf-workflow.5 # the workflow-format reference
```

## Documentation

- **`awf(1)`** ([`man/awf.1.md`](man/awf.1.md)) — the command reference: what each command does, its
  flags, exit codes, and examples.
- **`awf-workflow(5)`** ([`man/awf-workflow.5.md`](man/awf-workflow.5.md)) — the workflow-format
  reference: top-level fields, the three step types, control flow and the gate, templating, and
  checkpoint/resume.
- [`docs/runtime-design.md`](docs/runtime-design.md) — the implementation design.

## Further reading

- Anthropic, [*Building Effective Agents*][anthropic] (2024) — the evaluator-optimizer pattern AWF
  builds in as the gate.
- Huang et al., [*Large Language Models Cannot Self-Correct Reasoning Yet*][selfcorrect] (ICLR 2024)
  — why the evaluator has to be independent of the generator.

[anthropic]: https://www.anthropic.com/research/building-effective-agents
[selfcorrect]: https://arxiv.org/abs/2310.01798
