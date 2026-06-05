# Runs Factory's droid CLI against ANY BYOK (bring-your-own-key) model endpoint:
# an OpenAI-compatible gateway (OpenRouter, Fireworks, Together, a self-hosted
# LiteLLM/vLLM proxy), a local Ollama, or a native Anthropic/OpenAI endpoint.
#
# The endpoint is NOT baked into this image. The AWF droid adapter writes a
# per-invocation `--settings` file from the workflow's `with:` keys
# (base_url/api_key_env/provider/model), so one image works for every provider
# and every model — see ./byok.yaml. This Dockerfile only installs droid and sets
# two image-level opsec preconditions (below).
#
# Build:
#   docker build -f runner.Dockerfile -t registry.example.com/droid-byok-runner .
#   docker push registry.example.com/droid-byok-runner

# Pin this to a digest (FROM debian:stable-slim@sha256:...) for reproducibility.
FROM debian:stable-slim

# droid's installer drops the binary in ~/.local/bin. xdg-utils is required on
# Linux per Factory's docs; ca-certificates/git are commonly needed by agents.
RUN apt-get update && apt-get install -y --no-install-recommends \
        curl ca-certificates git xdg-utils \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://app.factory.ai/cli | sh
ENV PATH="/root/.local/bin:${PATH}"

# --- Opsec preconditions (BYOK keys never persist; sessions can still leak) ----
#
# The resolved API key is never written to disk: the adapter writes only the
# literal ${LITELLM_API_KEY} placeholder into the per-invocation settings file,
# and droid expands it from process env at runtime.
#
# Telemetry: the adapter sets OTEL_SDK_DISABLED / OTEL_CUSTOMER_ENABLED in the
# container env. We also set OTEL_SDK_DISABLED here for defense in depth.
ENV OTEL_SDK_DISABLED=true
#
# cloudSessionSync has NO env knob and is ON by default — droid would mirror
# session transcripts (gateway URL + tool I/O) to Factory's web app. Disable it
# at the image level for metadata hygiene. This is the only thing this file must
# write to ~/.factory/settings.json; the BYOK endpoint config lives in the
# workflow, not here.
RUN mkdir -p /root/.factory && cat > /root/.factory/settings.json <<'EOF'
{
  "general": { "cloudSessionSync": false }
}
EOF
