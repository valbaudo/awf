# Eliminating shadow contracts between steps — declared fan-in for `map → gate → reduce`

- **Date:** 2026-06-22
- **Status:** Draft — awaiting user review (brainstorming → spec gate)
- **Author:** design session (Valerio + Claude)
- **Decision:** mechanism **P1 (forwarding)** chosen over mechanism (a) (gate output fields). See §3, §5, §7.
- **Related:** [[faithful-delivery-design]] (the "prestige" customer incident lineage), `man/awf-workflow.5.md` (gate §870-934; file-forwarding exception §744-747), `engine/reduce.go`, `engine/artifact_scope.go`, `engine/gate.go`

---

## 1. Problem

A "shadow contract" is a cross-step data hand-off that escapes AWF's declared, typed channel: two
black-box steps rendezvous on a filesystem path-*name* (e.g. `/workspace/validated/{id}.json`) that
the runtime never modeled. AWF cannot see it, validate it, or resume across it; the gate evaluates
the wrong interface; and when it breaks, it breaks **silently**.

### 1.1 Verified diagnosis of the customer incident

Read directly from `Customer's issue/validate.awf.yaml` and the engine. The earlier informal
diagnosis ("data was invisible to the *reducer*") was **wrong** — there is no reducer.

- The map body (lines 162–260) is a single `gate:`. The generator writes
  `/workspace/validated/{{candidate.id}}.json` via **prompt free-text** (line 206); the evaluator
  reads it via **prompt free-text** (line 236). Neither declares any `output_files`. The hand-off is
  **100 % shadow**.
- Within one map item, generator and evaluator share the per-item container `workspace-item-N`, so
  the *gate's internal* hand-off worked. (On native with `AWF_STAGING_ROOT=/workspace` the absolute
  write also collided with sandbox write-confinement, which is why some items burned all attempts.)
- The loss is at the post-map **`merge`** step (lines 264–281, `container: workspace` = the **base**
  container), which globs `/workspace/validated/*.json` — a directory the **isolated** per-item
  writes never reached — and swallows misses with `except: pass` (line 273). Silent loss.
- `merge` declares its *own* `output_files` feeding workflow `outputs`, so it is **not** a "dangling
  consumer" in the IR; static graph analysis would not flag it.

### 1.2 Root cause (two layers)

1. **Disease (general).** AWF cannot see an undeclared filesystem rendezvous. When two steps meet on
   a path-name across a **container-identity boundary the runtime silently created** (`map` forks
   `container: X` into per-item `X-item-N`, while a post-map step in `X` is the *base* container —
   `engine/map.go` per-item naming), data is lost with no signal. Nothing declared the edge, so
   nothing can assert it is missing.
2. **Acute (and it is the format's fault).** The natural pattern — *map → per-item gate → fan-in the
   verdicts* — **has no working declared channel**, so the author was pushed onto the shadow glob:
   - `reduce:` exists but **cannot consume gate results today**. `collectReduceBranches`
     (`engine/reduce.go:340–356`) registers producers via `ir.WalkNodes`, which *does* recurse into a
     gate's `generate`/`evaluate` (`ir/walk.go:63–66`); but it then looks each producer up at the
     **static** path `…item-N.gate[0].generate.<id>` (`ItemStepPath(..., suffix)`), while gate-internal
     steps commit at **attempt-suffixed** paths. The lookup misses → the branch "has no committed body
     output" → it is compacted out. **Net: a map body whose only producers live inside a gate
     contributes nothing to `reduce:`.**
   - The existing gate file-forwarding exception (`man:744–747`; runtime `engine/artifact_scope.go`
     `passedGateArtifactRuntimePath`) lets a **sequential** consumer's `input_files` resolve to a
     gate's accepted-attempt artifact — but it keys on the **static** gate path and is **not**
     item-aware, so it does not cover a gate *inside a map* fanned in by `reduce:`.
   - There are **zero** `map + gate + reduce` examples or conformance tests.

The author hand-rolled a fan-in because the sanctioned one did not reach their (very normal) shape.

---

## 2. Principle (state of the art)

A prior-art sweep (hermetic builds Bazel/Nix; object-capability security; typed channels; dataflow +
schema registries; workflow orchestrators; contract-first IDLs; provenance/IFC) yields:

> **The declared channel must be the only channel with a downstream future.** Enforce it by
> constraining the *medium* (content-addressed handle / static graph), **never by reading the black
> box's prompt.** Convert *silent-wrong* into *loud-missing*.

A second sweep specifically on **how retry/validation/refinement loops expose their output** found
**6/6 systems agree on transparent forwarding**: Temporal, Dagster, Argo Workflows, Airflow,
LangGraph/Reflexion, and GitHub Actions/CircleCI all forward the *accepted attempt's own declared
output*; the loop construct carries **only policy** (attempt count, stop condition), never a separate
"loop output", and the evaluator/critic verdict stays **internal**. AWF already embodies this: a
gate is policy over its `generate`, the verdict is gate-scoped, and the file-forwarding exception
forwards the accepted attempt's artifact. **This design simply extends that existing forwarding from
the sequential case to the `reduce:` fan-in case.**

This also resolves an internal-consistency tension: "the agent didn't produce the declared artifact"
is a **mechanical** contract violation, which AWF's own invariant says belongs to the mechanical lane
("**Retry ≠ repair; outcomes are mechanical, quality is the gate's job**"), not to the gate's quality
loop.

---

## 3. Decision: P1 (forwarding) over (a) (gate output fields)

Two mechanisms were designed and compared. **P1 chosen.**

- **P1 — forwarding.** Declare `output_files` (and optionally `output_schema`) on the **generate
  step**; extend `reduce:`/aggregate to resolve a gate-nested producer to the **accepted attempt's**
  already-committed artifact (reusing `engine/artifact_scope.go`'s `AttemptPassed` scan, made
  item-aware). **No new IR field, no gate commit, no new fold arm.**
- **(a) — gate output fields (rejected).** Add `Gate.OutputFiles`, capture at acceptance, commit a
  gate `node.completed`. More surface, and it would *entrench* the Retry≠repair violation by letting
  the gate capture an artifact the quality loop is still arbitrating.

**Why P1:** simplest (reduce-side only; reuses the resolver AWF already ships), prior-art-unanimous,
already-endorsed by AWF's sequential forwarding exception, and it puts existence-checking in the
mechanical lane where the invariants say it belongs.

**The one honest cost (see §6):** P1 changes the customer's *missing-evidence* handling from
feedback-driven quality repair to feedback-less mechanical retry. The principled remedy is an
**orthogonal** follow-up (surface capture-failure reason into mechanical retry), not gate-level
capture.

---

## 4. Goals / Non-goals

**Goals**
- A map body that ends in a `gate:` can fan its accepted attempt's result into `reduce:` as a
  **declared, content-addressed branch artifact**, by declaring `output_files` on the generate step.
- The customer's exact silent loss becomes a **loud, located failure** (it falls out of existing
  `output_files` capture semantics — §5.4).
- A correct, documented, conformance-tested `map → gate → reduce` pattern exists.

**Non-goals (out of scope — see §7)**
- Adding output fields to the gate node (mechanism (a)).
- Write-side OS capability confinement as a new anti-shadow mechanism (Direction A).
- Provenance digests / channel-shape in the resume guard (Direction D).
- Detecting shadow contracts by reading prompt/`run:` free-text. Any graph-shape lint (§6) stays
  structural, advisory, optional, deferred.
- In-band channels (data inside a legitimately-declared output, a RW cred dir, a compose-exposed
  network/DB). Accepted, documented residual risk.

---

## 5. Mechanism P1 — the design

### 5.1 Author-facing shape
The author declares the per-item result as a named artifact on the **generate step**, and replaces
the hand-rolled glob with a `reduce:`:

```yaml
- map:
    over: "{{ step.load_ai_findings.findings }}"
    as: candidate
    container: workspace
    body:
      - gate:
          generate:
            - id: validate_attempt
              container: workspace
              uses: factory/droid
              with: { prompt: "… write the result to /workspace/validated/{{ candidate.id }}.json …" }
              output_files:
                validated: /workspace/validated/{{ candidate.id }}.json   # NEW: declared, captured, loud-on-missing
          evaluate: [ … judges CONTENT quality … ]
          until: "{{ evaluate.passed }}"
          max_attempts: 3
    reduce:                                   # NEW: sanctioned fan-in (replaces the post-map glob merge)
      run: |
        # every branch's `validated` artifact is staged at $AWF_STAGING_ROOT/branch-<N>/validated
        …
      output_files: { … }
```

The evaluator still reads the file within the same per-attempt container (the blessed
single-container, transient hand-off) — no change there.

### 5.2 Runtime change (the actual gap)
Extend the two collectors that walk a map body so a **gate-nested producer** resolves to the
**accepted attempt's** committed node, *per item*:
- `engine/reduce.go` `collectReduceBranches` — used for `reduce:` staging
  (`$AWF_STAGING_ROOT/branch-<N>/<name>` + `aggregate.json`).
- `engine/scope.go` `aggregateMapOutputs` — used for typed-output aggregation / map→workflow-output.

For a producer whose static suffix is gate-nested (detect via `gateScopePrefix`), instead of looking
up `ItemStepPath(mapPath, N, suffix)` (which misses), compute the **item-qualified gate path**
`ItemPath(mapPath, N) + ".gate[i]"`, scan `LookupGateAttempts(thatPath)` for the last `AttemptPassed`
(K), splice `.attempt-K` into the lookup path, then read the committed `NodeResult.Files`/`Outputs`.
This is the same algorithm as `engine/artifact_scope.go:passedGateArtifactRuntimePath`, generalized
to the per-item gate path; **factor a shared helper** so the sequential and reduce cases stay in
sync. Items with no `AttemptPassed` (rejected/failed) compact out as today.

### 5.3 Validation
- The reduce-producer / `AWF3007` checks must accept a gate-nested generate producer in a
  `map + reduce` shape (the producer must declare a *named* `output_files`). Confirm/extend
  `ir/validate_output_files.go` and the `isGateScope` exception in `ir/validate_input_files.go` to
  cover the gate-inside-map → reduce path (today the exception is sequential-only).
- No new template power, no new reserved keys.

### 5.4 Loud-missing (the silent → loud win, already built)
A declared `output_files` whose file is absent at capture is **already** a loud failure:
`Backend.CaptureFiles` errors → `OutcomeRetryableFailure` (code step `engine/local_dispatcher.go:226–247`;
agent step `engine/local_dispatcher_agent.go:266–275`; tests `TestLocalDispatcherMissingOutputFileIsRetryable`,
`agent_forgot`). So once `validate_attempt` declares `output_files: { validated: … }`, an attempt
that doesn't write the file fails mechanically (retry → permanent), surfacing as a **visible failed
map item** — never an empty merge. No new assertion needed.

### 5.5 Resume / fold
**No fold change.** The accepted generate attempt's `node.completed` is already committed and folded
like any step; the `gate.attempt` events are already folded into `GateAttempts`. `reduce:` resolves
the accepted attempt at read time from data already in `RunState`. Committed steps replay; the
uncommitted frontier re-executes; crash ≠ verdict — all unchanged.

---

## 6. What the decision settles (D1/D2/D4) and the honest cost

- **D1 (capture container) — dissolved.** Capture is the generate step's existing job; a
  multi-producing-step generate uses reduce's existing shallow-merge. No special gate capture.
- **D2 (gate `output_schema` meaning) — dissolved.** There is no gate schema. Typed fan-in, if
  wanted, is the generate step's own `output_schema`, forwarded into `aggregate.json` by §5.2.
- **D4 (structural lint) — deferred.** The primary fix removes the need; any such lint is heuristic.
  Optionally revisit a structural (not free-text) warning: a `map` over container X followed by a
  same-container post-map step with no `reduce:` and no declared edge.
- **D5 (container-identity transparency) — keep, docs-only.** Document that `container: X` inside a
  `map` body is per-item-isolated and the base `X` is a different filesystem; the only sanctioned
  route out of a map item is a declared output consumed by `reduce:`.

**Honest cost of P1.** It changes missing-evidence handling from feedback-driven quality repair
(customer's `validate.awf.yaml:241–243`) to feedback-less mechanical retry — weaker for a forgetful
agent. The customer's evaluator should be simplified to judge *content* only; existence is now
mechanical. **Orthogonal follow-up (not in this spec):** surface the capture-failure reason into
mechanical retry so a retried agent is told "you exited 0 but did not write declared output X" —
that recovers the nudge without gate-level capture, and benefits every step, not just gates.

---

## 7. Explicitly out of scope (with rationale)
- **Mechanism (a) — gate output fields.** More surface; entrenches the Retry≠repair violation.
  Reconsider only if a future need requires capturing an artifact the quality loop is still
  arbitrating.
- **Direction A — write-side capability confinement.** Native already confines each step to its
  per-item workdir; the incident is read-side / missing-edge / container-identity, not unconfined
  writes. Also collides with the blessed single-container hand-off.
- **Direction D — provenance digests + channel-shape in the resume guard.** The incident data never
  became a blob; nothing to attest. P1 already makes the break loud.
- **Docker write-observation tier / prompt free-text scanning.** "Don't reinvent docker"; never read
  the prompt. The fix is backend-uniform because it is declaration + read-time resolution.

---

## 8. Conformance / definition of done
- New conformance workflow (`conformance/`) exercising `map → gate(generate with named output_files)
  → reduce`, runnable against the **fake backend**: each branch's accepted-attempt artifact is staged
  to the reducer at `$AWF_STAGING_ROOT/branch-<N>/<name>`, and `aggregate.json` carries any forwarded
  typed outputs.
- **Loud-missing** test: a gate whose accepted attempt does not write its declared `output_files`
  fails that map item (retryable → permanent), located by the producer path — never a silent empty
  merge.
- **Rejected/failed branch** test: an item with no `AttemptPassed` compacts out of the fan-in with a
  visible status, not silently.
- **Resume** test: a committed `map → gate → reduce` replays without re-running gates; resolution of
  the accepted attempt is stable across resume (no fold change required).
- `make lint test` green; conformance green at the phase boundary.
- The customer's `validate.awf.yaml`, rewritten to declare generate `output_files` + a `reduce:`,
  produces a complete (non-lossy) fan-in.

---

## 9. Prior-art appendix (selected)
- **Retry/validation-loop output exposure (6/6 forward transparently):** Temporal (activity result
  after retries), Dagster (op/asset RetryPolicy + Out), Argo (retryStrategy + outputs/artifacts),
  Airflow (retries + XCom), LangGraph/Reflexion ("the final result is returned from the generator
  node"; critique fed back as an internal message), GitHub Actions/CircleCI (step outputs after a
  successful retry). None adds a separate wrapper output; the verdict stays internal.
- **Bazel / REAPI:** only declared outputs are harvested; undeclared access fails loudly.
- **Snakemake / Nextflow / Dagster:** missing-output hard error; declared edge is the only edge.
- **Capsicum / Landlock / WASI:** authority is an unforgeable handle, not a name.
