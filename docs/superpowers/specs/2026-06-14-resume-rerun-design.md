# Re-running a chosen committed step (`resume --rerun <step>`)

Status: design — pending author review. Builds on the shipped `resume --force`
(`cli/resume_admission.go`, `cli/resume.go`) and the resume durability model.

## 1. Problem

`awf resume` (incl. `--force`) replays every committed step and re-runs only the
**uncommitted frontier**. That is correct checkpointing — it skips re-paying
expensive committed work after a failure — but it has a sharp limitation when
**iteratively debugging a pipeline**: if a step committed `ok` but you later fix
its logic (or its inputs), resume **replays the cached output** and your fix never
runs. The only way to apply such a fix today is a full fresh run, re-paying the
entire expensive prefix.

Motivating case (cve-feasibility shakeout): `merge_runtime_compose` committed
`ok` but emitted a runtime compose with an unresolvable placeholder digest — a
*latent* bug that only failed downstream. The fix lives in `merge-compose.py` (a
digest-invisible loose script). `resume --force` re-enters the run but replays the
cached bad compose, so the fix needs a fresh run that re-pays `prepare_lab` +
`version_universe` (~$1.5, ~25 min) — for a one-line fix.

`resume --rerun <step>` lets an operator **re-run from a chosen committed step**:
invalidate that step + everything after it, replay everything before it, and
re-execute the invalidated set against the *current* definition/scripts — without
re-paying the expensive upstream. It is a deliberate **dev-loop / recovery
affordance**, not a production-durability primitive.

### Prior art

Reset-to-a-point recovery is a first-class, operator-initiated primitive in
durable-execution systems: **Temporal "Reset"** (reset a workflow to a chosen
event and re-run forward), **AWS Step Functions "Redrive from state"**, **Argo
Workflows retry from a node**. `--rerun` is AWF's equivalent, scoped to the
single-host checkpoint model.

## 2. Goals / non-goals

**Goals**

1. `awf resume <run> <wf> --rerun <step>` re-enters the run, invalidates the named
   top-level node **and every top-level node after it** (+ their committed
   descendants), replays everything before it, and re-runs the invalidated set
   against the current definition.
2. `--rerun` implies admission — it re-enters a terminal run on its own (no
   separate `--force` needed) and re-enters an interrupted run too.
3. Invalidation is **journaled** (a durable, auditable `node.invalidated` event),
   not an in-memory mutation.
4. A soundness guard on the **replayed** set: the new workflow validates and every
   replayed committed path still exists as a node in the new graph; a loud warning
   covers semantic edits to replayed steps.

**Non-goals (v1)**

- Re-running a single step *inside* a map / gate / parallel / loop. v1 names a
  **top-level** node; to re-run a nested step, name its top-level container.
- Per-node hard pinning of the replayed set (warning only in v1 — see §6).
- Rolling back external side effects (at-least-once, same as `resume`/`--force`).
- Bypassing the soundness guard, or re-running with a workflow that no longer
  contains the replayed nodes.

## 3. Contract (normative)

> `awf resume <run> <wf> --rerun <step>`:
>
> - **Admits** the run regardless of terminal state (interrupted, or finished
>   `ok` / `permanent_failure` / `rejected` / `retryable_failure` / `cancelled`).
> - **Bypasses** the whole-workflow definition-digest pin AND runtime-version
>   drift (a debug-mode re-run runs against the *current* definition + images).
> - `<step>` MUST resolve to exactly one **top-level** node of `<wf>` (AWF refuses
>   an unknown or non-top-level id, listing valid top-level ids).
> - **Invalidates** that node + every later top-level node (graph-index order) +
>   all their committed descendants; **replays** earlier top-level nodes.
> - **Refuses** if the new `<wf>` fails validation, or if any *replayed* committed
>   path no longer maps to a node in `<wf>` (structural drift of the replayed set).

`--rerun` and bare `--force` are independent; passing both is accepted (`--force`
is redundant — `--rerun` already admits). `--rerun` with no failed/terminal run
(a fully-`ok` run) is valid: it re-runs the chosen tail of a successful run.

## 4. Invalidation model — "X + everything after it"

**Execution order = top-level graph index.** Naming top-level node X at index `i`:

- **Invalidated set** = every committed runtime path whose **top-level segment**
  is X or a node at index `> i`. (A runtime path's top-level segment is its first
  `.`-delimited segment, e.g. `merge_runtime_compose`, `version_universe`,
  `map[5]`; map it to its graph index via the loaded IR.)
- **Replayed set** = every committed path whose top-level segment is at index
  `< i`.

This is deliberately coarser than a typed-ref data-closure: it never replays a
node that ran *after* an invalidated one, so it cannot miss implicit
container-state / shared-file dependencies that the typed-ref graph
(`ir/validate_refs.go`) does not see — at the cost of re-running some nodes a
precise closure would skip. For nested concurrency (a step inside a top-level
`parallel`/`map`), the **whole top-level container** is the invalidation unit, so
there is no "after" ambiguity among concurrent siblings.

**Why top-level only in v1:** "everything after X" is well-defined and total for
top-level graph indices; inside a `map`/`gate`/`parallel`/`loop` it is not (no
total order among concurrent items/branches), and clearing partial map/gate state
mid-container is its own design. Naming the top-level container is unambiguous and
covers the motivating case.

## 5. Mechanism — journaled invalidation

The CLI owns the computation (it holds the loaded graph + folded RunState); fold
owns the durable application.

1. **Resolve** `<step>` to a top-level node + index `i` (refuse if absent /
   ambiguous / non-top-level).
2. **Compute** the invalidated path set from the folded RunState: every key in
   every **path-keyed** RunState index whose top-level segment is at index `≥ i`.
   The implementation MUST enumerate these indices exhaustively — at least
   `Completed`, `MapItems`, `GateAttempts`, `LoopIters`, `ReactRounds`,
   `Branches`, `SnapshotRefs`, `CallStarted` — missing one leaves stale committed
   state that would wrongly replay (the plan pins this with a RunState-field audit).
3. **Append** one `node.invalidated{paths:[…]}` event (then `Sync`), BEFORE the
   `run.resumed{epoch+1}` append. One atomic event = the whole disposition is
   durable or absent (crash-safe, mirrors `map.frontier`'s atomicity).
4. **Fold** gains a new arm: on `EventNodeInvalidated`, **delete** each listed path
   from *every* path-keyed RunState index (same exhaustive set as step 2). Fold is
   otherwise append-only; this is the first
   event that removes folded state, and it is sound because the event is appended
   *after* all the node.completed/map.item/etc. events it supersedes (so the
   removal is the last word for those paths). Precedent in spirit:
   `map.frontier` replaces per-item `map.item` records (`engine/fold.go`).
5. **Resume** proceeds normally: `run.resumed{epoch+1}`, then `runAndFinish`. The
   interpreter's `LookupCompleted` now misses the invalidated paths → re-dispatch.

New code: `engine.EventNodeInvalidated` + `NodeInvalidatedData{Paths []string}`
(`engine/events.go`); the fold arm (`engine/fold.go`); the top-level resolver +
invalidation-set computation (a new `cli/resume_rerun.go`); the `--rerun` flag +
wiring (`cli/resume.go`). The interpreter and dispatcher are unchanged — they
already re-run anything not in `Completed`.

## 6. Pin bypass + soundness guard

`--rerun` bypasses the definition-digest pin (`cli/resume.go` digest check) and
the runtime-version drift check — a debug re-run runs against the current
definition + images. This is sound for the **re-run set** (it executes fresh
against current state) and irrelevant for the **replayed set** (replayed nodes do
not re-execute, so their images/definitions are not consulted). The one real
hazard is a replayed node whose committed output came from an *old* definition.

Guard (what v1 enforces):

- The new `<wf>` must pass `ir.Validate` (no resuming into a broken definition).
- Every **replayed** committed path must still resolve to a node in `<wf>`
  (structural check — catches rename / remove / reorder / retype of an upstream
  node). If not, refuse: *"step `<path>` is replayed but no longer exists in
  `<wf>`; --rerun from an earlier step."*

Guard (what v1 warns, not enforces): a *semantic* edit to a replayed node's body.
The run stores only the workflow digest, not per-node bodies, so AWF cannot detect
this. `--rerun` prints, before executing:

> `awf resume --rerun: replaying N upstream step(s) under a changed definition. If
> you edited any step BEFORE <step>, re-run from there instead — their replayed
> outputs came from the old definition.`

**Future hardening (noted, not v1):** persist a `{static-path → node-digest}` map
in `run.started` so the replayed-set check becomes a hard error instead of a
warning. Out of scope here; v1 ships the warning.

## 7. Container-state soundness = today's `resume` envelope

`--rerun` adds **no new failure class** beyond normal resume. Re-run steps draw
inputs from committed upstream **typed outputs / `output_files` / `input_files` /
assets** (preserved as content-addressed blobs) and `snapshot:workspace`
(restored from the latest committed snapshot). Non-snapshotted container *scratch*
state from a replayed step is gone — but that is already true of `resume` (infra
is rebuilt from its image/compose recipe, never restored except
`snapshot:workspace`). `--rerun` simply re-runs more steps under the same rules; a
workflow that resumes soundly today re-runs soundly under `--rerun`.

## 8. Invariants preserved

- **The interpreter is the only writer to `state`.** The CLI appends
  `node.invalidated` exactly as it appends `run.resumed` (resume already writes
  these CLI-side at resume start); the interpreter still owns all node commits.
- **Append-only journal.** `node.invalidated` is appended, never a truncation; the
  superseded `node.completed` events remain in the log for audit.
- **Outcome taxonomy unchanged.** `--rerun` is a recovery *operation*, not a new
  outcome class.
- **Determinism / replay.** Replayed steps still reuse their committed artifacts;
  only the explicitly-invalidated set re-executes.
- **Pinning is still a hard error — at a chosen granularity.** v1 trades the
  whole-workflow pin for a structural replayed-set check + warning; the future
  per-node-digest hardening restores a hard drift error for the replayed set.

## 9. Testing plan (fake backend — no Docker)

**Resolver + invalidation set (unit, `cli/resume_rerun_test.go`):**
- top-level id at index `i` → set = its path + all committed paths with top-level
  index `≥ i`, incl. nested descendants (a committed `map[k].item-*` under a later
  top-level node is included).
- unknown id / nested-only id → refuse with the valid-top-level-ids list.

**Fold drop (unit, `engine/fold_test.go`):**
- `node.completed`(a,b,c) then `node.invalidated{[b,c]}` → Completed has only `a`.
- invalidating a map path drops its `MapItems` + per-item `Completed` +
  `GateAttempts`/`LoopIters` under it.

**End-to-end (cli, fake backend):**
- 3 top-level steps a→b→c, all committed `ok`; `--rerun b` with `b` reprogrammed →
  `a` replays (its exec not called again), `b`+`c` re-run; run finishes `ok`.
- `--rerun` admits a terminal `permanent_failure` run (no `--force` needed).
- structural guard: remove a replayed node from `<wf>` → refuse with the message.
- warning emitted whenever ≥1 node is replayed.
- pin bypass: a changed workflow digest does NOT refuse under `--rerun` (contrast
  with `resume` / `--force`, which do).

## 10. Docs

- `man/awf.1.md` resume section: document `--rerun <step>` — what it
  invalidates/replays, that it bypasses pinning (debug-mode) and implies
  admission, the at-least-once + replayed-set caveats, and the top-level-only v1
  scope.

## 11. Resolved decisions

1. **Pin envelope** → bypass the whole-workflow digest pin + runtime drift (debug
   mode), with a structural guard + warning on the replayed set.
2. **Invalidation model** → execution-order ("X + everything after it"), top-level
   granularity, replay everything before.
3. **Journaling** → one atomic `node.invalidated{paths}` event; fold deletes those
   paths from all RunState indices.
4. **Admission** → `--rerun` implies re-entry (terminal or interrupted); `--force`
   not required.
5. **Nested re-run / per-node digest hardening** → deferred (§2, §6).

## 12. Out of scope

- Re-running a nested step without naming its top-level container.
- Per-node-digest hard pinning (warning only in v1).
- Side-effect rollback / compensation.
- A typed-ref precise-closure invalidation mode.
