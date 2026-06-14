# awf/llm — Gemini explicit context caching

Uploads the `input_files` document once as a Gemini `CachedContent` object and references it on
every `:generateContent` call (≈75% off the cached tokens). Most valuable when a document is read
many times (a gate's generate + evaluate + repairs).

## Run

    export GEMINI_API_KEY=...
    awf run examples/awf-llm-gemini-cache/workflow.yaml --input-files document=/path/to/doc.pdf

## Notes

- `gemini_cache: {mode: explicit, ttl: "600s"}` requires `provider: gemini` and at least one
  `input_files` document (it caches the document). Rejected on other providers.
- **The document must exceed the model's minimum cacheable token count (~2048 for 2.5, ~4096 for
  3.x)** — a smaller document makes cache creation fail with a 400. Use a real, sizeable document.
- Reuse is per `awf run` (an in-process, content-addressed map keyed by model + system prompt +
  document bytes). To share ONE document cache across a gate's generate AND evaluate, leave
  `system_prompt` empty and put role instructions in the user `prompt` (a differing `system_prompt`
  is baked into the cache → per-role caches). Set `ttl` to ~the gate's wall-clock, not longer.
- Storage is billed per-token-per-hour for the whole TTL regardless of reads; AWF's derived cost
  does NOT include storage (verify on the Google console). Cache-read savings ARE reflected.
- Without `gemini_cache`, the document is sent inline each call and Gemini's free *implicit* caching
  applies (document-first ordering) — best-effort, no storage cost.
