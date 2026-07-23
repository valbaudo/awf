# jury: a hot-swap panel of judges

Demonstrates `jury:` (see **awf-workflow(5)**, `## gate` → `jury:`): a gate whose
`evaluate:` is a single judge step with a 3-reviewer voting panel bolted on,
instead of a hand-composed `map` + `run:` reducer. The `generate:` step drafts a
short product description; the panel votes on whether it follows the brand
style guide (plain language, no exclamation points, no superlatives), and the
gate repairs the draft until 2 of 3 reviewers agree it does.

The judge (`uses: example/style-checker`) is a placeholder adapter and its
container image is not real — this fixture is for `awf validate`, not
`awf run`. Swap in a real container-based judge (or `awf/llm`) to make it
executable.

## Validate

    awf validate examples/jury-gate/workflow.yaml

Expect a clean pass. `awf validate` reports one informational warning,
**AWF3002**, on the judge step — nothing outside the gate references
`step.judge.*` directly, because the panel's verdict is read positionally as
`{{ evaluate.meets_style }}` (a jury verdict is never `step.<id>`, see
**awf-workflow(5)**). The same warning appears on the generator step of
`examples/awf-llm-gate-extract/workflow.yaml` for the identical reason: a
gate's internal steps are gate-scoped by design.

## The hot-swap

Removing the `jury:` block and its `over:`/`quorum:` keys — with no edits to
`uses:`, `container:`, `output_schema:`, or `until:` — turns this back into an
ordinary single-judge gate. That single edit, in either direction, is the whole
feature.
