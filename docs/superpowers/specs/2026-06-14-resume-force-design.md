# Force-resuming terminally-failed runs (`resume --force`)

Status: design — pending author review. Sibling of
`2026-06-14-resume-retryable-failure-design.md`; this spec builds on that spec's
§5.1 outcome-aware guard and must not introduce a second, competing guard rewrite.

## 1. Problem

`awf resume` refuses any run that reached a terminal `run.finished` event
(`cli/resume.go:147-151`). The retryable-failure spec relaxes that for one
outcome — `retryable_failure` — and deliberately keeps `permanent_failure`,
`rejected`, and `cancelled` terminal, because "re-running changes nothing"
**under the same binary**.

That last clause is the gap. A `permanent_failure` can be caused by an *engine
bug*, not an author error — e.g. a reducer template ref that the engine failed to
resolve because of a scope bug (the AWF4002 reduce-in-a-called-subworkflow bug
fixed in `e998424`). The classifier correctly called it `permanent_failure`
(re-running the identical step on the *same* binary is identical), so the run was
sealed. After the operator fixes the binary the run *would* now succeed, but the
durable, content-addressed checkpoint (everything committed before the failed
frontier) is unreachable — forcing a full fresh run that re-pays correct work.

`resume --force` is the deliberate operator override for exactly this case:
re-enter a terminally-failed run and re-run its uncommitted frontier, **with every
pin still enforced** (so the workflow definition and container runtimes are
provably unchanged; only the un-pinned engine binary may differ).

### Prior art

The same shape as the retryable spec's prior art (Temporal Reset, AWS Step
Functions Redrive, Argo retry): after-terminal recovery is a first-class,
operator-initiated primitive distinct from automatic retry, gated on the
definition being unchanged. `--force` differs from Redrive's automatic
eligibility only in that it admits the *explicitly-terminal* classes (permanent /
rejected / cancelled) behind an explicit operator flag rather than automatically.

## 2. Background — current behaviour (evidence)

- **Guard.** `cli/resume.go:147-171` refuses on the mere *presence* of
  `run.finished`, `run.cancelled`, or `node.failed`. The retryable spec §5.1
  replaces this with a single outcome-aware guard where `run.finished.Outcome` is
  the sole authority (crash window: `node.failed` outcome). This spec extends that
  guard's admitted set under `--force`; it does not rewrite the guard a second
  time.

- **Frontier re-run is already free.** A non-`ok` step writes only `node.failed`
  (observational — Fold ignores it; `fold.go` default arm) and never a
  `node.completed`. On resume the interpreter skips only committed nodes
  (`LookupCompleted` hit → replay), so a permanently-failed node is treated as
  uncommitted frontier and **re-executes** with no engine change. The CLI guard
  is the sole barrier.

- **Pins are independent hard errors.** Definition-digest
  (`cli/resume.go:199-226`) and resolved-runtime drift (`cli/resume.go:332-351`)
  abort resume regardless of outcome. The awf binary version is **not** recorded
  in `run.started` nor pinned — so a binary change is invisible to pinning.

- **`Cancelled` does not halt the interpreter.** `run.cancelled` sets
  `rs.Cancelled=true` in Fold (`fold.go:362-367`), but the flag is read only by
  the CLI guard and `live_resume_preflight.go:313` — the main interpreter loop
  never consults it. So admitting a cancelled run is sufficient for its frontier
  to re-run.

- **Gate budget is folded.** `engine/gate.go:93` computes
  `startN = len(LookupGateAttempts(gatePath)) + 1` and loops `n <= g.MaxAttempts`.
  A `rejected` gate already has `MaxAttempts` attempts folded in, so on re-entry
  `startN > MaxAttempts` → the loop never runs → it immediately re-rejects
  (`gate.go:192` fall-through). Each attempt's `generate`/`evaluate` sub-nodes
  commit `node.completed` at `attempt-N.…` paths, so re-running an *existing*
  attempt number would replay the old (rejected) outputs.

## 3. Relationship to the retryable-failure spec

| | retryable (`resume`) | force (`resume --force`) |
|---|---|---|
| Trigger | automatic on `retryable_failure` | explicit `--force` flag |
| Admitted outcomes | `retryable_failure` | + `permanent_failure`, `rejected`, `cancelled` |
| Rationale | transient fault — retry may help | operator asserts the deterministic cause is fixed (typically the un-pinned binary) |
| Guard | §5.1 rewrite (this spec depends on it) | extends §5.1's admitted set |
| Pins | enforced | enforced (unchanged) |

`--force` is the manual counterpart to the retryable spec's automatic admission.
The two share one guard. **Dependency:** this work lands on top of (or rebased
onto) the retryable spec's §5.1 guard; it never rewrites that guard independently.

## 4. Goals / non-goals

**Goals**

1. `awf resume <id> <path> --force` re-enters a run whose terminal rollup is
   `permanent_failure`, `rejected`, or `cancelled`, replaying all committed steps
   and re-running the uncommitted frontier.
2. `rejected` is made *productive*: a gate that exhausted its budget re-runs from
   a fresh attempt allotment (minimal gate-budget reset, §8).
3. All pins remain hard errors under `--force` (§6).
4. `ok` is never re-enterable. Without `--force`, behaviour is exactly the
   retryable spec's.

**Non-goals**

- Re-running a *permanent map item* (committed `map.item{permanent}`,
  replayed-as-failed). Deferred to a follow-up that reuses the retryable Scope B
  selection machinery (§9).
- Relaxing any pin (definition digest / runtime version). A changed definition or
  image is still refused — that is a different, larger feature, explicitly out of
  scope (§16).
- A configurable extra-attempt count, retry-budget caps, or `awf ls` status
  vocabulary changes (mirror the retryable spec later).
- Binary-version pinning. `--force` is the operator's assertion in lieu of it.

## 5. Resumability contract (normative)

Extends the retryable spec's §4:

> A run is resumable by **`awf resume`** iff its terminal rollup is
> `retryable_failure` (crash window: its only terminal marker is a
> `node.failed{retryable_failure}`). `ok`, `permanent_failure`, `rejected`, and
> `cancelled` are not resumable by bare `resume`.
>
> A run is additionally resumable by **`awf resume --force`** iff its terminal
> rollup is `permanent_failure`, `rejected`, or `cancelled` (crash window: the
> corresponding `node.failed` outcome, or a `run.cancelled` marker). `ok` is never
> resumable. **`--force` relaxes only the terminal-outcome guard; all pins remain
> hard errors.**

## 6. Design — Scope A admission (CLI + guard)

**CLI.** Add a `--force` bool flag (default false) to the resume flag set
(`cli/resume.go`, the `flag.NewFlagSet("resume", …)` block). Usage:
`awf resume <run-id> <path> [--state-dir <dir>] [--force]`.

**Guard.** In the §5.1 outcome-aware guard, widen the admitted set when `--force`:

```
admit iff outcome == retryable_failure
       OR (force AND outcome ∈ {permanent_failure, rejected, cancelled})
refuse  ok            (always — a finished-ok run has nothing to resume)
refuse  the force set when --force is absent, with a message that names --force
```

Crash-window (no `run.finished`): mirror §5.1's `node.failed`-outcome reading,
widened to the force set under `--force`; `run.cancelled` (a terminal marker with
no outcome field) is admitted only under `--force`.

**Pins unchanged.** The definition-digest and runtime-drift checks
(`cli/resume.go:199-226`, `:332-351`) run unconditionally, before execution,
exactly as today. `--force` does not gate or skip them.

**Threading.** Carry a force-resume signal on `RunOptions` (a distinct
`ForceResume bool`, or folded into a shared resume-mode with the retryable spec's
`Resume` flag — decided at §17) and thread it onto `interpreterContext`
(`interpreterContext.forceResume`) so the gate executor can read it (§8). No other
node kind reads it.

## 7. Design — frontier re-run (free)

No engine change. Once admitted, `engine.Run` folds the journal and the
interpreter re-runs every uncommitted node:

- **`permanent_failure` at a node/step/agent/reduce** — the failed node has no
  `node.completed` → re-runs. (run-29927: replay `prepare_lab` + the
  `version_universe` fan-out items, re-run the now-fixed reduce, then the
  downstream gates that never ran.)
- **`cancelled`** — the in-flight frontier when the cancel landed is uncommitted →
  re-runs. `rs.Cancelled` does not halt the loop (§2), so admission suffices.

Committed steps replay (recorded outputs reused, not recomputed); infra is rebuilt
from its image/compose recipe, never restored from a snapshot — unchanged.

## 8. Design — gate-budget reset (the one engine change)

Makes `rejected` productive. A rejected gate must re-run from a *fresh* attempt
allotment whose attempt numbers do not collide with the committed (rejected)
attempts (else the sub-nodes replay). Change only the loop ceiling in
`engine/gate.go`:

```go
startN := len(ictx.runstate.LookupGateAttempts(gatePath)) + 1
folded := startN - 1
ceiling := g.MaxAttempts
if ictx.forceResume && folded >= g.MaxAttempts {
    // Exhausted gate (it rejected): grant ONE fresh MaxAttempts allotment,
    // numbered ABOVE the committed attempts so attempt-N sub-node paths are
    // uncommitted and really re-run.
    ceiling = folded + g.MaxAttempts
}
for n := startN; n <= ceiling; n++ { /* unchanged */ }
```

Behaviour:

- Rejected gate (folded == MaxAttempts) under `--force`: runs fresh attempts
  `MaxAttempts+1 … 2·MaxAttempts`. The prior verdict auto-feeds as repair feedback
  (`evaluate.*` already resolves the latest folded attempt) — i.e. the repair
  conversation *continues*, which is desirable.
- Partially-used gate (folded < MaxAttempts): untouched — finishes its original
  budget (covers a gate interrupted mid-budget by a cancel, then force-resumed).
- Passed gate: committed `node.completed` → replayed/skipped; the executor is not
  entered.
- A second `--force` after another rejection grants another allotment (folded
  grows), which is the intended "force again = another budget."

This is the minimal change: one ceiling computation, gated on `forceResume` and
an exhausted budget. No new event, no attempt deletion, append-only preserved.

## 9. Deferred — permanent map-item re-run

A permanently-failed *map item* commits its terminal status
(`map.item{permanent}` / atomic `map.frontier`) and is replayed-as-failed on
resume — admission alone does not re-run it. Making it productive is the direct
analogue of the retryable spec's Scope B: gate per-item re-run on resume, keyed on
the recorded item outcome. The clean implementation widens that Scope B selection
predicate to include `permanent` **when `forceResume` is set**, reusing its
machinery rather than duplicating it. This is out of v1 scope and is cleanest once
Scope B has landed. v1 documents the boundary: under `--force`, a permanently-failed
map *item* is not yet re-run (the surrounding run still re-enters and its frontier
re-runs).

## 10. Journal & lifecycle (append-only)

Re-entry appends `run.resumed{epoch+1}` (the existing resume mechanism;
`fold.go:128-136`). Completion appends a fresh `run.finished{outcome}`. Fold
already ignores `run.finished` (observational); the guard reads the *latest* one.
The stale terminal markers (`run.finished`, `run.cancelled`, `node.failed`,
rejected `gate.attempt`s) remain in the log — nothing is truncated. The gate's
fresh attempts append new `gate.attempt-N` events above the old ones.

## 11. Safety & side-effects (honest)

- **At-least-once.** Re-running the frontier re-runs its side effects (agent calls,
  tool/command invocations). `--force` re-runs a node that previously *failed*, so
  its side effects may not have completed — but a partially-applied effect (e.g. a
  half-written external row) can repeat. This is the same exposure the retryable
  spec documents (§9 there); `--force` users inherit it and are warned.
- **Operator assertion.** Because the binary is un-pinned, `--force` is the
  operator stating "I fixed the deterministic cause." If they did not, the frontier
  re-fails identically and the run re-finishes terminal — no corruption, just no
  progress (idempotent w.r.t. the durable log).
- **Warning.** `--force` prints one line before executing, e.g.:
  `awf resume --force: re-entering a terminally-<outcome> run; the failed frontier
  (and its side effects) will re-run. Pins are still enforced.`
  No interactive prompt (CLI/automation).

## 12. Invariants preserved

- Only `ok` `node.completed` commits — unchanged.
- The interpreter is the only state writer — unchanged (guard relaxation +
  ceiling change write nothing new; the gate's fresh attempts commit exactly like
  first-run attempts).
- Pinning-on-drift is a hard error — unchanged and explicitly re-asserted under
  `--force`.
- Outcome taxonomy is unchanged — `--force` is a recovery *operation*, not a new
  outcome class.
- Gate independence (fresh evaluator context) and crash≠verdict — unchanged; the
  fresh allotment runs full attempts exactly like a first run.
- Resume folds the log; committed steps replay, infra rebuilt from recipe —
  unchanged.

## 13. Testing plan (fake backend — no Docker)

Pin behaviour before the change.

**Guard (unit, `cli/resume_test.go`):**
- `run.finished{permanent_failure|rejected|cancelled}` without `--force` → refused
  (message names `--force`).
- Same three with `--force` → admitted.
- `run.finished{ok}` with `--force` → still refused.
- `run.cancelled` (no `run.finished`) with/without `--force` → admitted/refused.
- Crash window: `node.failed{permanent_failure}` only, ± `--force`.

**Pins under `--force` (unit/conformance):**
- Definition-digest mismatch + `--force` → still aborts.
- Runtime-version drift + `--force` → still aborts.

**Frontier re-run (conformance):**
- Force-resume a `permanent_failure` run whose reduce-in-a-called-subworkflow
  failed (reuse the AWF4002 scenario / `e998424` fixture): committed fan-out
  replays, reduce re-runs and commits, downstream nodes execute → run finishes ok.
- Force-resume a `cancelled` run → in-flight frontier re-runs to completion.

**Gate-budget reset (engine + conformance):**
- A rejected gate (folded == MaxAttempts), force-resume → runs attempts
  `MaxAttempts+1…2·MaxAttempts`; with a now-passing evaluator → gate passes, run
  finishes ok. Assert new `gate.attempt` numbers, old ones intact.
- Partially-used gate under `--force` → ceiling stays `MaxAttempts` (no
  over-grant).
- Bare `resume` (no `--force`) never reaches a rejected run (guard refuses) — the
  ceiling branch is force-only.

## 14. Docs to update (co-landed)

- `man/awf.1.md` resume section: document `--force`, the admitted outcome classes,
  that pins remain hard errors, and the at-least-once warning.
- The resumability-contract doc / §4 cross-reference: add the `--force` clause so
  the terminal-outcome set reads as one state machine across bare and `--force`
  resume.

## 15. Resolved decisions

1. **Coordination** → sibling spec, builds on the retryable §5.1 guard (one
   guard, parameterised by outcome + `--force`).
2. **Safety envelope** → `--force` relaxes only the terminal-outcome guard; pins
   stay hard errors.
3. **Admitted outcomes** → `permanent_failure`, `rejected`, `cancelled`.
4. **`rejected` productivity** → minimal gate-budget reset now (Hybrid): a fresh
   `MaxAttempts` allotment numbered above the committed attempts. Not configurable.
5. **Permanent map-item re-run** → deferred; reuse the retryable Scope B selection
   under `forceResume` once it lands.

## 16. Out of scope

- Any pin relaxation (definition / runtime). Changed definition or image stays a
  hard error under `--force`.
- Permanent map-item per-item re-run (§9 — follow-up).
- Binary-version pinning / recording.
- Configurable attempt budgets, retry caps, `awf ls` status vocabulary.
- Any change to `ok` handling.

## 17. Open details for the implementation plan

- Confirm `live_resume_preflight.go` (copies `rs.Cancelled`,
  `live_resume_preflight.go:313`) does not block a forced cancelled re-entry; if
  it does, gate it on `forceResume`.
- Exact wording + placement of the `--force` warning line.
- Whether `RunOptions.ForceResume` is a distinct field or folded into a richer
  resume-mode shared with the retryable spec's `Resume` flag (decide when the two
  land together).
