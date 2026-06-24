# Contributing to awf

Thanks for your interest in `awf`. This document is the contributor bar — the
same gate the maintainers hold themselves to.

`awf` is the reference runtime for the Agentic Workflow Format: a single Go 1.26
CLI binary (`awf`) that orchestrates black-box agent CLIs and shell commands
against long-lived containers, with an independent gate checking every step and
content-addressed checkpoint/resume.

## Ground rules

1. **Ask, don't assume.** If intent, architecture, or a requirement is unclear,
   ask before writing code.
2. **Simplest solution first.** Implement the simplest thing that works. Don't
   add abstractions, interfaces, or flexibility that weren't requested — the
   codebase keeps seams only where behavior genuinely varies.
3. **Don't touch unrelated code.** Keep a change scoped to its task.
4. **Flag uncertainty explicitly.** Say so when you're unsure rather than
   projecting false confidence.

## The format is the contract

The man pages are the stable reference, and the format reference outranks the
code:

- [`awf-workflow(5)`](man/awf-workflow.5.md) — the workflow-format reference:
  fields, control flow, templating, typed outputs, the gate, and
  checkpoint/resume. A code change that contradicts it is wrong; the format must
  be revised, explicitly and separately, first.
- [`awf(1)`](man/awf.1.md) — the command reference: flags, exit status,
  environment, and tracing.
- [`README.md`](README.md) — what AWF is and why. [`ROADMAP.md`](ROADMAP.md) —
  build order and scope.

When CLI or format behavior changes, update the man-page source (`man/*.md`) in
the same change.

## Development workflow

- **Test-driven, red-green-refactor.** Write the failing test before the code
  that satisfies it.
- **The conformance suite is the definition of done.** New durability or gate
  behavior needs a conformance test, and it must run against the **fake backend**
  (no Docker required). Keep it green at every step.
- Keep packages single-purpose with narrow interfaces; split a file when it
  grows broad.

## The pre-commit bar

Run this before every commit — and note it is `make lint test`, **not** a bare
`go test`/`go vet`/`gofmt`:

```sh
make lint test
```

`make lint` runs `golangci-lint`, which catches issues (`errcheck`,
`staticcheck`, …) that `go vet` does not. CI runs `make lint test build` plus the
Docker integration suite and fails on any of them. Install `golangci-lint`
locally so you match CI:

```sh
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.12.2/install.sh \
  | sh -s -- -b "$(go env GOPATH)/bin" v2.12.2
```

Other useful targets:

```sh
make build        # build ./bin/awf
make integ        # Docker/native integration suite; spends no API money
make man          # regenerate the troff man pages from man/*.md
```

`make integ-live` (the real-agent tier that may spend API money) is local-only
and is never run by CI.

## Scope

`awf` is single-host by design. Checkpointing skips work; it does not distribute
it. Distributed dispatch, multi-tenancy, durable-execution guarantees, and
saga/compensation machinery are explicitly out of scope.

## Pull requests

- Branch off `main`; keep the PR focused.
- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit and
  PR titles (`feat:`, `fix:`, `chore:`, `docs:`, …) — release notes are grouped
  from them.
- Make sure `make lint test build` is green and the conformance suite passes.

## Security

Please report vulnerabilities privately — see [SECURITY.md](SECURITY.md). Do not
open a public issue for a suspected security problem.
