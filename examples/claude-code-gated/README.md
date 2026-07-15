# Claude Code gated readiness

This clean-room fixture proves that the published AWF v0.5.1 binary can run an
existing Claude Code CLI as a black box, validate its typed output, and pass it
through an independent deterministic gate. It contains one AWF agent step and
one Claude Code CLI invocation, followed by a deterministic shell evaluator.
The generator and evaluator retries and the gate attempt limit are each one, so
AWF performs no orchestration-level reruns. AWF does not measure how many
provider requests occur inside the CLI; the accepted readiness run reported two
Claude turns. The fixture does not require Docker, Ollama, or an Anthropic API
key. Subscription authentication uses a long-lived token created by Claude Code
itself.

## Prerequisites

- macOS or Linux on a released AWF architecture
- Claude Code installed with an active subscription (`claude --version`
  succeeds)
- `curl`, `shasum`, `tar`, and POSIX `sh`

## Install the pinned public binary

Set `OS` and `ARCH` for your machine (`darwin` or `linux`; `amd64` or
`arm64`):

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

Follow Claude Code's instructions to set `CLAUDE_CODE_OAUTH_TOKEN` in the
current private shell. Keep the token out of shell history: never paste it into
the workflow, a committed file, chat, or captured logs.

Then run:

```sh
"$AWF" validate examples/claude-code-gated/workflow.yaml
"$AWF" run \
  --state-dir "$PWD/.awf" \
  --backend native \
  --agent-env CLAUDE_CODE_OAUTH_TOKEN \
  examples/claude-code-gated/workflow.yaml
unset CLAUDE_CODE_OAUTH_TOKEN
```

`--backend native` is required. The Claude Code adapter currently requires a
declared container, which makes automatic selection choose Docker; native
execution intentionally ignores the declared image and invokes the host
`claude` binary. The explicit `--agent-env` forwards only the subscription
token into AWF's isolated per-run Claude configuration; the host's normal
logged-in state is intentionally not reused.

On AWF v0.5.1, spell `--state-dir` as the absolute `"$PWD/.awf"`. This works
on both macOS and Linux and avoids a macOS native-sandbox bug in which the
default relative `.awf` scratch path is denied by `sandbox-exec`. Do not disable
the sandbox as a workaround.

The run succeeds with `run <run-id>: ok`. Retrieve the typed generator output
using the full gate runtime address:

```sh
"$AWF" outputs <run-id> \
  --state-dir "$PWD/.awf" \
  --step 'gate[0].attempt-1.generate.draft'
```

The output contains `{"ready": true}`. Plain `awf outputs <run-id>` does not
expose a gate-internal generator step.

## Cleanup

The run command unsets `CLAUDE_CODE_OAUTH_TOKEN` when it finishes. AWF keeps
the journal and typed output under `"$PWD/.awf"` so the `outputs` command and
resume remain available. Keep that state while evaluating the fixture; after
you no longer need any runs from this checkout, remove the state directory
using your normal workspace-cleanup process.

## Troubleshooting

- Authentication failure: unset the token, rerun `claude setup-token`, and
  repeat from the same private shell. A normal interactive Claude login alone
  does not cross AWF's isolated `CLAUDE_CONFIG_DIR`.
- Docker starts or an image pull is attempted: rerun with the explicit
  `--backend native` flag.
- `.awf/output/...: Operation not permitted` on macOS: confirm that the run
  uses the absolute `--state-dir "$PWD/.awf"` shown above.
- Any other native sandbox failure: keep the error and stop; do not weaken the
  sandbox.
- Gate rejection or a missing typed output: keep the run id and output and
  report the fixture as a readiness failure.
