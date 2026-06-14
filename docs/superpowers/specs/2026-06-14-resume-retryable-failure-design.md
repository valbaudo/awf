# Resuming transiently-failed runs (`retryable_failure`)

**Date:** 2026-06-14
**Status:** Design — pending author review (spec only; no implementation plan yet)
**Scope:** A (plain-step relaxation) **+** B (failed map items re-run)

---

## 1. Problem

Today `awf resume` refuses any run that reached a terminal `run.finished` event,
regardless of the rolled-up outcome. A run that died because its frontier step
**exhausted its retry budget on a transient fault** (`run.finished{retryable_failure}`)
is treated identically to a run that succeeded (`ok`), failed deterministically
(`permanent_failure`), or was rejected by the gate (`rejected`): all are refused,
forcing a brand-new run.

A fresh run mints a new run id and a new log, so it re-pays **all** prior work —
including expensive, already-committed setup steps (e.g. a `prepare_lab` step) —
even though those artifacts are durably committed in the failed run's log and are
mechanically replayable.

This is the one genuinely arbitrary edge in the resume policy. `permanent_failure`
("not retryable" by definition) and `rejected` (repair budget exhausted) are
correctly terminal: re-running changes nothing. But `retryable_failure` is, by its
own definition, **transient** — the exact case where re-running the identical
frontier could succeed. Lumping it with the permanent outcomes is the bug.

### Motivating distinction

`retryable_failure` is not one thing. Where the failure occurred changes both the
log shape and whether resume can currently help:

| Failure site | Terminal log tail | Re-runs on resume today? |
|---|---|---|
| **Plain step** exhausts retries | `node.failed{retryable_failure}` + `run.finished{retryable_failure}` | **Would** — the step never committed (only `ok` commits), so it is not in `Completed` and would re-execute fresh. Blocked only by the CLI guard. |
| **Map** misses `min_success` | per-item `map.item{ItemFailed}` commits + `run.finished{retryable_failure}` (propagated; no map-level `node.failed`) | **No** — `map.go:159-162` replays every committed item's status verbatim, and `committed` includes `ItemFailed`. Even past the guard, resume re-tallies the same pass/fail and re-fails identically. |

So the work has two layers:

- **Scope A** — relax the CLI guard so a `retryable_failure` run is *admitted* back
  to resume. Fixes the plain-step case completely (the `prepare_lab` scenario).
- **Scope B** — make re-entry *productive for maps* by re-running the transiently
  failed items instead of replaying them as failed.

A admits; B makes re-entry progress. They compose.

---

## 2. Background — current behaviour (evidence)

- **Guard:** `cli/resume.go:145-171` scans the folded event slice and refuses on the
  first of `run.finished` → `run.cancelled` → `node.failed`, each with a distinct
  message. The `node.failed` arm's comment is self-described as provisional:
  *"Phase 2 does not resume past a failed step; Phase 3's try/catch will revisit this."*
- **Terminal emission:** `cli/execute.go:109-124` writes `run.finished{Outcome}` and
  fsyncs it for **any** non-empty rolled-up outcome — `ok`, `retryable_failure`,
  `permanent_failure`, `rejected`. The outcome→exit switch (`:126-144`) maps
  retryable/permanent/rejected to `ExitRunFailed` (exit 1).
- **Outcome rollup:** `interpNodes` returns on the first non-`ok` node, propagating
  that node's outcome up to `engine.Run` (`interpreter.go:188-194`). So the run-level
  `run.finished.Outcome` equals the first failing node's outcome.
- **Plain-step failure:** `failStep` (`interpreter.go:502-530`) appends
  `node.failed{Outcome}` + fsync. The failed step does **not** commit (`node.completed`
  is `ok`-only; `fold.go:183-186` rejects a non-`ok` `node.completed` as corruption), so
  on resume it is absent from `Completed` and re-executes.
- **Map failure:** a non-reduce map returns `OutcomeRetryableFailure` when
  `pass < min_success` (`map.go:322-327`) — a plain `return`, **not** `failStep`, so
  there is no map-level `node.failed`. Per-item terminal status is committed as
  `map.item{N, Status}` (`commitMapItem`, `map.go:514-534`), where `Status` is
  `ItemPassed` / `ItemFailed` / `ItemPruned` — a two-value tally that **does not record
  the item's underlying outcome** (`MapItemData`, `events.go:119`). An item that
  exhausted retries and one that hit a `non_retryable_exit_code` both record
  `ItemFailed`.
- **Map resume reconciliation:** `map.go:95-162` folds every prior `map.item` into
  `committed[N]` (no status filter) and, for any `committed[i]`, replays the status and
  `continue`s — the item never re-runs. `MapItemRecord`'s own comment: *"committed map
  items are replayed-as-skipped on resume and never re-boot."*
- **Fold:** `fold.go:280-294` appends a `MapItemRecord` per `map.item` event with **no
  dedup by N**.

---

## 3. Goals / non-goals

**Goals**

1. A run whose terminal rollup is `retryable_failure` is resumable via **bare
   `awf resume`** (the trigger model chosen: automatic, no new flag).
2. Resuming a `retryable_failure` run re-executes the transiently-failed frontier —
   for plain steps (Scope A) and for map items (Scope B) — while replaying all
   committed `ok` work unchanged.
3. `permanent_failure`, `rejected`, `ok` (already finished), and `cancelled` stay
   refused, unchanged.
4. No new side-effect hazard beyond what crash-resume already accepts (re-running an
   uncommitted frontier).

**Non-goals**

- No `try`/`catch`/`finally` work (that is the separate, larger recovery story this
  edge was deferred to). This spec is the narrower, correct relaxation.
- No change to the *original* run's behaviour: it still writes
  `run.finished{retryable_failure}` and exits 1. The operator sees the failure and
  chooses to resume.
- No resume-attempt cap / loop detection. Each resume is operator-driven; there is no
  automatic re-resume, so no unbounded loop.
- No reduce-map quorum changes beyond what falls out of item re-run (call out in §6.4).

---

## 4. Design — Scope A: relax the guard

Make the two refusal loops in `cli/resume.go:145-171` **outcome-aware** instead of
event-type-blanket.

```go
// run.finished: refuse UNLESS the run rolled up a transient (retryable) failure.
for _, e := range events {
    if e.Type == engine.EventRunFinished {
        d, err := engine.RunFinishedDataFromEvent(e)   // new tiny accessor in engine
        if err != nil {
            fprintf(stderr, "awf resume: run %q has a corrupt run.finished record: %v\n", runID, err)
            return ExitUsage
        }
        switch engine.Outcome(d.Outcome) {
        case engine.OutcomeRetryableFailure:
            // resumable: re-run the uncommitted frontier (a transient fault
            // exhausted the step's / map's retry budget). Same idempotency
            // contract crash-resume already relies on.
        case engine.OutcomeOK:
            fprintf(stderr, "awf resume: run %q already finished (ok). Nothing to resume.\n", runID)
            return ExitUsage
        default: // permanent_failure, rejected
            fprintf(stderr, "awf resume: run %q ended with a terminal %s. Not resumable; start a new run id.\n", runID, d.Outcome)
            return ExitUsage
        }
        break
    }
}
// run.cancelled: unchanged — terminal, refuse.
// node.failed: refuse ONLY on a permanent failure; a retryable node.failed re-runs.
for _, e := range events {
    if e.Type == engine.EventNodeFailed {
        d, err := engine.NodeFailedDataFromEvent(e)
        if err == nil && engine.Outcome(d.Outcome) == engine.OutcomePermanentFailure {
            fprintf(stderr, "awf resume: run %q terminated on a permanent failure at path %q. Not resumable.\n", runID, e.Path)
            return ExitUsage
        }
    }
}
```

Notes:

- The `run.finished.Outcome` check is the **primary** decision; because `interpNodes`
  returns on the first non-`ok`, the rollup equals the first failing node's outcome, so
  a `retryable_failure` rollup cannot co-exist with an earlier `permanent_failure`
  node. The `node.failed{permanent}` arm is belt-and-suspenders for the rare crash
  *between* `failStep` and the CLI writing `run.finished` (a `node.failed` with no
  `run.finished`).
- On the relaxed path, print an operator line:
  `run X previously failed transiently (retryable_failure); re-attempting the uncommitted frontier`.
- **`cli/execute.go` is untouched.** Pinning hard-errors (digest / runtime drift),
  backend-from-log, and `run.resumed{epoch+1}` append are all unchanged.

**New code:** `engine.RunFinishedDataFromEvent` and `engine.NodeFailedDataFromEvent`
accessors (thin `json.Unmarshal` wrappers, mirroring the existing
`RunStartedDataFromEvents`).

**Surface area:** `cli/resume.go` + the two accessors. No engine control-flow change —
the plain-step frontier already re-executes once admitted.

---

## 5. Design — Scope B: re-run transiently-failed map items

For a map's `retryable_failure` to recover, transiently-failed items must re-run on
resume; permanently-failed (and passed, and pruned) items must not. Three changes:

### 5.1 Record the item outcome (events)

Extend `MapItemData` (`events.go:119`) with an additive, `omitempty` field carrying the
item body's rolled-up outcome **on failure**:

```go
type MapItemData struct {
    N      int    `json:"n"`
    Status string `json:"status"`
    // Outcome is the item body's rolled-up mechanical outcome when Status ==
    // ItemFailed: "retryable_failure" | "permanent_failure". Empty for ItemPassed
    // / ItemPruned, and for pre-this-change logs. Drives resume re-run selection:
    // only a retryable item re-runs; a permanent item is replayed-as-failed.
    Outcome string `json:"outcome,omitempty"`
    ImageDigest string `json:"image_digest,omitempty"`
    Reason      string `json:"reason,omitempty"`
}
```

`commitMapItem` / `runItem` pass the body's `bodyOC` through when `status == ItemFailed`
(`map.go:490-511`). Mirror the field on `MapItemRecord` (`runstate.go:95`) and fold it
(`fold.go:288` and the `map.frontier` arm `:308`).

This keeps the two-value `Status` tally and `min_success` math untouched — `Outcome` is
a resume-selection input only, analogous to how `Reason` was added additively (P6a).

### 5.2 Dedup the fold by item-N (last-wins)

A re-run item appends a **second** `map.item{N}`. `fold.go:288` currently appends
without dedup, so `MapItems[mapPath]` would carry two records for N. Change the fold to
keep last-wins per N (replace-in-place by N rather than blind append) for both the
`map.item` and `map.frontier` arms. This also makes the existing `H8`
non-deterministic-`over` check (`map.go:111`) and the tally robust to re-commits.

### 5.3 Re-run retryable items in reconciliation

In `runMap`'s resume reconciliation (`map.go:95-162`), an item is **skip-and-replay**
only if its latest folded status is `ItemPassed` / `ItemPruned`, **or** `ItemFailed`
with `Outcome == permanent_failure`. An `ItemFailed` with `Outcome == retryable_failure`
falls through and re-runs like any uncommitted item (re-deriving `ItemValue` from `over`
as today). On re-run it commits a fresh `map.item{N}` (pass → recovered; fail → updated
record), and §5.2 keeps the fold single-valued.

The map then re-tallies (`map.go:318-327`): if enough retryable items recover to meet
`min_success`, the map returns `ok` and the run proceeds. A map of all-permanent
failures that rolled up `retryable_failure` (because `min_success` wasn't met) gains
nothing from resume — correct, there is nothing transient to recover.

### 5.4 Consistency framing (why this is right, not just a feature)

Plain steps never commit a failure (`node.completed` is `ok`-only; failures are a
separate `node.failed` record that halts). Maps are the lone place that durably commits
failures (`map.item{ItemFailed}`). Scope B narrows that asymmetry: a **transiently**
failed item stops being a durable commit and rejoins the uncommitted frontier, matching
the rest of the system's "only real results commit; transient faults re-run" rule. A
**permanently** failed item stays committed (it is a settled negative result that counts
against `min_success`) — the analogue of a plain step's `node.failed`.

---

## 6. Open decisions for author review

1. **Item-outcome granularity (§5.1).** Recommended: store `Outcome` on `map.item` and
   re-run only retryable items. Alternative (simpler, rejected): re-run *all*
   `ItemFailed` items, accepting that permanent ones re-run once per resume and re-fail.
   The recommended option preserves the retryable/permanent contract at item
   granularity; confirm.
2. **Uniform vs. terminal-only re-run.** Recommended: the reconciliation rule (§5.3)
   applies to **every** resume of the map (crash-resume too), not just a terminally
   `retryable_failure` run. This is uniform and strictly better — a crash that left some
   items transiently failed would now recover them — but it *is* a behaviour change to
   crash-resume of maps. Confirm we want the uniform rule.
3. **Old-log default for absent `Outcome`.** A pre-change log's `ItemFailed` records have
   no `Outcome`. Recommended conservative default: treat absent as **non-rerunnable**
   (replay-as-failed), so an in-flight upgrade never surprises an operator by re-running
   items it previously skipped. Confirm.
4. **Infra-unavailable items (`ReasonImageUnavailable`, `map.go:462`).** These are
   tolerated `ItemFailed` with no body outcome. Are they retryable (the image might boot
   next time → re-run) or permanent (replay-as-failed)? Recommended: classify as
   retryable, since image availability is a transient/infra condition — but this is a
   distinct judgement call from a body retry exhaustion. Confirm.
5. **Reduce maps.** Item re-run changes the set of passed items feeding a quorum reduce.
   The reducer already re-runs on resume when uncommitted; confirm no additional change
   is needed beyond items now possibly flipping fail→pass before the reduce.

---

## 7. Invariants preserved

- **Commit = content-address-then-pointer-swap.** Unchanged. Re-running a retryable item
  goes through the same `commitMapItem` (artifact-then-event). A "done" record never
  references a missing artifact.
- **Only `ok` `node.completed` commits.** Unchanged. Scope B touches `map.item` (which
  always committed both pass and fail); it does **not** introduce non-`ok`
  `node.completed`.
- **Resume folds the log; committed work is replayed, not recomputed.** Preserved for
  `ok` work. Scope B reclassifies *transiently-failed* items as uncommitted frontier
  (re-run), which is the same treatment a transiently-failed plain step already gets.
- **Crash ≠ verdict.** Untouched — the gate path is not modified.
- **Retry ≠ repair.** This is pure retry (re-run identical work after a transient fault,
  no feedback); no gate/repair semantics change.
- **Determinism / pinning.** Unchanged. Digest + runtime drift remain hard errors on
  resume; the `H8` non-deterministic-`over` guard is retained (and hardened by §5.2).
- **The interpreter is the only writer to state.** Unchanged; re-commits go through the
  existing map commit path.

---

## 8. Testing plan (fake backend — no Docker)

**Scope A (unit, `cli/resume_test.go`):**

- `run.finished{retryable_failure}` → resume **proceeds** (currently asserted to refuse;
  flip the expectation) and the frontier step re-runs to `ok` when the dispatcher
  succeeds on re-run.
- `run.finished{permanent_failure}` → refused, distinct message.
- `run.finished{rejected}` → refused.
- `run.finished{ok}` → refused ("already finished").
- `node.failed{permanent_failure}` with no `run.finished` (crash window) → refused.
- `node.failed{retryable_failure}` with no `run.finished` → resumes.

**Scope A (conformance):** workflow with a setup step whose dispatcher fails transiently
and exhausts retries → run rolls up `retryable_failure`, exit 1 → `awf resume` with a
now-succeeding dispatcher → frontier re-runs, run completes `ok`; assert the committed
setup step was **not** re-run (replayed from `Completed`).

**Scope B (conformance):** map of N items, `min_success` = M; on the first run, some
items pass and ≥1 transiently fail so `pass < M` → `run.finished{retryable_failure}`. On
resume with those items now succeeding: passed items are **not** re-run, retryable items
**are** re-run, the tally reaches `min_success`, map → `ok`, run → `ok`. Plus: a map
with a `permanent_failure` item that keeps `pass < M` → resume re-runs only the retryable
items, the permanent item stays failed, and if `min_success` still unmet the run
re-fails `retryable_failure` (idempotent re-fail, no duplicate `MapItems[N]` thanks to
§5.2).

The conformance suite must stay green at the phase boundary (project rule).

---

## 9. Docs to update

Run the `updating-the-manual` skill for:

- `man/awf-workflow.5.md` — document the failed-vs-resumable rule explicitly in the
  CHECKPOINTING AND RESUME / Pause vs Cancellation sections (closes the current gap
  where this rule lives only in `cli/resume.go` comments): a `retryable_failure` run is
  resumable; `permanent_failure` / `rejected` / `cancelled` / `ok` are not.
- `man/awf.1.md` — `awf resume` description: note it now also resumes a transiently
  failed run and re-runs the transiently-failed frontier (steps and map items).
- `README.md` — the resume row of the "how AWF is different" table, if it asserts
  failed-runs-aren't-resumable.
- Update the stale `cli/resume.go` `node.failed` comment
  ("Phase 3's try/catch will revisit this").

---

## 10. Out of scope

- `try` / `catch` / `finally` recovery.
- Resume-attempt caps / automatic re-resume.
- Distinguishing infra vs. result beyond the existing `Reason` field (except the §6.4
  classification call).
- Any change to permanent / rejected / cancelled / ok handling.
