# Claude Code Readiness Fixture Design

**Date:** 2026-07-15
**Status:** Approved
**Purpose:** Public Day-0 readiness gate for the AWF external-adoption test

## Problem

AWF v0.5.1 supports `anthropic/claude-code`, independent gates, typed outputs, and the native backend, but the public onboarding path does not demonstrate those capabilities together. The current gated example depends on Ollama, the README install snippet still selects v0.1.0, and the gate-scoped output lookup planned for the adoption test is incorrect.

The adoption clock must not start until a developer with an existing Claude Code subscription can follow public documentation, create an explicit long-lived subscription token with `claude setup-token`, and complete one clean-room gated run using the published v0.5.1 binary, without Docker, Ollama, a second model provider, or runtime changes.

## Chosen Approach

Add one committed public Claude Code fixture and correct the adjacent README instructions.

The fixture uses:

- one `anthropic/claude-code` generator with `bare: false`, receiving an operator-generated `CLAUDE_CODE_OAUTH_TOKEN` through AWF's `--agent-env` allowlist while the per-run Claude config remains isolated;
- one deterministic POSIX-shell evaluator, so onboarding costs one model call and cannot fail because a second model makes a stochastic judgment;
- one engine-owned `gate` with `max_attempts: 1`;
- a static digest-pinned container declaration required by the current Claude adapter contract;
- explicit `awf run --backend native`, which runs the host Claude Code binary and does not invoke Docker;
- a tiny typed generator output promoted to an artifact for the evaluator;
- a typed `{approved, feedback}` evaluator verdict used by `until`.

This fixture proves the public binary can launch an unchanged Claude Code CLI, enforce a typed output, stage the committed artifact into an independent evaluator, apply a gate verdict, and expose the accepted generator output through the runtime-addressed `awf outputs --step` path. It does not attempt to prove repair behavior, model quality, Docker execution, or production workflow value.

## Files and Scope

Only these public artifacts may change:

1. `examples/claude-code-gated/workflow.yaml` — the clean-room workflow.
2. `examples/claude-code-gated/README.md` — prerequisites, exact commands, expected success signals, cleanup, and troubleshooting.
3. Root `README.md` — correct the release version and link to the Claude Code fixture from the quickstart/adapters material.

No Go source, runtime behavior, adapter behavior, schemas, release tag, packaging, or existing example is changed. The fixture is tested against the downloaded v0.5.1 Darwin arm64 release binary, not a source build. The fixture/documentation commit is recorded separately from the immutable v0.5.1 runtime tag.

## Workflow Contract

The generator prompt requests the exact harmless value `{"ready": true}` under a one-property JSON schema whose only field is the required boolean `ready`; additional properties are rejected. The generator declares `output_artifact`, allowing the evaluator to receive the canonical typed JSON as an `input_files` artifact rather than interpolating model text into a shell command.

The evaluator reads only that staged artifact, removes JSON-insignificant ASCII whitespace with POSIX `tr`, and compares the result with the constant `{"ready":true}`. It writes `{"approved":true,"feedback":""}` to `$AWF_OUTPUT` on a match and a typed false verdict with a fixed non-secret feedback string on a mismatch. It exits mechanically only if the evaluator itself cannot run; a content mismatch produces `approved: false`, allowing the gate to reject honestly.

The accepted output is retrieved with its full runtime address:

```sh
awf outputs <run-id> --step gate[0].attempt-1.generate.draft
```

The example never claims that plain `awf outputs <run-id>` exposes a gate-internal generator output.

## User Flow

1. Confirm `claude --version` succeeds and the operator has a Claude subscription.
2. In the operator's private terminal, run `claude setup-token` and follow its instructions to set `CLAUDE_CODE_OAUTH_TOKEN` in that shell. The token is never pasted into chat, printed in captured logs, written to the workflow, or committed to disk.
3. Download the v0.5.1 archive for the operator's OS/architecture and verify it against the published checksum.
4. Put the extracted `awf` binary on a temporary PATH; installation into `/usr/local/bin` is optional.
5. From the pinned fixture commit, run `awf validate examples/claude-code-gated/workflow.yaml`.
6. In the same private shell, run `awf run --backend native --agent-env CLAUDE_CODE_OAUTH_TOKEN examples/claude-code-gated/workflow.yaml`, then unset `CLAUDE_CODE_OAUTH_TOKEN` when the run ends.
7. Record the run id, confirm the final status is `ok`, and retrieve the generator's typed output with the documented runtime address.

The example explains why `--backend native` is mandatory: the Claude adapter is container-backed in the format contract, so automatic backend selection sees the static image and otherwise chooses Docker. Under the native backend, the declared image is intentionally not used.

## Failure Handling

- Missing `claude`: stop with a prerequisite failure.
- Missing `CLAUDE_CODE_OAUTH_TOKEN`: stop and tell the operator to run `claude setup-token` in a private terminal; the host's normal Claude Code login is intentionally insufficient because AWF isolates `CLAUDE_CONFIG_DIR` per run.
- Token rejected or expired: stop, unset it, and repeat `claude setup-token`; never fall back to copying the host Claude config into AWF's isolated run.
- Missing native sandbox support: report the existing AWF native-backend error; do not weaken sandboxing.
- `auto` or Docker selected: point back to the explicit native command; do not create a custom image containing credentials or Claude Code.
- Typed output or gate failure: preserve the run id and output, classify the readiness gate as failed, and do not start adoption outreach.
- v0.5.1 cannot validate or execute the fixture as documented: classify the result as product-readiness failure. Do not change runtime code inside this test.

## Verification

The implementation is complete only when all of the following pass:

1. The downloaded v0.5.1 archive matches the published checksum.
2. That extracted binary reports version v0.5.1.
3. The fixture validates with that binary.
4. A live subscription-authenticated run succeeds on Darwin arm64 with `--backend native --agent-env CLAUDE_CODE_OAUTH_TOKEN`, executed by the operator without exposing the token to the automation transcript.
5. The documented runtime-addressed output command returns `{"ready": true}` semantically.
6. The root README contains no stale `VERSION=0.1.0` install instruction.
7. `make lint test` passes after the documentation/example changes.
8. `git diff` contains no runtime or unrelated changes.

## Explicit Non-Goals

- No runtime fix for the container/native backend ergonomics.
- No automatic backend-selection change.
- No Docker image for Claude Code.
- No Ollama setup.
- No second Claude evaluator.
- No repair-loop demonstration.
- No token capture, credential-file copying, or weakening of AWF's per-run Claude config isolation.
- No marketing claims or adoption-clock start merely because the founder's machine passes.
