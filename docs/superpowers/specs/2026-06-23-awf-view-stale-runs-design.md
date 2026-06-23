# Viewing previous runs after the workflow file changed (UI)

**Date:** 2026-06-23
**Status:** approved (design)
**Surface:** `awf ui`

## Problem

`awf ui <path>` becomes blind to a workflow's own history the moment you edit the file.
Two distinct couplings to the *current* file cause this:

1. **List scoping (the reported symptom).** `ui/runs.go:47` filters the run list by exact
   content digest — `if digest != wantDigest { continue }`, where `wantDigest` is the hash of
   the file passed to `awf ui` (`cli/ui.go:46`). Any edit changes the digest, so every prior
   run is silently skipped: the UI shows an empty list, with no error.

2. **Detail rendering.** `ui/server.go:137` renders a selected run by overlaying its events onto
   `graph.BuildWithRunLoaded(s.ld, events)` — the graph of the **currently loaded** file. A run's
   executed steps still surface (they come from the event log as instance nodes,
   `graph/instances.go:46-62`), but the *template skeleton* reflects the new file, not the
   version the run actually executed against.

This is purely a UI/display concern. It is **not** the §8 pinning gate — that gate lives only on
`awf resume` (`cli/resume.go:254`) and `awf outputs --workflow` (`cli/outputs.go:174`), and stays a
hard error. Indeed `awf ls`, `awf inspect`, and `awf trace` already show these runs fine because
they read only the event log.

## Goal

After editing a workflow file, `awf ui <path>` still lists every previous run of *that* workflow and
renders each one faithfully against the exact definition it ran on.

## Identity model (why "same workflow" is well-defined)

Three independent axes already exist per run, recorded in `run.started`:

- **digest** (`WorkflowDigest`) — content hash; changes on every edit.
- **workflow id** (`WorkflowID` ← `Workflow.ID`, the required top-level `workflow:` field; man page
  awf-workflow(5) line 56: *"a stable identifier for the workflow"*) — constant across edits.
- **filename** — irrelevant to identity.

"Same workflow, all versions" = same `workflow:` id. This is what the new list filter keys on.

## Invariants preserved (do not break)

- **Resume/pinning is untouched and snapshot-blind.** `resume` and `outputs --workflow` continue to
  re-read the live file, recompute the digest, and hard-error on §8 drift. The definition snapshot
  introduced below is **read-only-view-only** and MUST NEVER be consulted to resume against a changed
  file. A conformance assertion guards this: drift still errors *with* a snapshot present.
- **Commit ordering.** The snapshot blob is materialized in `Blobs` **before** the `run.started`
  event is appended (content-address-then-pointer-swap), exactly like input/asset snapshots.
- **Interpreter is the only writer to `state`.** The snapshot is written on the existing run-start
  path in `cli/run.go`; no new writer is introduced.

## Design

### Part 1 — list runs by workflow id, badge other versions

- `ui.RunRow` gains `VersionMatch bool` (`json:"version_match"`): true iff the run's recorded digest
  equals the current file's digest.
- `listRuns(stateDir, wantID, wantDigest)` includes a run iff:
  `wfID == wantID || (wfID == "" && digest == wantDigest)`.
  The second arm preserves pre-6.1 logs (which have an empty `WorkflowID`): they remain visible
  exactly when the file is unchanged, as today. `VersionMatch = (digest == wantDigest)`.
- `ui.Server` gains `workflowID string`; `NewLoaded`/`New` accept it (or derive it from the loaded
  workflow). `handleRuns` passes both id and digest. `cli/ui.go` passes `ld.Workflow.ID` (the same
  value `cli/run.go:367` records into `run.started`).
- Frontend: `ui/src/App.tsx` `RunRow` interface gains `version_match`; the run `<option>`
  (App.tsx:163-165) appends a marker (e.g. `· ⚠ other version`) when `!version_match`. Rebuild the
  embedded SPA with `make ui`.

### Part 2 — snapshot the full definition per run; render against it

- **Schema.** `engine.RunStartedData` gains
  `DefinitionRef string json:"definition_ref,omitempty"` — a CAS blob ref to the run's full
  canonical definition. Empty in logs from before this change (mirrors `StructuralDigest`,
  `WorkflowID`, `InputFiles` — each an `omitempty` field added with a graceful pre-X fallback).
- **Write (run start).** New helper `engine.StoreRunStartedDefinitionSnapshot(blobs state.Blobs,
  ld *ir.LoadedDefinition) (string, error)` = `blobs.Put(json.Marshal(ld))`, returning the ref
  (mirrors `engine.StoreRunStartedAssetsForLoadedDefinition`). `cli/run.go` calls it after blobs are
  open (~line 295, beside the asset snapshot) and before the `run.started` append (~line 380), then
  sets `DefinitionRef` in the `RunStartedData` (~line 363).
  CAS dedup means N runs of an unedited workflow share **one** blob.
- **Read (UI).** `ui.Server` gains a `state.Blobs` handle (opened by `cli/ui.go` from
  `<stateDir>/blobs`, injectable in tests). In `projectionFor`, after `FoldFile(events)`:
  extract `definition_ref` from the run's `run.started` (helper `runDefinitionRef(events)` beside
  `runMeta`); if present, `blobs.Get(ref)` → `json.Unmarshal` into `ir.LoadedDefinition` → render
  with `graph.BuildWithRunLoaded(&ld2, events)`. If the ref is absent, or the blob is
  missing/unreadable/unparseable, **fall back** to `graph.BuildWithRunLoaded(s.ld, events)` (today's
  behavior — never error the view). The `(size,mtime)` projection cache is unchanged.

#### Round-trip feasibility — RETIRED

`ir.LoadedDefinition` round-trips through JSON byte-identically: a spike loaded a real import+call
tree, `json.Marshal`→`Unmarshal`→`BuildStaticLoaded`, and the projection was identical (no-imports
and imports cases; snapshots ~1.3–1.5 KB). All node kinds are covered because `ir.NodeList`'s
marshalers and `unmarshalNode` (`ir/node_unmarshal.go`) are exact inverses (step nodes flat, control
nodes wrapped). `LoadedDefinition`/`LoadedModule` fields are all exported (default JSON marshaling).

## Backward compatibility

- **Pre-snapshot runs** (no `definition_ref`): detail view falls back to the current-file overlay —
  identical to today. They still appear in the list (by id, or by digest for pre-6.1 logs).
- **Pre-6.1 runs** (empty `WorkflowID`): visible iff the file is unchanged (digest arm), as today.

## Build order (TDD)

1. **engine** — `RunStartedData.DefinitionRef` + `StoreRunStartedDefinitionSnapshot` + tests
   (round-trip helper; `RunStartedData` marshal/unmarshal carries the field).
2. **cli/run.go** — write the snapshot at run start; test the run.started carries a resolvable ref.
3. **ui** — `Server` blobs handle + `projectionFor` snapshot load + fallback; tests prove faithful
   old-structure render and graceful fallback.
4. **ui** — by-id list filter + `version_match`; update `ui/server_test.go:191-214`; App.tsx badge +
   `make ui`.
5. **conformance + docs** — fake-backend test (run writes a resolvable `definition_ref`; drift still
   hard-errors with a snapshot present); `docs/runtime-design.md` documents the field; `man/awf.1.md`
   `awf ui` section notes cross-version visibility + faithful rendering (via updating-the-manual).

## Verification

`make lint test build` + conformance green at the end; `make ui` regenerates the embedded SPA;
real-binary smoke: run a workflow, edit it, confirm the run still lists (badged) and renders against
its original structure, and that `awf resume` still refuses the drift.

## Out of scope

- Faithful `awf graph --run` / `awf inspect` against the snapshot (same blob enables it later; not
  the reported problem).
- Any change to resume/pinning semantics.
- Using the snapshot for anything other than read-only rendering.
