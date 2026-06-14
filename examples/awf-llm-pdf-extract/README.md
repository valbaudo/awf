# awf/llm PDF / image extraction

Extract typed fields from a scanned PDF or image with a single `awf/llm` model
call. The document is passed to the model as an inline content part via
`input_files`; no container is needed.

## What it does

The `extract` step receives the document at runtime, forwards it to the Gemini
API as an inline part, and returns a typed JSON object with two fields:

```json
{ "total": 142.50, "vendor": "Acme Corp" }
```

The shape is enforced by `output_schema` — the step fails if the model returns
something that does not match.

## Run

### Gemini (default)

```sh
export GEMINI_API_KEY=<your key>
awf run examples/awf-llm-pdf-extract/workflow.yaml \
  --input-files document=invoice.pdf
```

### OpenAI (vision-capable model)

Switch `provider` and `model` in `workflow.yaml`:

```yaml
with:
  provider: openai
  model: gpt-4o
  api_key_env: OPENAI_API_KEY
  prompt: "Extract the invoice total and vendor name from the attached document."
```

Then run:

```sh
export OPENAI_API_KEY=<your key>
awf run examples/awf-llm-pdf-extract/workflow.yaml \
  --input-files document=invoice.pdf
```

### Ollama (local, images only)

Point `base_url` at the local Ollama instance and pick a vision model:

```yaml
with:
  base_url: http://localhost:11434
  model: llava
  api_key_env: OPENAI_API_KEY
```

## How input_files works

`input_files` at the top level declares the files the workflow accepts.  The
`extract` step's `input_files` block forwards `input.files.document` to the
`awf/llm` adapter as a labelled binary part.  The adapter encodes it inline
(base64 for OpenAI/Gemini REST paths; the `images[]` field for Ollama) before
issuing the model call.  Per-file format and provider compatibility is checked
at run time (warning AWF2003).
