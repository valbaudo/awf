# Resuming transiently-failed runs (`retryable_failure`)

**Date:** 2026-06-14
**Status:** Design — pending author review (spec only; no implementation plan yet).
Revised after a self-critique + a code-grounded / prior-art cross-reference pass; the
five prior "open decisions" are now resolved (§7).
**Scope:** A (admit a transiently-failed run; re-run its uncommitted frontier) **+**
B (re-run transiently-failed **plain-map** items). Prune and reduce maps are handled
explicitly (§6.4, §6.5); neither needs new re-run machinery.

---

## 1. Problem

Today `awf resume` refuses any run that reached a terminal `run.finished` event,
regardless of the rolled-up outcome. A run that died because its frontier
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

### Prior art (this is a known, shipped pattern — not a novel risk)

"Resume a failed run by re-running only the failed frontier, replaying successful
steps from their recorded results" is the dominant industry pattern:

- **AWS Step Functions "Redrive"** (GA 2023-11): eligibility is gated on the terminal
  status (*"You can redrive executions if your original execution attempt meets these
  conditions: The execution status isn't SUCCEEDED"*); it *"preserves the results and
  execution history of the successful steps... not rerun"*; and for Map/Parallel it
  *"redrives only the iterations and branches that failed or aborted"* — i.e. Scope A
  and Scope B respectively. Redriven executions must use the same definition (= AWF
  digest pinning). A redriven step is billed twice (= AWF re-paying a frontier; still
  cheaper than a fresh run).
- **Azure Durable Functions Rewind**, **Temporal/Cadence Reset**, **DBOS/Restate**
  ("resume from last completed step") are the same pattern. All draw the same two axes
  AWF already has: interrupted-vs-terminal (different recovery ops) and
  transient-vs-permanent (a classification that gates whether recovery can help).

Two precedents shape the design below: (a) every system gates eligibility on the
**terminal outcome class** at the recovery primitive (§5), and (b) no system recomputes
a **top-k / quorum / aggregate** over a *partial* fan-out result set on recovery —
they make that decision atomic and recompute over the full set, or forbid partial
(Hadoop aborts duplicate commits; Spark fails the result stage). That second precedent
is why prune maps are excluded from per-item re-run (§6.4).

### Motivating distinction

`retryable_failure` is not one thing. Where the failure occurred changes both the
log shape and whether resume can currently help:

| Failure site | Terminal log tail | Re-runs on resume today? |
|---|---|---|
| **Plain step (or any non-map frontier node)** exhausts retries | `node.failed{retryable_failure}` + `run.finished{retryable_failure}` | **Would** — the node never committed (only `ok` commits), so it is absent from `Completed` and would re-execute fresh. Blocked only by the CLI guard. |
| **Plain map** misses `min_success` | per-item `map.item{ItemFailed}` commits + `run.finished{retryable_failure}` (propagated; no map-level `node.failed`) | **No** — `map.go:159-162` replays every committed item's status verbatim, and `committed` includes `ItemFailed`. Even past the guard, resume re-tallies the same pass/fail and re-fails identically. |

So the work has two layers:

- **Scope A** — relax the CLI guard so a `retryable_failure` run is *admitted* back
  to resume. Once admitted, the engine already re-runs the entire **uncommitted
  frontier**, for any node kind (§5.2). Fixes the `prepare_lab` scenario completely.
- **Scope B** — make re-entry *productive for plain maps* by re-running the transiently
  failed items instead of replaying them as failed.

A admits; B makes re-entry progress for plain maps. They compose.

---

## 2. Background — current behaviour (evidence)

All line cites verified against HEAD of branch `docs/resume-retryable-failure-spec`.

- **Guard:** `cli/resume.go:147-171` scans the folded event slice with **three**
  separate, event-type-blanket loops, refusing on the first of `run.finished` →
  `run.cancelled` → `node.failed`, each with a distinct message (it reads only
  `e.Type`, no outcome). The `node.failed` arm's comment is self-described as
  provisional: *"Phase 2 does not resume past a failed step; Phase 3's try/catch will
  revisit this."*
- **Terminal emission:** `cli/execute.go:109-124` writes `run.finished{Outcome}` and
  fsyncs it for **any** non-empty rolled-up outcome — `ok`, `retryable_failure`,
  `permanent_failure`, `rejected`. The outcome→exit switch (`:126-144`) maps
  retryable/permanent/rejected to `ExitRunFailed` (exit 1).
- **Top-level outcome rollup:** `interpNodes` returns on the first non-`ok` node
  (`interpreter.go:188-194`). So in a **sequential** graph the run-level
  `run.finished.Outcome` equals the first failing node's outcome — but this is **not**
  a max/severity over all `node.failed` events, and it does **not** hold across
  compound nodes (see §5.1).
- **Plain-step failure:** `failStep` (`interpreter.go:507-530`, called at
  `:341`/`:346` etc.) appends `node.failed{Outcome}` + fsync. The failed node does
  **not** commit (`node.completed` is `ok`-only; `fold.go:183-186` rejects a non-`ok`
  `node.completed` as corruption), so on resume it is absent from `Completed` and
  re-executes (`interpreter.go:259-261`).
- **Plain-map failure:** a non-reduce map returns `OutcomeRetryableFailure` when
  `pass < min_success` (`map.go:322-327`) — a plain `return`, **not** `failStep`, so
  there is no *map-level* `node.failed`. Per-item terminal status is committed as
  `map.item{N, Status}` (`commitMapItem`, `map.go:519-536`), where `Status` is a
  **three-value** tally: `item_passed` / `item_failed` / `item_pruned`
  (`events.go:90-102`; note the stale `events.go:131` comment still says "two-value"
  — also to be corrected). `MapItemData` (`events.go:119-137`) records `N`, `Status`,
  `ImageDigest`, `Reason` but **no underlying outcome** — an item that exhausted
  retries and one that hit a `non_retryable_exit_code` both record `item_failed`.
- **Prune (SP5) map:** never emits `map.item`; it commits its whole disposition as
  ONE atomic `map.frontier` event in the final pass (`map.go:271-289`,
  `commitMapFrontier` `map.go:545-564`). The per-item commit is deferred
  (`dispatchItem` returns `(status, nil)` when `pr != nil`, `map.go:507-510`).
- **Map resume reconciliation:** `map.go:95-162` folds every prior `map.item` /
  `map.frontier` item into `committed[N]` (no status filter) and, for any
  `committed[i]`, replays the status and `continue`s — the item never re-runs.
  `MapItemRecord`'s own comment: *"committed map items are replayed-as-skipped on
  resume and never re-boot."*
- **Fold:** `fold.go:288` (`map.item` arm) and `fold.go:308` (`map.frontier` arm)
  both **append** a `MapItemRecord` per event with **no dedup by N**.
- **Live insert:** `RunState.RecordMapItem` (`runstate.go:515-519`) blind-appends; the
  by-N mutators `updateMapItemStatus` (`map.go:570-580`) and `UpdateMapItemValue`
  (`runstate.go:529-539`) match the **first** record for N.

---

## 3. Goals / non-goals

**Goals**

1. A run whose terminal rollup is `retryable_failure` is resumable via **bare
   `awf resume`** (trigger model: automatic, no new flag).
2. Resuming re-executes the transiently-failed uncommitted frontier — for any node
   kind (Scope A) and for plain-map items (Scope B) — while replaying all committed
   `ok` work unchanged.
3. `permanent_failure`, `rejected`, `ok` (already finished), and `cancelled` stay
   refused, unchanged.
4. No new side-effect hazard **mechanism** beyond what crash-resume already accepts
   (re-running an uncommitted frontier). The honest at-least-once consequences are
   documented (§9), not papered over.

**Non-goals**

- No `try`/`catch`/`finally` work (the separate, larger recovery story this edge was
  deferred to). This spec is the narrower, correct relaxation.
- No change to the *original* run's behaviour: it still writes
  `run.finished{retryable_failure}` and exits 1.
- No resume-attempt cap / DLQ analog. Each resume is operator-driven (§9 documents
  the tradeoff honestly).
- No per-item re-run for **prune** maps (§6.4) and no new machinery for **reduce**
  maps (§6.5 — safe by construction).

---

## 4. Resumability contract (normative)

This is the load-bearing rule the guard, the `awf ls` status, the man page, and the
tests all trace to. It covers the crash window §5 relies on:

> A run is resumable by `awf resume` **iff** its terminal rollup is
> `retryable_failure` — i.e. the log contains `run.finished{retryable_failure}`, **or**
> (the `failStep`→`run.finished` crash window) its only terminal marker is
> `node.failed{retryable_failure}` with no `run.finished`.
> `ok`, `permanent_failure`, `rejected`, and `cancelled` are **not** resumable.
>
> **Crash-window asymmetry (deliberate):** with no `run.finished` there is no rollup to
> trust, so a log whose only terminal marker is a *permanent* (or empty) `node.failed`
> is refused — even a tolerated nested permanent `node.failed` (a map item) whose run
> *would* have rolled up `retryable_failure`. The identical shape *with* `run.finished`
> is admitted. This conservative refuse matches the pre-change behavior (no regression);
> the operator's recourse is a fresh run id.

This is the AWF spelling of Step Functions' redrive-eligibility predicate (eligible iff
status ≠ `SUCCEEDED`, refined here to a whitelist because AWF distinguishes transient
from permanent failure). It must be stated in `man/awf-workflow.5.md` and reflected in
the `awf ls` status vocabulary (§6.6), co-landed with the code (§10).

---

## 5. Design — Scope A: admit a transiently-failed run

### 5.1 Relax the guard — `run.finished.Outcome` is the sole authority

Replace the three event-type-blanket loops at `cli/resume.go:147-171`. The fix is
**not** "make two loops outcome-aware while leaving a standalone `node.failed{permanent}`
loop": that standalone loop would *refuse a run the `run.finished` arm just admitted*,
because the rollup is **not** a severity-max over all `node.failed` events. Three
compound cases produce `run.finished{retryable_failure}` co-existing with a durable
`node.failed{permanent_failure}` at a nested path:

- **Tolerated permanent map-item body:** a permanent step inside a map item writes
  `node.failed{permanent_failure}` at the item-body path (`failStep`,
  `interpreter.go:507-530`), but `map.go:500-504` absorbs it into `ItemFailed`; the map
  rolls up `retryable_failure` via `map.go:322-327`. (This is the **common** case —
  `min_success` defaults to "all required", so a single permanent item among transient
  failures is normal.)
- **Parallel:** `parallel.go:188-194` deterministically picks the **lowest-index**
  branch error as the rollup; branch-0 retryable + branch-1 permanent ⇒ rollup
  retryable, branch-1's `node.failed{permanent}` durable.
- **try/catch:** `try.go:73-79` — a permanent `do` (durable `node.failed{permanent}`)
  with a retryable `catch` makes `runTry` return retryable.

So the correct structure makes `run.finished.Outcome` the **sole** admit/refuse
decision when a `run.finished` event is present, and consults `node.failed` **only** in
the crash window (no `run.finished`):

```go
// 1. Find the terminal run.finished (sole authority when present).
var finished *engine.RunFinishedData
for _, e := range events {
    if e.Type == engine.EventRunFinished {
        d, err := engine.RunFinishedDataFromEvent(e)   // new thin accessor
        if err != nil { fprintf(stderr, "awf resume: run %q corrupt run.finished: %v\n", runID, err); return ExitUsage }
        finished = &d
        break
    }
}

// 2. run.cancelled stays a blanket terminal refuse, checked BEFORE the node.failed
//    fallback (preserves the cancel-during-step precedence in resume_test.go).
for _, e := range events {
    if e.Type == engine.EventRunCancelled { fprintf(stderr, "awf resume: run %q was cancelled ...\n", runID); return ExitUsage }
}

if finished != nil {
    switch engine.Outcome(finished.Outcome) {
    case engine.OutcomeRetryableFailure:
        fprintf(stderr, "awf resume: run %q eligible for resume (retryable_failure); attempting the uncommitted frontier\n", runID)
        // ADMIT. Do NOT scan node.failed.
    case engine.OutcomeOK:
        fprintf(stderr, "awf resume: run %q already finished (ok). Nothing to resume.\n", runID); return ExitUsage
    default: // permanent_failure, rejected, empty/unknown
        fprintf(stderr, "awf resume: run %q ended with a terminal %s. Not resumable; start a new run id.\n", runID, finished.Outcome); return ExitUsage
    }
} else {
    // CRASH WINDOW only: no run.finished. node.failed is the terminal record.
    // Default: admit ONLY retryable; refuse everything else INCLUDING empty/absent.
    for _, e := range events {
        if e.Type == engine.EventNodeFailed {
            d, err := engine.NodeFailedDataFromEvent(e)
            if err == nil && engine.Outcome(d.Outcome) == engine.OutcomeRetryableFailure { continue }
            fprintf(stderr, "awf resume: run %q terminated on a non-transient failure at path %q. Not resumable.\n", runID, e.Path); return ExitUsage
        }
    }
}
```

Key points vs the earlier draft: (i) the `node.failed` scan runs **only** in the
`finished == nil` branch — the standalone unconditional loop is deleted, fixing the
false-refusal of compound retryable runs; (ii) the crash-window default is
**admit-only-retryable / refuse-everything-else-including-empty**, not refuse-only-
permanent; (iii) `run.cancelled` stays ahead of the `node.failed` fallback. A cancelled
run has `run.cancelled` and *no* `run.finished` (the CLI short-circuits on `ErrCancelled`
before writing `run.finished`); but a tolerated `node.failed` can co-exist with
`run.cancelled` when cancel lands during a later step, so checking cancelled first shows
"cancelled" rather than a nested-failure message.

**New code:** `engine.RunFinishedDataFromEvent` / `engine.NodeFailedDataFromEvent` —
trivial `json.Unmarshal` wrappers over `RunFinishedData` (`events.go:435`) /
`NodeFailedData` (`events.go:424`), mirroring `RunStartedDataFromEvents`. Also update
the stale `cli/resume.go` comment ("Phase 3's try/catch will revisit this").
**`cli/execute.go` is untouched** (original run still exits 1; pinning, backend-from-log,
`run.resumed{epoch+1}` unchanged).

### 5.2 Scope A is node-kind-agnostic (the frontier, not just "plain steps")

Once admitted, the engine re-runs the entire **uncommitted frontier** — this already
works for every node kind; no engine change is needed for Scope A. Each `interpNode`
kind (`interpreter.go:206-235`) re-enters correctly:

- **committed-count cursor:** gate (`startN = LookupGateAttempts+1`, `gate.go:93`;
  crash≠verdict propagates a retryable body *before* the `gate.attempt` event,
  `gate.go:111-112`), react (`startK = LookupReactRounds+1`, `react.go:52`; parse miss
  fails retryable without committing, `react.go:91-93`), loop
  (`startK = LookupLoopIters+1`, `interpreter.go:444`; body failure returns without
  emitting `loop.iter`, `interpreter.go:462-464`).
- **`LookupCompleted` short-circuit:** code/agent/signal step (`interpreter.go:259`),
  call (commits only on child-ok; on child failure `failStep` at `call_step.go:118`;
  resume replays input from `call.started` and re-walks the child graph,
  `call_step.go:38`,`:104`).
- **no completion record, re-walk children:** parallel (`parallel.go` re-walks every
  entry), compose, `if` (`branch.taken` replays the choice), try, **map** (per-item
  `committed[]` reconciliation — the Scope B subject).

**`map` is the only kind that durably commits a failure** (`map.item{ItemFailed}` /
`map.frontier`), which is exactly why Scope B is needed and the other kinds are not.
A nested-compound retryable failure can write multiple `node.failed{retryable}` records
(e.g. a call writes one at the child-step path and one at the call path); the §5.1 guard
tolerates this because it keys on `run.finished.Outcome`, never on the count of nested
`node.failed` records.

---

## 6. Design — Scope B: re-run transiently-failed **plain-map** items

For a plain map's `retryable_failure` to recover, transiently-failed items must re-run
on resume; permanently-failed, rejected, passed, and pruned items must not. Prune maps
(§6.4) and reduce maps (§6.5) are handled separately.

### 6.1 Record the item outcome (events) — and why the field is required

Re-running **all** `ItemFailed` items (the "no new field" shortcut) is **incorrect**, not
merely wasteful: a **gate nested in a map item body** returns `OutcomeRejected`
(`gate.go:183-184`), which `dispatchItem` collapses to `ItemFailed` (`map.go:500-505`).
Re-running a rejected item re-runs a gate that already exhausted its repair budget —
violating `max_attempts` ("crash ≠ verdict", "retry ≠ repair"). A permanent item re-run
is a pointless at-least-once side-effect hazard. Both are *settled negatives* that every
mature system quarantines (Temporal `NonRetryableErrorTypes`; Step Functions
`States.Runtime`/`DataLimitExceeded` cannot be caught even by `States.ALL`). So the
re-run decision needs the item's **underlying** outcome, which `map.item` does not record
today.

Extend `MapItemData` (`events.go:119-137`) with an additive, `omitempty` field:

```go
type MapItemData struct {
    N      int    `json:"n"`
    Status string `json:"status"`
    // Outcome is the item body's rolled-up mechanical outcome when Status ==
    // item_failed: "retryable_failure" | "permanent_failure" | "rejected".
    // Empty for item_passed / item_pruned, for the image_unavailable path (§6.3),
    // and for pre-this-change logs. Resume re-run selection (§6.2): ONLY a retryable
    // item re-runs; permanent / rejected / absent are replayed-as-failed.
    Outcome string `json:"outcome,omitempty"`
    ImageDigest string `json:"image_digest,omitempty"`
    Reason      string `json:"reason,omitempty"`
}
```

**Mechanism (the load-bearing detail the earlier draft skipped):** `dispatchItem`
currently returns `(status string, err error)` and **discards** `bodyOC`
(computed at `map.go:490`, used only as a bool at `map.go:500`). Change it to also carry
the body outcome — either widen its return to `(status string, bodyOC Outcome, err error)`
or capture `bodyOutcome := bodyOC` at `map.go:500-504` — and add a trailing
`outcome string` param to `commitMapItem` (`map.go:519`), written into
`MapItemData.Outcome` **only** on the non-prune body-failure path (`map.go:511`). The
stored value is constrained to `{retryable_failure, permanent_failure, rejected}` (via
`ParseOutcome`, `runstate.go:43`). Mirror the field on `MapItemRecord`
(`runstate.go:95-108`) and fold it on the **`map.item` arm only** (`fold.go:288`); the
`map.frontier` arm (`fold.go:308`) stays Outcome-free (prune is excluded — §6.4 — and
`bodyOC` is not in scope at `commitMapFrontier`). `Status` tally and `min_success` math
are untouched — `Outcome` is a resume-selection input only.

### 6.2 Single record per N — at all times, in BOTH layers

A re-run item appends a second `map.item{N}`, and the live `RecordMapItem`
(`runstate.go:515-519`) blind-appends a pending record while the fold-seeded record for
the same N is still in `MapItems`. Two records for N corrupt every **slice-keyed**
reader:

- `aggregateMapOutputs` (`map_product_scope.go:107-134`) skips only `ItemPruned`, so
  both records resolve the same `ItemStepPath` and the recovered item's typed output is
  emitted **twice** into a cross-map `{{ step.<id> }}` aggregate.
- value-binding `<as>.<field>` (`scope.go:323-340`) is first-match-wins; the fold-seeded
  record (nil `ItemValue`) can shadow the re-run's record → a "value not bound" error or
  a stale value mid-re-run. (The earlier draft wrongly claimed value-bindings were
  unaffected.)
- reduce fan-in (`collectReduceBranches`, `reduce.go:349-385`) is *incidentally* safe in
  the fail→pass case only because it filters on `ItemPassed` (the stale record is
  `ItemFailed`) — not a guarantee to rely on.

**Invariant:** `RunState.MapItems[mapPath]` holds **at most one record per N at all
times** — enforced in BOTH the fold (durable replay) and the live insert:

1. **`RecordMapItem` becomes upsert-by-N** (`runstate.go:515-519`): replace the record
   for an existing N in place, else append. This single change dedups the live slice AND
   fixes the `scope.go` value-binding staleness for free (the re-run's pending record
   carries the re-derived `ItemValue` from `map.go:167`, overwriting the nil-valued
   folded record), and makes `updateMapItemStatus` / `UpdateMapItemValue` first-match
   scans trivially correct. (Chosen over "drop the folded record before re-run", which
   needs extra control flow and doesn't fix value-binding.)
2. **Fold last-wins by N** on the **`map.item` arm only** (`fold.go:288`): replace the
   `MapItemRecord` for an existing N rather than appending, **highest-seq wins** (Fold
   walks in seq order). Required because Fold rebuilds `MapItems` from the durable log
   before `runMap` ever calls `RecordMapItem`, and a re-run appended a second
   `map.item{N}`. The `map.frontier` arm (`fold.go:308`) stays a blind append: a prune
   map commits one atomic `map.frontier` per path with distinct Ns, so per-N dedup there
   is vacuous (and prune items are excluded from re-run, §6.4) — re-pointing that
   load-bearing "frontier is never re-derived" arm would be rule-3 creep. This also keeps
   the `H8` non-deterministic-`over` guard (`map.go:111`) and the tally robust to re-commits.

External precedent: MapReduce keeps only the winning attempt per logical task; Step
Functions' `ResultWriter` re-aggregates after redrive rather than patching — the dedup
must cover every materialized view, not just the journal.

### 6.3 Re-run selection — gated on resume, keyed on outcome (and image_unavailable)

The re-run rule must fire on **resume only**, not on every map entry. `runMap`
reconciliation (`map.go:95-162`) is run-outcome-blind today, and the run-level rollup is
the **wrong** discriminator anyway (it is the first-failing-node's outcome — possibly an
unrelated plain step — `interpreter.go:188-194`). The honest discriminator is "are we
folding a prior log?":

- Add `Resume bool` to `engine.RunOptions` and a matching field to
  `interpreterContext` (`interpreter_context.go:14-29`); set it **only** at the resume
  call site (`cli/resume.go` → `executeRun`), copy into `ictx` in `engine.Run`. It
  threads through the existing `ictx` to `runMapWithContext` with no signature churn.
- This keeps fresh runs and **direct callers byte-identical**:
  `TestRunMapResumeFailedItemsStayFailed` (`map_test.go:681-729`) calls `runMap`
  without the flag, so a folded `ItemFailed` still replays-as-failed there (the pinned
  engine-level contract is preserved); the new behavior gets a test that sets
  `RunOptions.Resume=true`.

Selection predicate at the `committed[]` short-circuit (`map.go:159-162`), evaluated
over a `committed` entry carrying `{Status, Outcome, Reason}` (Reason already folded at
`fold.go:292`; Outcome added by §6.1). An item is **re-run** iff:

```
n.Prune == nil                       // prune excluded (§6.4)
&& ictx.resume                       // resume only
&& Status == item_failed
&& Reason != image_unavailable       // peeled off FIRST (§6.3 below)
&& Outcome == retryable_failure
```

Otherwise **skip-and-replay**: `item_passed`, `item_pruned`, `item_failed` with
`Outcome ∈ {permanent_failure, rejected, ""}`, and any `image_unavailable` item. This is
a **closed** predicate over the full terminal set — no fall-through.

**`image_unavailable` is resolved to NON-rerunnable for v1.** The image_unavailable item
commits *before* the body runs (`map.go:457-462`), so it has no `bodyOC` and an empty
`Outcome` — peel it off via a `Reason == image_unavailable` branch evaluated **before**
the absent-Outcome rule, so §6.3-absent and the image case stop conflicting (the old
spec's §6.3/§6.4 contradiction). Grounded: `map.go:443-451` already diverts the
genuinely-transient ctx-cancel/timeout case away from the image_unavailable commit, so
what remains (`map.go:452-453`) is a digest-pinned image that won't pull/boot — a
determinism fault that matches "pinning is a hard error on drift" and Step Functions'
"exceeded-window = permanent". (Re-runnable images can be a future toggle; it would need
a re-run precedence rule over `{Outcome, Reason}` and accepts the unbounded-loop
exposure of §9.)

On re-run the map re-tallies (`map.go:318-327`): if enough retryable items recover to
meet `min_success`, the map returns `ok` and the run proceeds. A map of all-permanent /
all-rejected failures that rolled up `retryable_failure` gains nothing — correct, there
is nothing transient to recover.

### 6.4 Prune (SP5) maps are excluded from per-item re-run

A prune map's disposition is a **global** `keep: top(k)` / `stop_when` selection over the
full score set, committed atomically as ONE `map.frontier` (`map.go:271-289`). Re-running
a single participating item would: (a) emit a **partial** `map.frontier` (only the
re-run item, since survivors stay in `committed` and are skipped) — exactly the partial
frontier the atomic commit forbids; and (b) re-decide `top(k)`/`stop_when` against an
**empty, freshly-rebuilt controller** (`prune.go:33-38`; survivors' scores are never
re-reported — `map.go:241-242` reports only freshly-passing items), yielding more
survivors than `k` allows (`prune.go:87-122`). This is the precise hazard the SP5 atomic
frontier was built to prevent, and the literature uniformly designs it out (Hadoop aborts
duplicate commits; Spark fails the result stage; no system recomputes a top-k over a
partial set).

**Rule:** the §6.3 predicate guards on `n.Prune == nil`. A prune map's items are always
skip-and-replay via the existing `committed[]` short-circuit (`map.go:159-162`),
regardless of folded status (`item_passed`/`item_failed`/`item_pruned`).

**Default for a committed-frontier prune map that rolled up `retryable_failure`** (pass <
`min_success` among non-pruned survivors): **stays terminal** on resume — it replays its
frontier verbatim and re-fails identically. (A prune map that crashed *before* its single
`map.frontier` append already re-runs the whole map from a clean slate for free,
`map.go:262-270`.) Productive per-item recovery for prune maps is deferred (it requires
re-seeding the controller from survivors' committed scores so the selection re-decides
over the full set — a separate design). The prune+retryable surface is narrow anyway:
an all-pruned frontier is a *success* (`map.go:316-317`), and prune decides via
`top(k)`/`stop_when`, not `min_success`.

### 6.5 Reduce maps are safe by construction (no exclusion, no new code)

A reduce map needs **no** Scope B change, and must **not** be excluded (excluding would
forfeit recovery — strictly worse). A reduce commits a `node.completed` at the map path
**only on success** (`runQuorumReduce` `reduce.go:193-197`; `runCommandReduce`
`reduce.go:302-306`); a quorum miss returns `OutcomeRetryableFailure` **uncommitted**
(`reduce.go:188-192`) and a failing command reducer goes through `failStep`
(`reduce.go:300`). The map handler returns the reduce outcome directly (`map.go:337`).
Therefore: **committed-reduce ⟺ map-node-ok ⟺ NOT the retryable path.** On the
`retryable_failure` resume path the reducer is *always* uncommitted, so the short-circuit
(`reduce.go:68-70`) never fires and the reducer **re-runs over the full post-recovery
passed set**: `statuses` is rebuilt every call (`map.go:149`), `effectiveTotal =
len(overArr) - pruned` (`map.go:320`) and the cohort (`map.go:337`) reflect the settled
set, and `runQuorumReduce` recomputes `agree` and `need = quorumThreshold(quorum, cohort)`
(`reduce.go:185-186`) over it. An item flipping fail→pass simply enlarges the agreeing set
*before* the single atomic reduce — recompute-over-full-set, never a partial blend
(Spark's "result stage cannot roll back → recompute or fail"; Step Functions
`ResultWriter` re-aggregates after redrive).

**Invariant to state (was an unanalyzed gap):** a committed reduce output is never blended
with newly-recovered items, because a committed reduce implies the map node already rolled
up `ok` and is replayed-as-skipped, never reached on the retryable path. A `run:` command
reducer that failed transiently is uncommitted and re-runs, carrying the same at-least-once
exposure as any uncommitted frontier (§9). Reduce correctness silently depends on §6.2's
dedup (`collectReduceBranches` reads `ItemPassed` records).

### 6.6 Operator visibility — `awf ls` distinguishes resumable from terminal

`obs.DeriveStatus` collapses every non-`ok` `run.finished` to `RunFailed`
(`obs/runstatus.go:46-51`), so an operator's primary inventory command cannot tell a
resumable `retryable_failure` run from a dead `permanent_failure`/`rejected` one. Every
mature system surfaces this as first-class (Step Functions shows redrive eligibility;
Azure maintainers explicitly wished for a "Suspended" state distinct from "Failed"). The
"just document it" fallback is unprecedented and weaker; **resolved: introduce a distinct
status.**

- Add `RunResumable RunStatus = "resumable"` to `obs/runstatus.go:13-24`.
- Split the `run.finished` arm (`:46-51`): `ok → RunFinished`,
  `retryable_failure → RunResumable`, else `RunFailed`. (It already unmarshals
  `RunFinishedData`, so this is reading a field it already reads — stays pure/OS-free.)
- Split the trailing `node.failed` arm (`:68-70`) symmetrically via
  `NodeFailedData.Outcome` (required for consistency with §5.1's crash-window resume
  path; without it `ls` and `resume` disagree).
- Update the precedence doc-comment (`:32-39`), `man/awf.1.md:219-221` ls vocabulary,
  and `obs/runstatus_test.go`. `cli/ls.go` and `ui/runs.go` emit `string(status)` so the
  new value flows through (text + JSON) for free; it is especially useful in the
  `ui/runs.go` resume picker.

### 6.7 Consistency framing (why this is right, not just a feature)

Plain steps never commit a failure (`node.completed` is `ok`-only; failures are a
separate `node.failed` record that halts). Maps are the lone place that durably commits
failures — a **plain** map via `map.item{ItemFailed}` (`events.go:62`), a **prune** map
via the atomic `map.frontier{... item_failed/item_pruned}` (`events.go:64-81`). Scope B
narrows this **only** for plain-map `map.item` items: a *transiently* failed item stops
being a durable commit and rejoins the uncommitted frontier (matching "only real results
commit; transient faults re-run"). A *permanent* or *rejected* item stays committed (a
settled negative that counts against `min_success`) — the analogue of a plain step's
`node.failed`. Prune dispositions stay durable and replayed verbatim (§6.4).

---

## 7. Resolved decisions

The five prior open decisions are resolved (code-grounded + prior-art backed):

1. **Item-outcome granularity** → **keep `MapItemData.Outcome`** (§6.1). Justified on
   **correctness**, not purity: re-running a rejected/permanent item is wrong (gate
   integrity / side effects), so re-run selection needs the per-item outcome. The
   "re-run all `ItemFailed`" shortcut is rejected.
2. **Uniform vs terminal-only re-run** → **resume-only** via a `Resume` flag (§6.3),
   NOT the run-rollup outcome. Matches the industry norm (after-terminal recovery is a
   distinct mode) and preserves the pinned crash-resume contract
   (`map_test.go:681-729`). Uniform crash-resume recovery is deferred.
3. **Old-log absent `Outcome`** → **non-rerunnable** (replay-as-failed). Conservative
   ("when in doubt, stop"), and unambiguous now that `image_unavailable` is peeled off
   first (§6.3).
4. **`image_unavailable` items** → **non-rerunnable** for v1 (§6.3), via a Reason-branch
   evaluated before the absent-Outcome rule. Residual image_unavailable is a
   digest-pinned determinism fault; the transient ctx-cancel case is already diverted
   upstream (`map.go:443-451`).
5. **Reduce maps** → **safe by construction; no exclusion, no new code** (§6.5). The
   reducer is always uncommitted on the retryable path and re-runs over the full
   recovered set; the invalidation invariant is now stated.

---

## 8. Invariants preserved

- **Commit = content-address-then-pointer-swap.** Unchanged. Re-running a retryable item
  goes through the same `commitMapItem` (artifact-then-event); a "done" record never
  references a missing artifact.
- **Only `ok` `node.completed` commits.** Unchanged. Scope B touches `map.item` (which
  always committed both pass and fail); it introduces no non-`ok` `node.completed`.
- **Resume folds the log; committed work is replayed, not recomputed.** Preserved for
  `ok` work. Scope B reclassifies only *transiently-failed plain-map* items as
  uncommitted frontier — the same treatment a transiently-failed plain step already
  gets; crash-resume of maps is unchanged (resume-flag-gated, §6.3).
- **Crash ≠ verdict / Retry ≠ repair.** Untouched. Pure retry (re-run identical work, no
  feedback); a rejected item is never re-run (§6.1).
- **Determinism / pinning.** Unchanged. Digest + runtime drift remain hard errors; the
  `H8` non-deterministic-`over` guard is retained (and hardened by §6.2).
- **The interpreter is the only writer to state.** Unchanged; re-commits go through the
  existing map commit path. `awf ls`'s new status (§6.6) is read-only.
- **`map.frontier` atomicity.** Preserved by excluding prune maps from per-item re-run
  (§6.4).

---

## 9. Side-effect, idempotency & operator-loop semantics (honest)

Resume re-executes a frontier step or map item that **already ran to its full retry
budget** (default 3, `retry/policy.go:66`; exhaustion at `retry_loop.go:87-89`). Stated
honestly, mirroring the industry framing (Temporal: an activity "may even partially
complete more than once"; "durable execution protects progress, it does not turn a
non-idempotent request into exactly-once"):

- A re-run has **at-least-once** side-effect semantics: the runtime guarantees the
  content-addressed commit is observed once, NOT that the step's external effects
  happened once. This is the **same** exposure crash-resume already accepts (same
  mechanism: Completed-skip + frontier re-run); it is not a new hazard *mechanism*.
- `idempotency_key` is a **hint the external system enforces**, never engine dedup:
  injected as `AWF_IDEMPOTENCY_KEY` (`local_dispatcher.go:148-149`) or the
  `Idempotency-Key` header (`awfllm/transport.go:151-152`) — **the Gemini adapter sends
  no key** (`transport.go:525`), so that path is pure at-least-once. Agent autonomous
  effects (`mcp://` calls, network exec) are at-least-once and **outside the guarantee**
  (already stated at `man/awf-workflow.5.md:1356-1358`).
- Re-run is **not atomic** — a step can redo its early side effects before completing its
  later ones, on each resume.
- A re-run agent frontier may produce a **different typed output** — correct, because the
  failed attempt never committed a `node.completed` (`interpreter.go:259-261`), so nothing
  was bound against it.
- **No resume-attempt cap / DLQ analog** — a deliberate scope cut, not the industry
  default (cf. SQS `maxReceiveCount`, Temporal `maximumAttempts`, redrive 14-day window).
  A flapping transient fault re-fails each manual resume; the operator is the only bound.
  (The `max_attempts` cap is on *repair*, a separate axis — retry ≠ repair.) Recommend
  the operator inspect the failure cause before resuming.

This lives in the spec here and in `man/awf-workflow.5.md` §EXTERNAL EFFECTS AND
IDEMPOTENCY (`:1348-1360`, extended with one resume paragraph) — not in invented spec
sections.

---

## 10. Testing plan (fake backend — no Docker)

Behavior must be pinned before its test exists (rejected → non-rerunnable §6.1; reduce
invalidation §6.5; image_unavailable → non-rerunnable §6.3) — done above.

**Scope A guard (unit, `cli/resume_test.go`):**

- `run.finished{retryable_failure}` → **proceeds** (flip the current refusal assertion);
  frontier re-runs to `ok` when the dispatcher succeeds.
- `run.finished{permanent_failure}` / `{rejected}` / `{ok}` → refused (distinct messages).
- Crash window: `node.failed{retryable_failure}` with no `run.finished` → resumes;
  `node.failed{permanent_failure}` (or empty) with no `run.finished` → refused.
- **Compound-admit regression (the bug §5.1 fixes):** `run.finished{retryable_failure}`
  with a co-existing `node.failed{permanent_failure}` at a map-item-body path → **admits**
  (would be wrongly refused by a standalone node.failed loop). Plus a multi-`node.failed`
  log (inner child + call path, both retryable) → admits.

**Scope A composite (conformance, exercising the relaxed guard):**
`testResumeTerminalRetryableComposite` — committed setup step, then a gate whose generate
body has a code step that transiently exhausts retries → `run.finished{retryable_failure}`,
exit 1. Drive resume **through `cli/resume.go`** (NOT `harness.resumeWorkflow`, which
bypasses the guard, `harness.go:145-223`). Assert: committed setup step is replayed (not
re-dispatched); the gate re-enters at the SAME attempt (`startN` unchanged, proving
crash≠verdict held); with a now-succeeding dispatcher the run → `ok`.

**Scope B (engine + conformance):**

- `TestRunMapResumePermanentItemStaysFailedRetryableReRuns`: min_success map, mixed
  retryable + permanent items; on resume (with `RunOptions.Resume=true`) only retryable
  items re-run, permanent stays failed; tally reaches `min_success` → `ok`, else re-fails
  `retryable_failure` idempotently.
- `TestRunMapResumeGateRejectedItemNotReRun`: a gate inside a map item rejects → item
  recorded `Outcome=rejected` → resume does NOT re-run it.
- `TestFoldMapItemLastWinsByN` + a live assertion: after a re-run re-commits,
  `LookupMapItems(mapPath)` has **no duplicate N** (sample mid-resume via the fake
  dispatcher hook, and at end); a cross-map `{{ step.<id> }}` aggregate over a map with
  one recovered item yields N elements, **not** N+1.
- Quorum reduce recovery: quorum=N over N items, one transiently fails first run
  (`agree<N` → retryable), passes on resume (`agree==N`) → reducer re-runs → map `ok`.
- Prune exclusion: a committed-frontier prune map that rolled up `retryable_failure`
  resumes → frontier replayed verbatim, re-fails identically, NO partial `map.frontier`
  emitted.
- `TestRunMapResumeFailedItemsStayFailed` (`map_test.go:681-729`) stays GREEN unchanged
  (direct caller, no resume flag).

The conformance suite must stay green at the phase boundary.

---

## 11. Docs to update (co-landed with the code as acceptance criteria, not a trailing pass)

- **`man/awf-workflow.5.md`** — add the §4 resumability contract to the **Resume** entry
  (after `:1316`) and a cross-reference in the **Cancellation** entry (`:1338`), so the
  terminal-outcome set reads as one state machine; cover the `node.failed` crash window;
  extend §EXTERNAL EFFECTS AND IDEMPOTENCY (`:1348-1360`) with the §9 resume paragraph.
- **`man/awf.1.md`** — `awf resume` entry (`:150-168`): it now also resumes a transiently
  failed run and re-runs the transiently-failed frontier (steps, composite nodes, plain-
  map items); add `resumable` to the `ls` status vocabulary (`:219-221`).
- **`README.md`** — the resume row of the "how AWF is different" table, if it asserts
  failed-runs-aren't-resumable.
- **Code comments** — update the stale `cli/resume.go` `node.failed` comment ("Phase 3's
  try/catch will revisit this") and the stale `events.go:131` "two-value Status tally"
  comment.

Run the `updating-the-manual` skill for the man-page edits.

---

## 12. Out of scope

- `try` / `catch` / `finally` recovery.
- Resume-attempt caps / automatic re-resume / DLQ.
- Per-item productive recovery for **prune** maps (controller re-seeding — §6.4).
- Re-runnable `image_unavailable` items (future toggle — §6.3).
- Uniform crash-resume recovery of map items (deferred — §7.2).
- Any change to permanent / rejected / cancelled / ok handling.
