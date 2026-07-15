# Claude Code Readiness Fixture Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish and verify one clean-room Claude Code gated example against the downloaded AWF v0.5.1 Darwin arm64 binary without Docker, Ollama, or runtime changes.

**Architecture:** A Claude Code generator emits the typed value `{"ready": true}`. A deterministic shell evaluator receives the schema-validated boolean through AWF's typed-reference channel and emits the gate verdict through `$AWF_OUTPUT`. The evaluator uses one mechanical attempt so fixture failures surface immediately. The workflow is forced onto the native backend with an absolute `--state-dir`; documentation explains both the current image-declaration/native-backend constraint and the v0.5.1 macOS relative-scratch bug.

**Tech Stack:** AWF workflow YAML v1, `anthropic/claude-code`, POSIX `sh`, AWF v0.5.1 Darwin arm64 release, Markdown, npm-free Go repository checks through `make lint test`.

## Global Constraints

- Modify only `examples/claude-code-gated/workflow.yaml`, `examples/claude-code-gated/README.md`, and root `README.md` during implementation.
- Do not modify Go source, runtime behavior, adapter behavior, schemas, release tags, packaging, or existing examples.
- Verify with the downloaded public AWF v0.5.1 binary, never a source build.
- Use an operator-generated `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token` with `bare: false`; do not require an Anthropic API key or copy the host Claude config.
- Never expose the OAuth token in chat, captured tool output, logs, workflow YAML, Markdown, git, or the adoption ledger. The operator performs the authenticated run in a private terminal and reports only the sanitized result.
- Use exactly one Claude model call and a deterministic shell evaluator.
- Require explicit `--backend native`; do not invoke Docker or Ollama.
- On macOS v0.5.1, require an absolute `--state-dir "$PWD/.awf"`; do not disable the native sandbox to work around its relative-scratch bug.
- Stop before committing implementation if v0.5.1 cannot validate, run, clean up, and expose the fixture as documented.
- Do not start the 14-day adoption clock merely because the founder's machine passes.

---

## File Map

- `examples/claude-code-gated/workflow.yaml`: executable clean-room readiness fixture.
- `examples/claude-code-gated/README.md`: fixture prerequisites, v0.5.1 download, run/output commands, expected results, cleanup, and troubleshooting.
- `README.md`: correct current release version and link the public Claude Code fixture.

### Task 1: Prove the workflow against the public v0.5.1 binary

**Files:**
- Create: `examples/claude-code-gated/workflow.yaml`

**Interfaces:**
- Consumes: public AWF v0.5.1 Darwin arm64 archive; a Claude subscription; an operator-created `CLAUDE_CODE_OAUTH_TOKEN` held only in the operator's private shell.
- Produces: a validated and live-tested workflow whose accepted generator output lives at `gate[0].attempt-1.generate.draft`.

- [ ] **Step 1: Download and verify the immutable public release in an isolated directory**

Run:

```sh
RELEASE_DIR="$(mktemp -d)"
cd "$RELEASE_DIR"
VERSION=0.5.1
OS=darwin
ARCH=arm64
BASE="https://github.com/valbaudo/awf/releases/download/v${VERSION}"
curl -fLO "${BASE}/awf_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fLO "${BASE}/awf_${VERSION}_checksums.txt"
shasum -a 256 -c "awf_${VERSION}_checksums.txt" --ignore-missing
tar -xzf "awf_${VERSION}_${OS}_${ARCH}.tar.gz"
AWF_RELEASE="$RELEASE_DIR/awf_${VERSION}_${OS}_${ARCH}/awf"
"$AWF_RELEASE" version
```

Expected:

- checksum output contains `awf_0.5.1_darwin_arm64.tar.gz: OK`;
- the final command reports version `0.5.1`;
- `AWF_RELEASE` points outside the repository.

- [ ] **Step 2: Record the red state before creating the fixture**

Run from the repository root:

```sh
"$AWF_RELEASE" validate examples/claude-code-gated/workflow.yaml
```

Expected: non-zero exit with a missing-file error for `examples/claude-code-gated/workflow.yaml`.

- [ ] **Step 3: Create the minimal workflow**

Create `examples/claude-code-gated/workflow.yaml` with exactly:

```yaml
workflow: claude-code-gated-readiness
version: 1

containers:
  workspace:
    image: alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

graph:
  - gate:
      generate:
        - id: draft
          container: workspace
          uses: anthropic/claude-code
          retry: { attempts: 1 }
          with:
            bare: false
            max_turns: 1
            prompt: |
              This is a deterministic AWF readiness check.
              Return a JSON object whose only property is `ready` with boolean value `true`.
          output_schema:
            type: object
            additionalProperties: false
            required: [ready]
            properties:
              ready: { type: boolean }

      evaluate:
        - id: judge
          container: workspace
          retry: { attempts: 1 }
          run: |
            set -eu
            typed_ready="{{ step.draft.ready }}"
            mkdir -p "$(dirname "$AWF_OUTPUT")"
            if [ "$typed_ready" = 'true' ]; then
              printf '%s\n' '{"approved":true,"feedback":""}' > "$AWF_OUTPUT"
            else
              printf '%s\n' '{"approved":false,"feedback":"typed readiness value did not match"}' > "$AWF_OUTPUT"
            fi
          output_schema:
            type: object
            additionalProperties: false
            required: [approved, feedback]
            properties:
              approved: { type: boolean }
              feedback: { type: string }

      until: "{{ evaluate.approved }}"
      max_attempts: 1
```

- [ ] **Step 4: Validate with the public release binary**

Run:

```sh
"$AWF_RELEASE" validate examples/claude-code-gated/workflow.yaml
```

Expected: exit `0`, no validation errors, and no warning requiring a runtime or documentation change.

If validation fails because v0.5.1 does not accept the documented workflow contract, stop. Remove the uncommitted fixture, record product-readiness failure, and do not continue to Task 2.

- [ ] **Step 5: Have the operator run the live fixture with an explicit subscription token**

The operator—not the automation—opens a private terminal and runs:

```sh
cd /Users/vabbb/Documents/GitHub/AgentWorkflowFormat/.worktrees/awf-claude-readiness
claude --version
claude setup-token
```

The operator follows Claude Code's on-screen instructions to set `CLAUDE_CODE_OAUTH_TOKEN` in that same shell. The token is not pasted into chat or written into a command, file, log, workflow, or shell-history example. With the token present, the operator runs:

```sh
test -n "$CLAUDE_CODE_OAUTH_TOKEN"
/tmp/awf-readiness-v0.5.1-20260715/awf_0.5.1_darwin_arm64/awf run \
  --state-dir "$PWD/.awf" \
  --backend native \
  --agent-env CLAUDE_CODE_OAUTH_TOKEN \
  examples/claude-code-gated/workflow.yaml \
  2>&1 | tee /tmp/awf-claude-code-gated-run.log
unset CLAUDE_CODE_OAUTH_TOKEN
```

Expected:

- Claude Code reports its installed version;
- AWF reports `run <run-id>: ok`;
- exactly one Claude generator invocation appears;
- AWF's credential warning is absent because `CLAUDE_CODE_OAUTH_TOKEN` was explicitly forwarded;
- Docker and Ollama are never invoked;
- the deterministic evaluator succeeds on its single mechanical attempt.

Extract the run id without guessing:

```sh
RUN_ID="$(sed -n 's/^run \([^:]*\): ok$/\1/p' /tmp/awf-claude-code-gated-run.log | tail -1)"
test -n "$RUN_ID"
```

The operator reports only whether the command exited successfully and the non-secret run id. If token setup, the run, cleanup, or sandbox fails, stop. Preserve the sanitized run log, remove the uncommitted fixture, record product-readiness failure, and do not modify runtime code.

- [ ] **Step 6: Prove the gate-scoped typed output path**

Run:

```sh
"$AWF_RELEASE" outputs "$RUN_ID" \
  --state-dir "$PWD/.awf" \
  --step 'gate[0].attempt-1.generate.draft' \
  | tee /tmp/awf-claude-code-gated-output.json
tr -d '[:space:]' < /tmp/awf-claude-code-gated-output.json | grep -F '"ready":true'
```

Expected: exit `0` and output semantically containing `{"ready": true}`.

- [ ] **Step 7: Commit the proven fixture**

Run:

```sh
git add examples/claude-code-gated/workflow.yaml
git diff --cached --check
git commit -m "docs: add Claude Code gated readiness fixture"
```

Expected: one-file commit containing no runtime changes.

### Task 2: Document the exact public onboarding path

**Files:**
- Create: `examples/claude-code-gated/README.md`
- Modify: `README.md:174-203`
- Modify: `README.md:213-245`

**Interfaces:**
- Consumes: the live-proven workflow and its gate runtime address from Task 1.
- Produces: public instructions that reproduce the exact release verification and native Claude Code path.

- [ ] **Step 1: Record the documentation failures**

Run:

```sh
test -f examples/claude-code-gated/README.md
! rg -n 'VERSION=0\.1\.0' README.md
rg -n 'examples/claude-code-gated' README.md
```

Expected before editing:

- the first command fails because the example README is absent;
- the second command fails because root README still contains `VERSION=0.1.0`;
- the third command fails because root README does not link the fixture.

- [ ] **Step 2: Write the example README**

Create `examples/claude-code-gated/README.md` with these sections and commands:

```markdown
# Claude Code gated readiness

This clean-room fixture proves that the published AWF v0.5.1 binary can run an existing Claude Code CLI as a black box, validate its typed output, and pass it through an independent deterministic gate. It uses one Claude call and does not require Docker, Ollama, or an Anthropic API key. Subscription authentication uses a long-lived token created by Claude Code itself.

## Prerequisites

- macOS or Linux on a released AWF architecture
- Claude Code installed with an active subscription (`claude --version` succeeds)
- `curl`, `shasum`, `tar`, and POSIX `sh`

## Install the pinned public binary

Set `OS` and `ARCH` for your machine (`darwin` or `linux`; `amd64` or `arm64`):

```sh
VERSION=0.5.1
OS=darwin
ARCH=arm64
BASE="https://github.com/valbaudo/awf/releases/download/v${VERSION}"

curl -fLO "${BASE}/awf_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fLO "${BASE}/awf_${VERSION}_checksums.txt"
shasum -a 256 -c "awf_${VERSION}_checksums.txt" --ignore-missing
tar -xzf "awf_${VERSION}_${OS}_${ARCH}.tar.gz"
AWF="$(pwd)/awf_${VERSION}_${OS}_${ARCH}/awf"
"$AWF" version
```

## Validate and run

From the AWF repository checkout containing this fixture:

```sh
claude --version
claude setup-token
```

Follow Claude Code's instructions to set `CLAUDE_CODE_OAUTH_TOKEN` in the current private shell. Never paste the token into the workflow, a committed file, chat, or captured logs. Then run:

```sh
"$AWF" validate examples/claude-code-gated/workflow.yaml
"$AWF" run \
  --state-dir "$PWD/.awf" \
  --backend native \
  --agent-env CLAUDE_CODE_OAUTH_TOKEN \
  examples/claude-code-gated/workflow.yaml
unset CLAUDE_CODE_OAUTH_TOKEN
```

`--backend native` is required. The Claude Code adapter currently requires a declared container, which makes automatic selection choose Docker; native execution intentionally ignores the declared image and invokes the host `claude` binary. The explicit `--agent-env` forwards only the subscription token into AWF's isolated per-run Claude configuration; the host's normal logged-in state is intentionally not reused.

On AWF v0.5.1, spell `--state-dir` as the absolute `"$PWD/.awf"`. This works on both macOS and Linux and avoids a macOS native-sandbox bug in which the default relative `.awf` scratch path is denied by `sandbox-exec`. Do not disable the sandbox as a workaround.

The run succeeds with `run <run-id>: ok`. Retrieve the typed generator output using the full gate runtime address:

```sh
"$AWF" outputs <run-id> \
  --state-dir "$PWD/.awf" \
  --step 'gate[0].attempt-1.generate.draft'
```

The output contains `{"ready": true}`. Plain `awf outputs <run-id>` does not expose a gate-internal generator step.

## Cleanup

The run command unsets `CLAUDE_CODE_OAUTH_TOKEN` when it finishes. AWF keeps the journal and typed output under `"$PWD/.awf"` so the `outputs` command and resume remain available. Keep that state while evaluating the fixture; after you no longer need any runs from this checkout, remove the state directory using your normal workspace-cleanup process.

## Troubleshooting

- Authentication failure: unset the token, rerun `claude setup-token`, and repeat from the same private shell. A normal interactive Claude login alone does not cross AWF's isolated `CLAUDE_CONFIG_DIR`.
- Docker starts or an image pull is attempted: rerun with the explicit `--backend native` flag.
- `.awf/output/...: Operation not permitted` on macOS: confirm that the run uses the absolute `--state-dir "$PWD/.awf"` shown above.
- Any other native sandbox failure: keep the error and stop; do not weaken the sandbox.
- Gate rejection or a missing typed output: keep the run id and output and report the fixture as a readiness failure.
```

- [ ] **Step 3: Correct and connect the root README**

Make exactly these changes in `README.md`:

1. Change `VERSION=0.1.0` to `VERSION=0.5.1` in the prebuilt install block.
2. After the Ollama quickstart run command, add this paragraph:

```markdown
To exercise a Claude Code subscription through a typed, deterministic gate without Docker or Ollama, follow the [Claude Code gated readiness example](examples/claude-code-gated/README.md). It uses the published v0.5.1 binary, a `claude setup-token` credential handoff, and the explicit native backend.
```

3. Change the built-in adapter bullet to:

```markdown
- `anthropic/claude-code` — see the [gated native readiness example](examples/claude-code-gated/README.md)
```

- [ ] **Step 4: Re-run the documentation assertions**

Run:

```sh
test -f examples/claude-code-gated/README.md
! rg -n 'VERSION=0\.1\.0' README.md
rg -n 'VERSION=0\.5\.1' README.md
rg -n 'examples/claude-code-gated/README\.md' README.md
rg -n -- '--backend native' examples/claude-code-gated/README.md
rg -n -- '--state-dir "\$PWD/\.awf"' examples/claude-code-gated/README.md
rg -n -- '--agent-env CLAUDE_CODE_OAUTH_TOKEN' examples/claude-code-gated/README.md
rg -n -- 'claude setup-token' examples/claude-code-gated/README.md
rg -n -- "gate\[0\]\.attempt-1\.generate\.draft" examples/claude-code-gated/README.md
```

Expected: every command exits `0`; the fixture link appears twice in root README.

- [ ] **Step 5: Re-run the public-binary workflow from the written instructions**

Open a fresh shell in the repository root and follow only `examples/claude-code-gated/README.md`, using the already-downloaded archive. Do not substitute a source build or undocumented command.

Expected: checksum passes, validation exits `0`, run ends `ok`, and the documented `outputs --step` command returns the typed readiness value.

- [ ] **Step 6: Commit the onboarding documentation**

Run:

```sh
git add README.md examples/claude-code-gated/README.md
git diff --cached --check
git commit -m "docs: document Claude Code native onboarding"
```

Expected: a two-file documentation commit containing no runtime changes.

### Task 3: Run the repository gate and audit scope

**Files:**
- Verify only; no planned file changes.

**Interfaces:**
- Consumes: the two implementation commits from Tasks 1 and 2.
- Produces: the final readiness verdict and evidence that the repository stayed within scope.

- [ ] **Step 1: Run the repository's required verification**

Run:

```sh
make lint test
```

Expected: lint and all tests pass.

- [ ] **Step 2: Audit the complete implementation diff**

Run:

```sh
git diff --check HEAD~2..HEAD
git diff --name-only HEAD~2..HEAD
git status --short
```

Expected changed paths exactly:

```text
README.md
examples/claude-code-gated/README.md
examples/claude-code-gated/workflow.yaml
```

Expected working tree: clean.

- [ ] **Step 3: Record the readiness verdict**

Pass only if all public-release, live-run, output-path, cleanup, documentation, lint, test, and scope checks succeeded. On pass, record the v0.5.1 tag, fixture/docs commit ids, Claude Code version, OS/architecture, run id, elapsed onboarding time, and result in the private adoption ledger.

On any failure, record the exact failing command and error as product-readiness evidence. Do not begin prospect research, outreach, runtime fixes, or the 14-day clock inside this implementation task.
