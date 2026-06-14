# awf/llm — native Anthropic example

Calls Claude via the native Messages API (`provider: anthropic`).

## Run

    export ANTHROPIC_API_KEY=sk-ant-...
    awf run examples/awf-llm-anthropic/workflow.yaml

## Notes

- `provider: anthropic` uses `POST https://api.anthropic.com/v1/messages` (SSE streaming).
- `cache_system: true` adds an Anthropic `cache_control` breakpoint on the system prompt;
  `cache_documents: true` adds one on the last `input_files:` document block (no effect without
  `input_files`). Breakpoints are 5-minute ephemeral; cached reads bill at ~0.1x base input;
  minimum cacheable prefix is 1024 tokens (Sonnet 4.6 / Opus 4.8), 4096 (Haiku 4.5) — smaller
  prompts simply won't cache.
- `base_url:` may be overridden, but only for **x-api-key-compatible** endpoints (e.g. a
  self-hosted Anthropic-compatible proxy). It does **not** reach Amazon Bedrock or Google Vertex,
  which use AWS SigV4 / GCP OAuth instead of the `x-api-key` header this transport sends.
- `structured_output: response_format` is rejected on this provider (Anthropic has no JSON-schema
  response format); `off` is the effective default and the schema is restated in the prompt.
