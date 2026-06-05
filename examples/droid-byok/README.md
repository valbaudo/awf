# droid + BYOK (any provider)

Run a `factory/droid` agent step against a **bring-your-own-key** model — any
provider — instead of Factory's hosted models. You declare the endpoint entirely
in the step's `with:` block; the AWF droid adapter writes a per-invocation
`--settings` file from those keys, so the **same image works for every provider**
and you change **only the workflow**, never the image.

## The four BYOK `with:` keys

| key | required | what it is |
| --- | --- | --- |
| `base_url` | yes (enables BYOK) | a custom OpenAI-compatible (or Anthropic) endpoint. Setting it puts the step in BYOK mode; `FACTORY_API_KEY` is then not required. |
| `api_key_env` | yes when `base_url` is set | the **name** of a host env var holding the provider key. The literal key never enters the workflow — droid expands a `${NAME}` placeholder from its own process env at runtime. |
| `provider` | optional (default `generic-chat-completion-api`) | the wire protocol the endpoint speaks (see below). |
| `tls_insecure` | optional (bool, default `false`) | skip TLS verification for internal/self-signed endpoints. Prefer the secure path (see below). |

`model` stays a plain id in both modes (e.g. `claude-sonnet-4`); the adapter
turns it into a `custom:<model>` reference internally.

### Naming the key, not inlining it

`api_key_env` names a host env var; the secret value never appears in `byok.yaml`.
Forward that name into the agent steps with the workflow's top-level `env:` field
(the in-workflow equivalent of `awf run --agent-env`):

```yaml
env: [LITELLM_API_KEY]          # forwards the NAME; the value is read from the host
...
    with:
      base_url: https://litellm.internal/v1
      api_key_env: LITELLM_API_KEY
```

The named var must be on the forwarded allowlist (via `env:` or `--agent-env`) or
the step fails as a permanent config error. Only the **name** folds into the
content digest; the value resolves from the host on every run and is never written
to the journal, blobs, or traces.

### `provider` choices

- `generic-chat-completion-api` (default) — OpenAI Chat Completions. Use for
  LiteLLM, OpenRouter, Fireworks, Together, vLLM, local Ollama, etc.
- `openai` — OpenAI Responses API.
- `anthropic` — Anthropic Messages API.

A provider that doesn't match what the endpoint speaks fails the call.

### TLS: prefer trusting the CA over skipping verification

For an internal gateway with a private CA, the **preferred** path is to trust that
CA rather than disable verification: set `NODE_EXTRA_CA_CERTS` to the bundle path
(or `NODE_USE_SYSTEM_CA=1` to use the system trust store) — bake it into the image
on docker, or forward it via the workflow `env:` field. Only fall back to
`tls_insecure: true` (which sets `NODE_TLS_REJECT_UNAUTHORIZED=0` for the droid
process) for throwaway/self-signed endpoints; it exposes the connection to
interception. (droid is Bun-compiled, so CA-bundle support can be version-dependent.)

### DRY tip: with several BYOK steps, use a YAML anchor

This is a generic-YAML aside, not a BYOK feature. When a workflow has more than one
BYOK droid step, define the shared `with:` keys **once** as a YAML anchor under an
`x-`-prefixed top-level holder (`x-` is the tolerated convention for author-defined
keys the validator skips — it does *not* mean the validator ignores arbitrary
unknown keys) and merge it into each step with the merge key `<<: *byok`, so the
keys aren't repeated:

```yaml
x-byok: &byok
  base_url: https://litellm.internal/v1
  api_key_env: LITELLM_API_KEY
  provider: generic-chat-completion-api
...
    with:
      <<: *byok                # merge the shared endpoint config
      prompt: "..."
```

## The image (`runner.Dockerfile`)

The image just installs droid; the endpoint lives in the workflow, not the image.
It sets two opsec preconditions, because the adapter cannot write to the image:

- **`cloudSessionSync: false`** in `~/.factory/settings.json`. droid mirrors session
  transcripts (gateway URL + tool I/O) to Factory's web app by default, with no env
  knob — disable it in the image for metadata hygiene. (The resolved API key is
  *not* part of this: it never touches disk — only the `${NAME}` placeholder does.)
- **`OTEL_SDK_DISABLED=true`** for defense in depth (the adapter also sets the OTEL
  vars in the container env).

## Run

AWF requires an `@sha256:`-pinned image, so build + push the runner image and pin
`byok.yaml`'s `image:` to its digest first (`docker buildx imagetools inspect <ref>`).
Then:

```bash
export LITELLM_API_KEY='sk-...'     # your provider key; only the NAME is in the workflow
awf run --input '{"question":"Say PONG and nothing else."}' ./byok.yaml
```

No `FACTORY_API_KEY` is needed: a BYOK step's `api_key_env` var carries auth, and
the adapter neither requires nor forwards `FACTORY_API_KEY` in BYOK mode.

## Notes (verified firsthand against droid v0.138.0)

- **The key stays out of the definition.** `api_key_env` names a host var; the
  adapter writes only the literal `${NAME}` placeholder into the per-invocation
  settings file. The resolved secret never reaches the command string, the image,
  or the journal.
- **A bad `model` id is a clean failure.** A typo is a permanent `Invalid model:`
  error, never a silent fallback to a hosted model.
- **Provider-agnostic by construction.** Supporting "any provider" needs zero
  adapter code — the adapter writes the `with:`-supplied endpoint and forwards env.
