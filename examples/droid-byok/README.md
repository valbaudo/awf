# droid + BYOK (any provider)

Run a `factory/droid` agent step against a **bring-your-own-key** model — any
provider — instead of Factory's hosted models. droid's BYOK is just one
`settings.json` entry: a `baseUrl`, an `apiKey`, and a `provider`. The AWF droid
adapter is provider-blind (it only passes `--model` and forwards env), so the
same image and the same workflow work for every provider. You change **only** the
three build args.

## How it works

1. `runner.Dockerfile` installs droid and bakes `~/.factory/settings.json` with
   one `customModels[]` entry. `baseUrl`/`apiKey`/`provider` come from build args;
   the API key is referenced as `${BYOK_API_KEY}` and read at runtime, never baked.
2. `byok.yaml` selects that model by id (`with.model: "custom:BYOK-0"`) and
   captures a typed `answer` — typed outputs work over BYOK unchanged.
3. AWF forwards your keys into the container via `--agent-env`.

## Pick a provider — change only the build args

The workflow never changes. Each row below is a different `runner.Dockerfile`
build:

| Provider | `--build-arg BYOK_BASE_URL` | `--build-arg BYOK_PROVIDER` | `--build-arg BYOK_MODEL` (example) |
| --- | --- | --- | --- |
| OpenAI-compatible gateway (OpenRouter, Fireworks, Together, **LiteLLM**, vLLM) | `https://openrouter.ai/api/v1/` | `generic-chat-completion-api` | `meta-llama/llama-3.1-8b-instruct` |
| Native Anthropic endpoint | `https://api.anthropic.com/` | `anthropic` | `claude-sonnet-4-6` |
| Native OpenAI endpoint | `https://api.openai.com/v1/` | `openai` | `gpt-5.5` |
| Local Ollama | `http://host.docker.internal:11434/v1/` | `generic-chat-completion-api` | `llama3.1:latest` |

```bash
docker build -f runner.Dockerfile \
  --build-arg BYOK_MODEL='meta-llama/llama-3.1-8b-instruct' \
  --build-arg BYOK_BASE_URL='https://openrouter.ai/api/v1/' \
  --build-arg BYOK_PROVIDER='generic-chat-completion-api' \
  -t registry.example.com/droid-byok-runner .
docker push registry.example.com/droid-byok-runner
```

## Run

AWF requires an `@sha256:`-pinned image, so pin `byok.yaml`'s `image:` to the
digest of your pushed image first (`docker buildx imagetools inspect <ref>`).
Then:

```bash
export BYOK_API_KEY='sk-...'        # your provider key
export FACTORY_API_KEY='unused'     # see note below
awf run --agent-env FACTORY_API_KEY,BYOK_API_KEY \
  --input '{"question":"Say PONG and nothing else."}' \
  ./byok.yaml
```

## Notes (all verified firsthand against droid v0.138.0)

- **The model id.** droid derives it from `displayName` + index:
  `displayName: "BYOK"` at index 0 → **`custom:BYOK-0`**. It also accepts the raw
  `model` field value and `custom:<model-field>`. A typo'd id is a clean
  `Invalid model:` failure (permanent), never a silent fallback to a hosted model.
  Run `droid exec --model x` inside the image to print the exact ids your
  settings produce.
- **`FACTORY_API_KEY` value is unused for pure BYOK.** The AWF adapter errors at
  run start if the name is *absent*, so it must be present — but droid runs BYOK
  inference without contacting Factory, so any dummy value works. (Confirmed by
  running with a deliberately invalid key: inference still routed to the BYOK
  endpoint.)
- **Keys stay local.** droid does not upload BYOK keys to Factory. `apiKey` uses
  `${VAR}` expansion; the secret is never written into the image.
- **Provider-blind adapter.** Supporting "any provider" needs zero adapter code:
  AWF passes `--model` and forwards env, nothing more.
