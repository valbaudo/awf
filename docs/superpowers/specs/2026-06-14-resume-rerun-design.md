# Re-running from a chosen committed step (`resume --from <step>`)

Status: design — pending author review. Builds on the shipped `resume --force`
(`cli/resume_admission.go`, `cli/resume.go`) and the resume durability model.

> This revision supersedes the earlier `--rerun` draft after a code-grounded
> self-critique + prior-art cross-reference (Temporal Reset, SFN Redrive, Argo,
> Nextflow `-resume`, Bazel, dbt, Dagster, Airflow). Two findings reshaped it:
> (1) **no mature system refuses a re-run on definition drift** — they either
> hard-pin (SFN), version with a loud error (Temporal), content-hash and
> auto-cascade (Nextflow/Bazel), or run loose; "refuse + warn" was a worst-of-all
> invention and is dropped. (2) **top-level-only naming defeats the motivating
> case** (the target step is a parallel branch), so naming is by runtime-path.

## 1. Problem

`awf resume` (incl. `--force`) replays every committed step and re-runs only the
uncommitted frontier — correct checkpointing, but it cannot apply a fix to a step
that already committed `ok`. When iteratively debugging a pipeline, a step can
commit `ok` yet be latently wrong (its output only fails downstream), and the fix
lives in a digest-invisible input (a loose script, an external service, container
content). Resume replays the cached-bad output; the fix needs a full fresh run,
re-paying the entire expensive prefix.

Motivating case (cve-feasibility shakeout): `merge_runtime_compose` committed `ok`
with a bad runtime compose (unresolvable placeholder digest); the fix is in
`merge-compose.py`. `resume --force` replays the cached compose. A fresh run
re-pays `prepare_lab` + `version_universe` (~$1.5, ~25 min) for a one-line fix.

`resume --from <step>` lets an operator **re-run from a chosen committed step**:
invalidate that step + everything after it, replay everything before it, and
re-execute the invalidated set against the *current* definition/scripts — without
re-paying the expensive upstream. It is a deliberate **operator-controlled
dev-loop / recovery affordance**, in the same family as `--force`.

### Prior art (what established systems actually do)

| System | Re-run-from-a-point? | Definition drift on re-run | Side effects |
|---|---|---|---|
| Temporal **Reset** | yes (to a chosen event) | runs current code; **loud nondeterminism error** unless versioned/patched | re-runs post-point; idempotency is the author's job |
| SFN **Redrive** | yes (from failed state) | **hard pin** — must start new on a definition change | succeeded states not re-run; tasks must be idempotent |
| Argo **resubmit --memoized** | yes (by node name) | run-loose; **documented silent-corruption bugs** | reuse prior outputs by name |
| **Nextflow `-resume`** | content-hash cache | changed script → that task **+ all downstream** re-run; cannot reuse a stale cache | re-runs cascade |
| **Bazel** | content-addressed actions | changed input/command → re-run **+ downstream cascade** | re-runs cascade |
| dbt `state:modified` w/o `+`, Dagster `FROM_FAILURE`, Airflow `clear` | yes | **no content check** — known stale-upstream footguns | manual |

Two lessons drive this design: **(a) nobody refuses** — the safe systems
(Nextflow/Bazel) *auto-re-run* what changed rather than blocking; the conservative
ones (SFN) hard-pin; refusing on detected drift is no one's design. **(b)** the
unsafe quadrant (Dagster/Airflow/dbt-without-`+`/Argo-memoized) is exactly
"re-run from a point without re-running changed upstream" — which `--from` avoids
by re-running the *entire* tail from the chosen point, never a precise data-closure
(see §4).

## 2. Goals / non-goals

**Goals**

1. `awf resume <run> <wf> --from <step>` re-enters the run, invalidates the named
   committed node + everything that happens-after it, replays everything before,
   and re-runs the invalidated set against the current definition.
2. `<step>` is a **runtime-path prefix** naming any committed node, including a
   node inside a `call:` sub-workflow or a `parallel:`/`map:` branch.
3. `--from` is **permissive**: it admits any run state and **bypasses** the
   definition-digest + runtime-drift pins. It does **not** refuse on drift, does
   not compute per-node digests, and works on runs started before this feature.
4. Invalidation is **journaled** (an engine-appended `node.invalidated` event)
   and clears exactly the path-keyed RunState indices (§6).
5. Before executing, the operator is shown the **re-run set** (the steps that will
   re-execute) — informed, not blocked.

**Non-goals (v1)**

- A precise typed-ref data-closure invalidation (`--from` is coarse-by-design:
  everything after the point, §4).
- Content-hash auto-staleness (the Nextflow/Bazel model — "figure out what
  changed for me"); `--from` is operator-chosen. Noted as a possible future mode.
- Rolling back external side effects (at-least-once, §5).
- Selecting an "after" boundary *among concurrent siblings* (parallel branches /
  map items have no order — see §4).

## 3. Contract (normative)

> `awf resume <run> <wf> --from <step>`:
>
> - **Admits** the run unconditionally — interrupted, or finished `ok` /
>   `permanent_failure` / `rejected` / `retryable_failure` / `cancelled`. (Its own
>   admission path; it bypasses the `resumeAdmission` outcome guard.)
> - **Bypasses** the workflow-digest pin AND runtime-version drift. The re-run set
>   executes against the *current* definition + resolved images; replayed nodes
>   are not re-executed, so their definition/images are not consulted.
> - `<step>` MUST be a runtime-path prefix matching **exactly one** committed node
>   (AWF lists candidates on an ambiguous/absent prefix).
> - **Invalidates** that node's committed subtree + every committed node that
>   *happens-after* it (§4); **replays** the rest.
> - **Prints the re-run set** (paths that will re-execute) + the replayed count
>   before proceeding.
> - Does **not** refuse on definition drift. The replayed steps reuse their
>   recorded outputs; if the operator changed an *upstream* (replayed) step, the
>   result may be incoherent — that is the operator's call to redo with an earlier
>   `--from`, exactly as `--force` trusts the operator that the fix is correct.

`--from` is a deliberate, fenced exception to the CLAUDE.md invariant *"pinning is
a hard error on drift"* — the same way `--force` is a fenced exception to terminal
runs being sealed. §8 records the contract note this requires.

## 4. Invalidation model — "the node + everything after it"

`--from N` invalidates `N`'s committed subtree plus every committed node that
**happens-after** `N` in the run's execution order, and replays everything else.
"Happens-after" respects sequential vs concurrent scopes:

- **Sequential scopes** (the root graph, a `call:` sub-workflow's graph, a loop
  body) order siblings by declaration index. Within such a scope, the siblings of
  `N`'s ancestor *after* it (higher index) + their subtrees are invalidated.
- **Concurrent scopes** (`parallel:` branches, `map:` items) have **no order**
  among siblings. `N`'s concurrent siblings are **replayed** (they do not depend on
  `N`); "after" jumps to *after the whole concurrent container*.

Computed bottom-up over `N`'s path segments: for each ancestor scope from `N`
outward, add the later-ordered siblings (sequential scopes only) + their subtrees;
at a concurrent scope add nothing and ascend to the container. This is coarser
than a typed-ref data-closure — it re-runs some independent later nodes — but it
**never replays a node that ran after an invalidated one**, so it cannot miss the
implicit container-state / shared-file dependencies the typed-ref graph
(`ir/validate_refs.go`) doesn't see.

**Motivating case, concretely.** `merge_runtime_compose` commits as
`parallel[0].merge_runtime_compose` (a branch of a top-level `parallel`). Its
concurrent siblings include the expensive `version_universe`. `--from
parallel[0].merge_runtime_compose`:
- invalidates `merge_runtime_compose`'s subtree + all root nodes after `parallel[0]`
  (the runtime window — exploit → item5/6/8 → final_record, which never committed);
- **replays** `version_universe`, the other parallel branches, and everything
  before `parallel[0]` (`prepare_lab` etc.).

That re-runs the fixed compose + the runtime window while replaying the expensive
upstream — exactly the intent. (Top-level-only naming, the prior draft, would have
forced `--from parallel[0]`, re-running `version_universe` from scratch — which is
why runtime-path naming is required, not deferred.)

**Boundary cases:**
- A `--from` whose invalidation set would cut into a **committed prune-map
  frontier** is widened to invalidate the **whole prune map** — partial frontier
  invalidation silently breaks `keep: top(k)` (the frontier is one atomic global
  decision; `engine/map.go:255-270`, `engine/events.go` `EventMapFrontier`), so it
  is re-run clean, which is the map.frontier atomicity rule, not a refusal.
- Naming a node inside a concurrent scope is fine (its subtree + after-the-container
  is well-defined); only *ordering among* concurrent siblings is undefined, and we
  never need it.

## 5. Side-effect blast radius (honest)

`--from` re-runs steps that previously **succeeded**, not just a failed frontier —
a materially larger blast radius than `resume`/`--force`. In this offensive tool
the re-run tail can include exploit-firing or lab-mutating steps; re-running them
re-fires those side effects against the target and duplicates external writes.
There is no dedupe for arbitrary steps (only cleanup carries an
`idempotency_key`, per CLAUDE.md §scope-discipline). Mitigation is disclosure, not
prevention: AWF **prints the full re-run set before executing** (mirroring SFN
showing what a redrive will run) so the operator scopes `--from` as tightly as
possible. Replayed-step soundness is unchanged from today's resume: re-run steps
draw inputs from committed upstream typed outputs / `output_files` / `input_files`
/ assets (content-addressed blobs) + `snapshot:workspace` (restored);
non-snapshotted container scratch from a replayed step is gone, exactly as under
`resume`.

## 6. Mechanism — engine-appended invalidation

All node-scoped journal events are appended **inside `engine/`** today (the CLI
appends only run-lifecycle events: `cli/run.go` run.started, `cli/resume.go`
run.resumed, `cli/execute.go` run.finished). `--from` keeps that separation:

1. **CLI** parses `--from <step>`, resolves it against the folded `Completed` keys
   to one committed node path (refuse on absent/ambiguous, listing candidates), and
   passes it as `RunOptions.RerunFrom`. The CLI bypasses the digest + runtime-drift
   checks and the `resumeAdmission` outcome guard when `RerunFrom` is set.
2. **Engine** (`engine.Run`, at resume start, after fold, before the graph walk):
   computes the happens-after invalidation path set from `def.Workflow` + the
   folded `RunState` (§4), **appends one atomic `node.invalidated{paths:[…]}`
   event** (+ `Sync`), then deletes those paths from the in-memory RunState
   indices, then walks the graph. The interpreter's existing `LookupCompleted`
   guard (`engine/interpreter.go:265`) now misses the invalidated paths → it
   re-dispatches them. No commit-once guard exists in `Commit`/`RecordCompleted`
   (`engine/commit.go:47`, `engine/runstate.go:397`) — the at-most-once property
   lives entirely in that `LookupCompleted` filter, so clearing the indices is both
   necessary and sufficient; re-running re-commits cleanly (the prior
   `node.completed` is superseded by last-event-wins fold, §7).

**Clear exactly the nine path-keyed indices** (verified exhaustive against
`engine/runstate.go`): `Completed`, `Branches`, `LoopIters`, `GateAttempts`,
`ReactRounds`, `MapItems`, `CallStarted`, `SignalReceivedAt`, `SelectedSkills`.
**Do NOT touch** the maps that only *look* path-shaped: `SnapshotRefs` (keyed by
container **name**) and `Signals` (keyed by signal **name**) — a path-prefix sweep
of those would be a bug. Clearing only `Completed` (the prior draft's implication)
would leak stale gate verdicts, loop cursors, map-item statuses, call pins, and
routed skills.

New code: `engine.EventNodeInvalidated` + `NodeInvalidatedData{Paths []string}`
(`engine/events.go`); the happens-after computation + clear routine + the append
(`engine/`); the fold delete-arm (`engine/fold.go`); `RunOptions.RerunFrom` +
the `--from` flag/resolver/bypass wiring (`cli/`). The dispatcher and the node
handlers are unchanged.

## 7. Fold: the first delete-arm, specified precisely

`engine/fold.go` is today a **purely additive** left-fold — no arm deletes
(verified: every arm appends or set-overwrites to a non-empty value). The earlier
draft claimed `map.frontier` as a deletion precedent; that is **false** —
`map.frontier` *appends* `MapItemRecord`s. `node.invalidated` is genuinely the
first event that *removes* folded state. It is sound because:

- Fold is a single **strict sequence-order** pass (`fold.go:96`, no sorting/goroutines),
  so each event applies at its journal position.
- Semantics: a path's presence is decided by the **last event touching it** —
  `node.completed`/`map.item`/`gate.attempt`/… ⇒ present; `node.invalidated` ⇒
  absent. A re-committed node (a second `node.completed` after the
  `node.invalidated`) is present again by last-event-wins.
- `node.invalidated` is **necessary** specifically for invalidated nodes that do
  *not* re-commit before the run ends (a re-committed node would already
  last-wins); without it, a later normal `resume` would re-fold their stale
  `node.completed` and wrongly skip them.

**Required conformance test (durability):** re-fold determinism — drive a run,
`--from`, re-commit some / fail before others, then a from-scratch `Fold` of the
final log MUST equal the live RunState (and a subsequent plain `resume` must see
exactly the still-uncommitted set).

## 8. Pins, contract, and invariants

`--from` bypasses the digest pin (`cli/resume.go` digest check) and runtime-version
drift. This **contradicts** the CLAUDE.md invariant *"pinning is a hard error on
drift"* — a documented contract, so per the doc hierarchy it needs an explicit,
separate revision, not a silent break. The revision is narrow and honest: the
resumability contract (`man/awf.1.md` + the durability contract doc) records that
`--from` is a deliberate, operator-fenced **debug-mode exception** to pinning,
sibling to `--force`'s exception to terminal-run sealing — the operator owns
correctness of what they replay, AWF discloses the re-run set, and a misuse is a
recoverable redo (`--from` an earlier step), never durable corruption.

Preserved invariants: the **engine** remains the only writer of node-scoped state
(the CLI passes intent; the engine appends `node.invalidated`). The journal stays
**append-only** (nothing truncated; superseded `node.completed`s remain for audit).
The **outcome taxonomy** is unchanged (`--from` is a recovery operation). Replayed
steps still reuse committed content-addressed artifacts; only the invalidated set
re-executes.

## 9. Testing plan (fake backend — no Docker)

**Resolver + happens-after set (unit, `cli/resume_rerun_test.go` / engine):**
- prefix → exactly-one committed node; absent/ambiguous → refuse with candidate list.
- happens-after for: a top-level step; a step inside a `call:`
  (`c.workflow.x` → invalidate its sub-workflow tail + all root nodes after `c`);
  a **parallel branch** (`parallel[0].x` → invalidate `x`'s subtree + after-`parallel[0]`,
  REPLAY the concurrent siblings); a step inside a `loop` body.
- prune-frontier widening: `--from` cutting into a committed frontier → whole map invalidated.

**Fold delete-arm (unit, `engine/fold_test.go`):**
- `completed(a,b,c)` then `node.invalidated{[b,c]}` → `Completed` has only `a`.
- invalidating a map/gate/call path clears its `MapItems`/`GateAttempts`/`CallStarted`
  + descendant `Completed`; `SnapshotRefs`/`Signals` (name-keyed) are untouched.
- the nine-index clear is exhaustive (a test per index that would leak).

**Conformance (durability, fake backend):**
- re-fold determinism (§7) — the load-bearing test.
- end-to-end: 3 top-level steps a→b→c committed `ok`; `--from b` (b reprogrammed)
  → `a` replays (exec not re-called), `b`+`c` re-run, run finishes `ok`.
- `--from` admits a terminal `permanent_failure` run with no `--force`.
- pin bypass: a changed workflow digest does NOT refuse under `--from` (contrast
  `resume`/`--force`, which do).
- re-run-set disclosure is printed.

## 10. Docs

- `man/awf.1.md` resume section: `--from <step>` — runtime-path naming, what it
  invalidates/replays (incl. the parallel-branch semantics), that it bypasses
  pinning (debug-mode, the §8 contract note) and admits unconditionally, the
  at-least-once + replayed-set caveats, and the re-run-set disclosure.

## 11. Resolved decisions

1. **No refusal, no per-node pin** → `--from` bypasses the digest + runtime pins;
   the operator owns replay correctness (like `--force`); AWF discloses, doesn't
   block. Works on pre-existing runs. (The "refuse on drift" and "per-node digest"
   ideas were dropped — no mature system refuses; per-node hashing is the
   *auto-staleness* model, a different, deferred feature.)
2. **Naming** → runtime-path prefix to any committed node (incl. call interiors and
   parallel/map branches), not top-level-only.
3. **Invalidation** → happens-after (node subtree + after its enclosing sequential
   boundary; concurrent siblings replayed); prune frontiers widened to whole-map.
4. **Journaling** → engine appends one atomic `node.invalidated{paths}`; fold gains
   a strict-seq-order last-event-wins delete-arm; clears the nine path-keyed indices.
5. **Flag** → `--from` (matches Temporal "reset-to" / SFN "redrive-from"); admits
   unconditionally.
6. **Contract** → fenced debug-mode exception to line-50 pinning, recorded in the
   resumability contract.

## 12. Out of scope

- Content-hash auto-staleness (Nextflow/Bazel "re-run what changed" mode).
- Typed-ref precise-closure invalidation.
- Side-effect rollback / compensation.
- Ordering among concurrent siblings (parallel branches / map items).
