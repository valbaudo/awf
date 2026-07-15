# Compatibility and stability

`awf` exposes a machine-facing contract for driving workflows from code. This
document is the stability promise for that contract. It is versioned as
**Contract v1**, tracked separately from the `awf` binary version.

## The binary is pre-1.0; the contract is not

The `awf` binary follows [Semantic Versioning](https://semver.org) and is still
`0.x` (see [SECURITY.md](SECURITY.md)) — under SemVer, a `0.x` binary makes no
stability guarantee, and you should upgrade to the latest `v0.x`. That caveat is
about the *binary*. The *machine contract* below carries its own promise on its
own version line, so "pre-1.0 binary" and "stable Contract v1" are not in
conflict: the surfaces listed as `stable` carry the promise in this document,
independent of the binary's `0.x` version. Contract v1 takes effect with `awf`
v0.5.0 — the first release to include this policy.

## The stability ladder

**Stable** — will not change incompatibly without a Contract-major bump (v2) and
a deprecation window (see below):

- **`awf run` invocation** — the flags documented in [awf(1)](man/awf.1.md):
  `--input` / `--input-file` / `--input-files` (run inputs), `--run-id`,
  `--state-dir`, `--backend`, and `--agent-env`, and their semantics. To learn a
  run's id from code, pass `--run-id`; do not parse the human `run <id>: <outcome>`
  line (see [Machine vs. human output](#machine-vs-human-output)).
- **Exit codes.** Most commands, including `awf run`, share one scheme: `0` ok,
  `1` a validation error or a run that terminated as a failure
  (`retryable_failure`, `permanent_failure`, or `rejected`), `2` a usage or
  precondition error, `3` an AWF-owned infrastructure failure. `awf outputs`
  deliberately uses its own *read-scoped* codes instead: `0` emitted, `1` not
  producible (the step committed no typed output, or validation failed), `2` bad
  invocation or state access/precondition failure (bad selector, digest mismatch,
  run not found, inaccessible state path, or no `outputs:` block) — it never
  returns `3`. See **EXIT STATUS** in [awf(1)](man/awf.1.md).
- **`awf outputs` JSON** — the typed-outputs object it prints, and its
  omit-on-absent rule: a field a run never produced (because its `if` branch or
  `loop` body was not taken) is omitted from the object and the command exits `0`,
  unless the workflow's `output_schema` marks that field `required`, in which case
  the read fails and it exits `1`.
- **The workflow document format** and its JSON Schema
  ([awf-workflow(5)](man/awf-workflow.5.md); the repo file
  `schema/workflow.v1.schema.json`).

**Experimental** — may change or disappear in any release; do not build a
contract on it:

- `awf trace --output json` — the OpenTelemetry span projection.
- Any future structured run-event / progress stream.

Any machine-facing surface not listed as `stable` above — including the
`--output json` of `awf ls`, `awf inspect`, and `awf graph` — is `experimental`
until this document classifies it.

## Contract-v1 safety correction

Code execution now defaults to one attempt: `run:` steps,
`react.tools[].impl.run`, and synthesized `reduce` executions are not repeated
unless the format exposes and the author sets a `retry:` policy with more
attempts. Agent `uses:` steps retain their eight-attempt transient-failure
default. This is an intentional Contract-v1 safety correction: silently
repeating an arbitrary shell command can duplicate external effects. Authored
`retry:` behavior is preserved, and every explicit field still overlays the
default for that step kind.

## Machine vs. human output

Only machine surfaces are the contract. A program driving `awf` reads the **exit
code** and the JSON from `awf outputs`. The human-facing output is *not*
versioned: the `stderr` progress stream, including retry notices, and the
`run <id>: <outcome>` line on `awf run`'s standard output. Retry notices include
the node path, failed/next/max attempt, cause, and wait duration for an operator;
the wait is cancellable, so a notice may not be followed by another dispatch. Do
not parse these notices. Read the exit code, and pass `--run-id` when you need to
know the run id up front.

## Versioning

- **The JSON Schema is versioned in its `$id`.** The schema's identifier carries
  its major — `…/workflow/v1.json` — so a consumer pins to a version. Additive
  changes keep the same major; a breaking change ships as a new major (`v2`)
  published alongside the retained `v1`. The `$id` host is provisional until
  confirmed (see the schema's own `$comment`); today the schema is the repo file
  `schema/workflow.v1.schema.json`.
- **The binary keeps SemVer.** This document states which binary versions
  implement a contract version (Contract v1 as of `awf` v0.5.0). If the binary
  reaches 1.0, the `stable` surfaces are exactly its SemVer public API.

## Deprecation

A `stable` surface slated for a breaking change is marked deprecated in the man
pages (and, where feasible, via a runtime `stderr` warning), kept for at least
the next binary-minor and never removed before the next Contract-major, then
removed only at that Contract-major with the replacement documented alongside.
