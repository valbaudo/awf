# Input-parameterizable agent roles

Shows a role's `model:` templated against `{{ input.* }}`, resolved at step
execution against the *owning module's* run input — so one `--input` steers a
whole fleet, and a child workflow's own role reads a model forwarded across the
`call:` boundary. Both files are containerless (`uses: awf/llm`), so this
validates and runs with no `containers:`/Docker.

- `review.awf.yaml` (parent) — declares `judge_model`/`coder_model` in its
  `input_schema`, two roles (`judge`, `coder`) whose `model:` is
  `"{{ input.judge_model }}"` / `"{{ input.coder_model }}"`, a `draft`/`review`
  pair using those roles, and a `deep` step that `call:`s the child, forwarding
  `coder_model` explicitly via `input:`.
- `analyzer.awf.yaml` (child) — declares its **own** `input_schema: { model }`
  and its **own** role `worker` reading `{{ input.model }}`; it never sees the
  parent's roles or input (call-boundary privacy).

## Run

```sh
export OPENAI_API_KEY=sk-...     # for judge (default provider: openai)
export ANTHROPIC_API_KEY=sk-ant-...  # for coder/worker (provider: anthropic)
awf run examples/roles-input-selection/review.awf.yaml \
  --input '{"judge_model":"gpt-5.5","coder_model":"claude-sonnet-5"}'
```

`--input` is required: an omitted `--input` resolves a role's `{{ input.* }}`
reference to empty — schema `default:`s are not materialized — so the
`judge`/`coder`/`worker` roles above need `judge_model`/`coder_model` supplied
explicitly.
