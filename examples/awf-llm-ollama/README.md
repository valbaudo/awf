# awf/llm + Ollama (local model, no container needed)

Run a `awf/llm` agent step against a **local Ollama** instance. The step is
containerless — no `containers:` block, no Docker image, no Compose file. The
adapter issues one streaming HTTP call directly to Ollama and binds the response
to a typed `output_schema`.

## Prerequisites

1. **Ollama running locally**

   ```sh
   ollama serve           # starts Ollama at localhost:11434
   ollama pull llama3.1   # download the model used in workflow.yaml
   ```

2. **`OPENAI_API_KEY` set to a placeholder** — Ollama ignores the key value, but
   `awf/llm` requires the named env var to be present:

   ```sh
   export OPENAI_API_KEY=ollama
   ```

## Run

```sh
awf run examples/awf-llm-ollama/workflow.yaml
```

The `classify` step streams token deltas live to stderr, then prints the typed
output. No containers are started; the step calls Ollama directly over HTTP.

## How `host.docker.internal` works

`workflow.yaml` uses `base_url: http://host.docker.internal:11434` so the same
workflow file works when AWF itself runs inside a container (e.g. a CI runner).
`host.docker.internal` resolves to the host on Docker Desktop (macOS/Windows)
automatically.

On **Linux** with a plain Docker daemon, add `--add-host` to the `docker run`
command that starts the AWF runner container:

```sh
docker run --add-host host.docker.internal:host-gateway ...
```

When running AWF directly on the host (not inside a container), replace
`host.docker.internal` with `localhost` or `127.0.0.1`.

## Containerless step

This workflow has no `containers:` block. The `awf/llm` adapter is the first
AWF adapter that does not require a container — it connects to the model
endpoint directly via HTTP. You can run `awf validate` and `awf run` without
Docker installed.

## Trying other local endpoints

`structured_output: ollama_format` targets Ollama's native `/api/chat` path.
To use the OpenAI-compat path instead (e.g. for vLLM, llama.cpp, or LM
Studio), switch to:

```yaml
base_url: http://host.docker.internal:11434/v1
structured_output: response_format
```

LiteLLM and Bifrost gateways also work on the OpenAI-compat path.
