# AWF backend auto, workflow assets, aggregate products, and artifact contracts

**Date:** 2026-06-09
**Status:** design, pending user review
**Repo:** the public `awf` runtime.
**Approach:** one umbrella spec, three implementation slices.

The man page (`man/awf-workflow.5.md`) remains the workflow-format contract. Every
format-touching change below lands in the man page before runtime behavior changes.

## 1. Goal and non-goals

**Goal.** Make file-bearing AWF workflows deterministic, replayable, and
self-validating:

- `awf run` defaults to a backend `auto` mode that records the concrete backend
  selected for the run.
- Workflows can declare local files and directories as assets. Asset bytes are part
  of the workflow digest, are snapshotted into blobs at run start, and are staged
  into steps from that run-start snapshot.
- `map` nodes can name their aggregate product so downstream refs can use a stable
  handle such as `step.version_universe.files.item4`.
- Named `output_files` can declare artifact contracts. JSON and JSONL artifacts are
  validated by AWF at capture time, before commit.

**Non-goals.**

- No native resume support. Native runs still record `native`; `awf resume` keeps
  rejecting native logs with the existing clear non-resumable error.
- No automatic asset staging into every step.
- No schema registry beyond inline schemas and schema refs to declared assets.
- No artifact formats beyond JSON and JSONL.
- No raw text refs. Typed outputs remain the scalar/object dataflow channel; files
  remain the artifact channel.
- No new backend factory, plugin layer, registry, or state writer.
- No distributed execution, multi-host scheduling, or compensation semantics.

## 2. Product shape

### 2.1 Backend `auto`

`awf run --backend` accepts `auto`, `fake`, `docker`, and `native`.

`auto` is the default for `awf run`. It is only an input mode: `run.started.Backend`
records the concrete backend selected for that run, never `auto`.

Selection rule:

- Select `docker` when the workflow uses a Docker-only feature.
- Otherwise select `native`.
- Never auto-select `fake`. `fake` is only used when the operator passes
  `--backend fake` or tests inject it.

For this spec, "Docker-only" means "statically detectable from the loaded workflow
and unsupported by native but supported by Docker." A future slice that adds another
native-unsupported production feature must add that feature to the auto-selection
scan and its tests in the same change.

The first Docker-only features are:

- any static compose container, meaning a top-level `containers.<name>.compose`
  entry;
- `snapshot: workspace`;
- runtime `compose:` control nodes, detected by `ir.FirstRuntimeComposePath(wf)`;
- map dynamic images, detected by `len(ir.MapImageTargets(wf)) > 0`;
- any later feature whose backend capability is unsupported by native and supported
  by Docker.

Explicit `--backend native` continues to fail fast for Docker-only features. Explicit
`--backend docker` keeps its current meaning. Explicit `--backend fake` remains a
test/development escape hatch: it is never chosen by `auto`, and it runs only if the
existing capability guards accept the fake backend for the workflow. Assets and
artifact contracts add no new backend capability; they use `Backend.CopyTo`,
`Backend.CaptureFiles`, and backend-independent JSON validation, so fake follows the
same semantics as any backend that already supports those calls. `awf resume` does
not accept a backend flag; it reads the recorded concrete backend from `run.started`.

### 2.2 Workflow assets

Add top-level `assets`:

```yaml
assets:
  policy: ./assets/policy.json
  fixtures: ./fixtures
  result_schema: ./schemas/result.schema.json
```

An asset id names one local file or one local directory. Paths are resolved relative
to the workflow file. Loader path safety matches compose loading: no absolute paths,
no backslashes, no `..` escape, and no symlink escape.

Path normalization and digest canonicalization:

- Asset ids must match `^[A-Za-z_][A-Za-z0-9_-]*$`. Duplicate asset ids are a
  validation error if the YAML decoder preserves duplicate-key information; if it
  does not, the implementation must add duplicate-key detection before unmarshaling
  into the final map.
- Asset ids are sorted lexicographically before digest folding.
- Authored paths are cleaned to forward-slashed workflow-relative paths.
- A cleaned path that is empty, absolute, contains `..`, or differs only by
  backslash normalization is rejected.
- Asset ids, normalized asset source paths, and directory relative paths containing
  NUL, tab, CR, or LF are rejected. The digest stream uses tab-separated rows, so
  ambiguous row encoding is a load error, not an escaping format.
- Directory walks use `Lstat`-style metadata and do not follow symlinks. Any symlink
  encountered as the asset path itself or inside a directory asset is a load error.
- Directory file entries are sorted lexicographically by their forward-slashed
  path relative to the asset root.
- Digest input includes explicit domain tags so file assets, directory assets, and
  compose files cannot collide by shape.

Asset digest byte stream, before the outer workflow digest hashes it:

```text
awf-assets-v1\n
asset\t<asset-id>\t<kind>\t<normalized-source>\n
file\t<asset-id>\t<relative-path>\tsha256:<hex>\t<size-decimal>\n
...
```

For file assets, `<relative-path>` is `.`. For directory assets, one `file` row is
emitted per contained regular file. Rows use LF newlines, UTF-8 path bytes as loaded
from the filesystem, and no extra whitespace.

Directory assets snapshot all regular files recursively under that directory. The
asset manifest stores each file by forward-slashed path relative to the asset root.
A directory asset with zero regular files is rejected. Empty directories inside a
non-empty directory asset are ignored and not recreated on staging. File mode bits
are not part of the first contract; staged files are regular readable files.
Workflows that need an executable script asset should invoke it through an
interpreter such as `sh /work/script.sh`.

Asset bytes are included in the workflow digest:

- file asset: asset id, normalized source path, type marker, and content hash;
- directory asset: asset id, normalized source path, type marker, and each contained
  relative file path plus content hash, sorted bytewise by relative path.

At run start, after opening blobs and before appending `run.started`, the CLI writes
all asset bytes to `Blobs`. `run.started` records an asset manifest of ids to blob
refs. This preserves the existing commit rule: content-addressed bytes exist before
the pointer that names them is synced.

Wire shape:

```go
type AssetSnapshot struct {
    Source string         `json:"source"` // normalized workflow-relative source path
    Kind   string         `json:"kind"`   // "file" or "dir"
    Files  []AssetFileRef `json:"files"`  // sorted by Path
}

type AssetFileRef struct {
    Path    string `json:"path"`     // "." for file assets; dir-relative path for dir assets
    BlobRef string `json:"blob_ref"` // state.Blobs ref, e.g. awf-d1:sha256:...
    SHA256  string `json:"sha256"`   // hex content hash for diagnostics
    Size    int64  `json:"size"`     // original byte length
}

type RunStartedData struct {
    // existing fields...
    Assets map[string]AssetSnapshot `json:"assets,omitempty"`
}
```

For a file asset, `Files` contains exactly one entry with `Path: "."`. For a
directory asset, each entry's `Path` is the relative file path under the asset root.

Resume remains strict. `awf resume` reloads the workflow and recomputes the digest
from the current workflow file, compose files, and asset bytes. If that digest differs
from `run.started.WorkflowDigest`, resume fails before dispatch. If the digest
matches, asset staging uses the asset blob refs recorded in `run.started`, not fresh
reads from the checkout.

Assets stage through the existing `input_files` mechanism:

```yaml
- code:
    id: inspect
    container: lab
    input_files:
      /work/policy.json: asset.policy
      /work/fixtures: asset.fixtures
    run: ./inspect.sh
```

`asset.<id>` refs are valid only in:

- step `input_files` values;
- artifact contract `schema_ref` values.

They are not template refs and do not expose raw text.

### 2.3 Named aggregate products

Add optional `id` to `map`:

```yaml
- map:
    id: version_universe
    over: "{{ step.scan.versions }}"
    as: version
    container: version_lab
    body:
      - code:
          id: build_version
          container: version_lab
          run: ./build-version.sh
          output_schema:
            type: object
            properties:
              version: { type: string }
            required: [version]
    reduce:
      run: ./combine.sh
      container: reducer
      output_files:
        item4:
          path: /out/item4.json
          format: json
```

`map.id` is an aggregate product id, not a node path. Journal keys and OTel
`awf.node.path` values stay positional (`map[0]`, `map[0].item-4`, and so on).

Validation:

- `map.id`, when present, must match `^[A-Za-z_][A-Za-z0-9_-]*$` and must not equal
  a reserved addressing token: `generate`, `evaluate`, `until`, `then`, `else`,
  `body`, `do`, `catch`, or `finally`.
- Aggregate product ids must not duplicate code, agent, or signal step ids.
- Aggregate product ids must not duplicate other aggregate product ids.
- Asset ids live in the `asset.` ref namespace and do not conflict with step or
  aggregate product ids.

Reference behavior:

- For a map with `reduce:`, `step.<map.id>.<field>` resolves to the reducer typed
  output and `step.<map.id>.files.<name>` resolves to the reducer named artifact.
- For a map without `reduce:`, `step.<map.id>.<field>` resolves to the same
  index-ordered aggregate array shape that body-step aggregate refs expose, using
  the output of the map body's last step as the product:
  - `step.<map.id>` is a compact array of committed item output maps.
  - `step.<map.id>.<field>` is a compact array of that field from each committed
    item output.
  - Items are ordered by item index ascending.
  - Failed, missing, and pruned items are omitted, so the result is compact and does
    not preserve original item indexes. Authors who need the original index must
    include it in the body step's typed output, typically from `{{ <as>.index }}`.
  - This unreduced aggregate array is legal initially and explicitly only in
    another map's `over:`.
- If a non-reduced map's body has multiple steps, `map.id` names only the last body
  step's output. Authors who need an earlier body step keep using the existing
  `step.<bodyStep>` aggregate ref.
- If a non-reduced map's last body node is not a code, agent, or signal step, refs
  to `step.<map.id>` are invalid. The implementation does not descend into a final
  control node to infer a product.
- `step.<map.id>.files.<name>` is only valid when the map has `reduce:`. A
  non-reduced map has N per-item files, not one stageable artifact. This slice does
  not create directory artifacts for unreduced branch files.
- Existing body-step reduce refs remain valid for backward compatibility:
  `step.<bodyStep>.<field>` and `step.<bodyStep>.files.<name>` continue to resolve
  to the reducer product where they resolve that way today.

### 2.4 Artifact contracts

Existing output forms remain valid:

```yaml
output_files: [/out/a]
output_files: { report: /out/report.md }
```

Named output files gain an object form:

```yaml
output_files:
  result:
    path: /out/result.jsonl
    format: jsonl
    schema_ref: asset.result_schema
  summary:
    path: /out/summary.json
    format: json
    schema:
      type: object
      properties:
        count: { type: integer }
      required: [count]
```

Fields:

- `path` is required and has the same rules as today's named output path.
- `format` is optional. Valid values are `json` and `jsonl`. Omitted means capture
  only.
- `schema` is an optional inline JSON Schema object.
- `schema_ref` is an optional `asset.<id>` ref to a declared file asset whose bytes
  are a JSON Schema document.
- `schema` and `schema_ref` are mutually exclusive.
- If `schema` or `schema_ref` is present, `format` must be `json` or `jsonl`.
- If `schema_ref` points at a directory asset or missing asset, validation fails
  before run.

Capture-time validation:

- If `format: json`, the full captured file must parse as one JSON value. If a
  schema is present, that value must validate against the schema.
- If `format: jsonl`, each non-empty line must parse as one JSON value. If a schema
  is present, each parsed line must validate against the schema.
- Artifact contract schemas use the same `github.com/santhosh-tekuri/jsonschema/v6`
  dependency and dialect behavior as current `output_schema` validation. There is no
  new dialect selector in this slice. Unlike `ValidateAgainstSchema`, artifact
  validation must allow any JSON value shape accepted by the schema, not only a JSON
  object, because artifacts may validly be arrays or scalar JSON values.
- Schema documents are compiled as self-contained resources. The compiler resource
  URI should be deterministic, for example
  `awf://artifact/<node-path>/<artifact-name>/schema`. This slice does not add
  remote schema loading or workflow-relative `$ref` resolution outside the schema
  document or declared schema asset bytes. Internal JSON Pointer `$ref` values
  inside the same schema document must work. External URI refs and relative refs to
  other files are rejected by validation or fail schema compile. The asset id is not
  a schema URI; it only selects the bytes to compile. Compile caching is optional;
  correctness must not depend on it.
- Invalid artifacts are mechanical failures. The dispatcher returns
  `retryable_failure` with an error shaped as:

  ```text
  artifact contract failed at <node-path>: output_files.<name> (<container-path>, format=<format>): <cause>
  ```

  `<cause>` is the first parse, schema compile, or schema validation failure. For
  JSONL, include `line <n>` in the cause. The interpreter retries exactly as it does
  for other retryable mechanical failures.
- Artifact validation runs in the dispatcher after successful `CaptureFiles` and
  before returning an `OutcomeOK` `DispatchResult`. `CaptureFiles` returns bytes;
  `engine.Commit` is still the only code that writes captured output files to blobs.
  If validation fails, the dispatcher returns `retryable_failure`; `engine.Commit`
  is never called for that attempt. No invalid captured artifact bytes are written
  to blobs, and no `node.completed` references them.
  API shape stays narrow: `ResolvedInputs.OutputFiles` carries capture paths plus
  contracts; `DispatchResult.Files` carries only captured files that have already
  passed their contracts; `Commit` remains contract-agnostic.

When this lands, remove workflow fixture scripts whose only job is validating JSON
or JSONL artifact shape, and replace their checks with artifact contracts. Do not
remove scripts that perform domain logic, fetch data, normalize data, or validate
anything AWF still does not own.

## 3. Existing seams to reuse

Backend selection is already centralized:

- `cli/run.go` owns `--backend`, validation, digesting, blob opening, backend
  construction, and `run.started`.
- `cli/backend.go` owns `newBackend`, `resolveBackend`, and
  `readBackendKindFromLog`.
- `cli/resume.go` already reads the backend from `run.started` and uses it for
  resume.

Assets should extend the compose byte path:

- `loader.Load` reads workflow-referenced files and normalizes paths.
- `ir.LoadedDefinition` carries `Workflow` plus loaded compose bytes.
- `Workflow.ComputeDigest` already folds compose path plus content hashes.

Artifact contracts should extend the artifact channel:

- `ir.OutputFiles` owns the bare list and named map wire forms.
- `engine.LocalDispatcher.runCode` captures output files after a successful exit and
  before committing.
- `engine.LocalDispatcher.runAgent` uses the same capture semantics for agent
  output files.
- `engine.Commit` already writes typed outputs, stdout, files, and snapshots to
  blobs before appending `node.completed`.

Named aggregate products should extend existing map/reduce resolution:

- `engine.Scope.aggregateMapOutputs` already resolves body-step aggregate refs and
  prefers a reduced `node.completed` at the map path when present.
- `Scope.ResolveArtifactPath` already has the reduce artifact alias behavior.
- `ir.validate_refs` and `ir.validate_input_files` already validate ref scope and
  artifact refs.

## 4. Slice plan

### Slice 1: backend `auto`

Contract:

- Update `awf run` docs and usage to include `auto`.
- `auto` is the default for `awf run`.
- `run.started.Backend` records `native`, `docker`, or `fake`, never `auto`.
- `awf resume` reads and uses the recorded concrete backend.

Implementation:

- Add a backend mode constant for `auto`.
- Add a pure workflow feature scan in `cli/backend.go` that selects `docker` for
  Docker-only features and `native` otherwise. It must use:
  - direct scan of `wf.Containers[*].Compose` for static compose;
  - direct scan of `wf.Containers[*].Snapshot == "workspace"` for snapshots;
  - `ir.FirstRuntimeComposePath(wf)` for runtime compose;
  - `len(ir.MapImageTargets(wf)) > 0` for map dynamic images.
- Replace the native-only static compose fail-fast with selection plus capability
  checks:
  - explicit native still fails for Docker-only features;
  - auto chooses docker for those features;
  - explicit docker proceeds;
  - explicit fake proceeds where fake advertises the needed capability. Current
    expected outcomes: fake accepts runtime image and runtime compose because its
    caps advertise both; fake accepts snapshot workflows only when constructed with
    blob storage and otherwise fails the existing snapshot guard; fake has no
    special auto-selection behavior because `auto` never selects fake.
- Keep `readBackendKindFromLog` rejecting native resume.

Tests:

- Unit tests for `resolveAutoBackendKind` or equivalent:
  - simple workflow -> native;
  - static compose -> docker;
  - `snapshot: workspace` -> docker;
  - runtime `compose:` -> docker;
  - map dynamic image -> docker.
- CLI test that default `awf run` writes concrete `native` for a simple workflow.
- CLI/integration test that auto static compose writes concrete `docker`.
- Resume test that a run started by auto uses the recorded backend and never
  re-selects from current features.

### Slice 2: assets plus artifact contracts

Contract:

- Add top-level `assets`.
- Add `asset.<id>` refs for `input_files` and `schema_ref`.
- Add object-form named `output_files`.
- Update artifact channel docs to say validation happens after capture and before
  commit.

Implementation:

- Extend IR with asset declarations and the `RunStartedData.Assets` snapshot
  manifest described in section 2.2.
- Extend loader to read files and directories once, using the same path safety model
  as compose files.
- Extend digest calculation to include asset manifests.
- Store asset blobs before `run.started`; record refs in `RunStartedData`.
- Pass the recorded asset snapshot into the dispatcher or input resolver.
- Extend `input_files` validation and resolution for `asset.<id>`.
- Extend `OutputFiles` to unmarshal named map values that are either strings or
  objects.
- Add artifact validation after successful `CaptureFiles` and before building an OK
  `DispatchResult`.
- Validate both code and agent capture paths.

Tests:

- Loader tests for file asset, directory asset, missing asset, absolute path,
  backslash path, `..` escape, symlink escape, and directory ordering.
- Digest tests showing asset byte changes and asset path changes alter the digest.
- Run-start test proving asset blob refs are written before `run.started` references
  them.
- Resume conformance test:
  - start a run with an asset;
  - commit the producer or pause before the consumer;
  - mutate the checkout asset;
  - prove resume fails on digest mismatch.
- Resume staging conformance test:
  - use a focused engine/input-resolution test or harness hook with a recorded
    `RunStartedData.Assets` manifest and mutated live asset bytes;
  - do not bypass the CLI digest check in production code;
  - prove the staged bytes are read from the recorded blob refs, not from the live
    asset files.
- Artifact validation tests for JSON parse failure, JSON Schema failure, JSONL line
  parse failure, JSONL line schema failure, schema refs, and inline schemas.
- Commit atomicity test proving invalid artifacts do not produce `node.completed`.

### Slice 3: named aggregate products

Contract:

- Add `map.id`.
- Document that it names the aggregate product, not the node path.
- Document duplicate id validation.
- Document reduced and unreduced ref behavior.

Implementation:

- Extend `ir.Map` with `ID string`.
- Extend structural validation to collect step ids and aggregate product ids into
  one duplicate-check pass.
- Add a map product index by id.
- Extend template ref validation so `step.<map.id>` is treated as a producer:
  - reduced maps expose reducer schema and files;
  - unreduced maps expose aggregate typed arrays only in another map's `over:`;
  - unreduced map `.files` refs are invalid.
- Extend engine scope resolution to resolve `step.<map.id>` through the map path.
- Extend artifact path resolution so `step.<map.id>.files.<name>` resolves to the
  reducer artifact for reduced maps.
- Keep existing body-step reduce aliases intact.

Tests:

- Validator rejects duplicate step id and map product id.
- Validator rejects duplicate map product ids.
- Validator allows `step.<map.id>.<field>` in another map's `over:` for unreduced
  maps where the body-step aggregate form is already legal.
- Validator rejects unreduced `step.<map.id>.files.<name>`.
- Engine test or conformance test resolves `step.version_universe.files.item4`
  through a reducer artifact.
- Backward compatibility conformance test keeps existing
  `step.<bodyStep>.files.<name>` reduce refs passing.
- Resume conformance test proves named aggregate artifact refs resolve from folded
  committed reducer output, not recomputation.

## 5. Acceptance criteria

- `awf run --backend auto` is accepted.
- `awf run` with no backend flag behaves as `auto`.
- `run.started.Backend` always contains a concrete backend value.
- `awf resume` uses the recorded backend and rejects native logs as non-resumable.
- Docker-only feature workflows select Docker under `auto`.
- Simple workflows select native under `auto`.
- Declared asset bytes and paths change the workflow digest.
- Asset bytes are stored in blobs at run start and referenced from `run.started`.
- Step asset staging reads from the run-start asset blob refs.
- Resume fails on changed workflow or asset bytes.
- Named aggregate products expose `step.<map.id>` typed refs.
- Reduced named aggregate products expose `step.<map.id>.files.<name>`.
- Existing body-step reduce refs keep working.
- Duplicate step and aggregate product ids are rejected.
- JSON and JSONL artifact contracts fail invalid captures mechanically before
  commit.
- Shape-only validation scripts in AWF fixtures/examples are removed where artifact
  contracts fully replace them.
- `make lint test` passes.
- Conformance coverage for resume, artifact refs, and invalid artifact contracts
  passes against the fake backend.

## 6. Risks and mitigations

**Risk: assets weaken pinning.** They do not. Asset bytes are digest input. Resume
still fails on drift before using the recorded asset snapshot.

**Risk: `auto` hides backend changes.** `auto` is never recorded. The concrete
backend is visible in `run.started`, `awf inspect`, and resume behavior.

**Risk: `map.id` looks like node addressing.** The man page must call it an
aggregate product id. Engine journal and OTel paths remain position-based.

**Risk: artifact validation duplicates `output_schema`.** It validates file
artifacts only. Typed outputs remain `$AWF_OUTPUT` plus `output_schema`.

**Risk: schema asset refs create a second asset use path.** They still use the same
declared asset snapshot and digest path as staged input assets.

## 7. Open implementation notes

- Prefer adding small pure helpers over new interfaces:
  - `cli.resolveAutoBackendKind`;
  - `ir.AssetFiles` or similar loader-owned manifest helpers;
  - `engine.ValidateOutputFileContracts`;
  - `ir.MapProductIndex`.
- Keep the dispatcher state-free. Asset refs should be resolved from data passed in
  through `ResolvedInputs` or an immutable dispatcher field, not by reading the log.
- Keep all blob writes before the journal event that points at them.
- Add conformance before implementation for every resume or commit-order behavior.
