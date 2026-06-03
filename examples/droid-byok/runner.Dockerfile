# Runs Factory's droid CLI against ANY BYOK (bring-your-own-key) model endpoint:
# an OpenAI-compatible gateway (OpenRouter, Fireworks, Together, a self-hosted
# LiteLLM/vLLM proxy), a local Ollama, or a native Anthropic/OpenAI endpoint.
#
# droid's BYOK is nothing more than a settings.json triple — baseUrl + apiKey +
# provider. The AWF droid adapter never reads that triple (it only passes
# `--model` and forwards env), so this one image works for every provider; you
# change only the build args. There is no per-provider code anywhere.
#
# Build (example: an OpenAI-compatible gateway such as OpenRouter):
#   docker build -f runner.Dockerfile \
#     --build-arg BYOK_MODEL='meta-llama/llama-3.1-8b-instruct' \
#     --build-arg BYOK_BASE_URL='https://openrouter.ai/api/v1/' \
#     --build-arg BYOK_PROVIDER='generic-chat-completion-api' \
#     -t droid-byok-runner .
#
# The API key is NOT baked into the image. It is read at runtime from
# $BYOK_API_KEY, which AWF forwards with `--agent-env BYOK_API_KEY`.

# Pin this to a digest (FROM debian:stable-slim@sha256:...) for reproducibility.
FROM debian:stable-slim

# droid's installer drops the binary in ~/.local/bin. xdg-utils is required on
# Linux per Factory's docs; ca-certificates/git are commonly needed by agents.
RUN apt-get update && apt-get install -y --no-install-recommends \
        curl ca-certificates git xdg-utils \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://app.factory.ai/cli | sh
ENV PATH="/root/.local/bin:${PATH}"

# --- BYOK custom model: the only provider-specific part ----------------------
ARG BYOK_MODEL
ARG BYOK_BASE_URL
ARG BYOK_PROVIDER=generic-chat-completion-api
ARG BYOK_DISPLAY_NAME=BYOK

# droid loads custom models from ~/.factory/settings.json. The apiKey field uses
# ${VAR} expansion, so the secret stays out of the image and arrives at runtime.
# cloudSessionSync:false stops droid mirroring sessions to Factory's web app
# (the AWF adapter already disables OTEL via container env).
#
# The model id you pass to AWF (`with.model`) is derived from displayName +
# index: displayName "BYOK" at index 0 => "custom:BYOK-0". droid also accepts the
# raw `model` field value and `custom:<model-field>`. Run `droid exec --model x`
# to print the exact ids your settings produce.
RUN mkdir -p /root/.factory && cat > /root/.factory/settings.json <<EOF
{
  "customModels": [
    {
      "model": "${BYOK_MODEL}",
      "displayName": "${BYOK_DISPLAY_NAME}",
      "baseUrl": "${BYOK_BASE_URL}",
      "apiKey": "\${BYOK_API_KEY}",
      "provider": "${BYOK_PROVIDER}"
    }
  ],
  "cloudSessionSync": false
}
EOF
