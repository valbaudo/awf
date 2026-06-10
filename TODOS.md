# TODOS

## DONE (2026-06-08): visual graph tool — projection, UI, data edges, instance model

Shipped across `awf graph` (`graph` package) and `awf ui` (`ui` package):
- **Projection + read-only UI + live SSE overlay** (Slices 1, 2a, 2b).
- **Data edges:** `step.<id>` references in templated fields become `Edge{kind:"data"}`,
  derived via the `template` parser; excluded from ELK layout, rendered dashed.
- **Runtime instance object model:** `BuildWithRun` emits `node_class:"instance"` nodes
  (map items / gate attempts / loop iterations + children) from `obs.Project`, nested via
  the runtime parent. `InstanceOf` resolves to the nearest template node (`map[0]` for an
  `item-N` scope, the template step for a leaf). `templateOf` inverts the map body→item-N
  replacement vs gate/loop attempt/iter append; `instanceEdges` projects control edges per
  instance. The SPA relays-out when the node set changes, restyles on state-only ticks.

### Out of scope here (separate engine feature)

**Nested-loop *reference resolution*** is unsupported upstream: `engine/scope.go`
rejects a `{{ }}` ref that crosses more than one `loop[…]` segment because the LoopIters
wire format for nested loops is unspecified (a deliberate engine deferral, slice 2.3
design Q3). This is an engine/format change, not a graph-tool gap — the graph layer's
`templateOf`/`instanceContext` already collapse multiple instance segments, so nested
expansions render correctly if the engine ever produces them (pinned by a
`TestTemplateOfAndContext` nested case). Pursue only via a dedicated engine design pass.

## Considered and cut: `--run=latest`

Run IDs are random 128-bit hex and `awf ls` sorts lexicographically, not by time; there
is no created-at index, so `latest` has no cheap well-defined meaning. `awf graph` / `awf
ui` take `--run=<id>` only. If CLI ergonomics later demand it, define `latest` as the run
with the newest `run.started` timestamp (requires scanning each run's first frame).

## Deferred: benchmark per-module step/map-product index caching

Subworkflow support may amplify repeated `StepPathIndex` and map-product index
construction because scopes are created during calls, gates, loops, and maps. Do not
cache in the first subworkflow implementation. After real modular fixtures exist,
benchmark large workflows and add per-module immutable index caching only if profiling
shows repeated graph walks are material.

## Deferred: remote imports, lockfile, and provenance

V1 subworkflows are local-only. Remote imports need a separate design covering immutable
pins, lockfile update semantics, offline resume, authentication, provenance, cache
layout, and review UX. Do not add URL or GitHub imports as a small patch on top of
local imports.

## Deferred: external mid-turn steering for live adapters

After observe-first live adapters ship, design external mid-turn steering/interrupt
as a separate feature. Same-session correction through normal AWF steps is enough
for V1. Any future external steer must route through the active interpreter/run
owner so the interpreter remains the only writer to state; never let a CLI command
mutate provider state and append AWF events out of band. Priority: P2. Effort:
M human / S with CC+gstack. Depends on live adapters, leases, and provider smoke
tests proving Codex/Goose/Claude behavior.

## Deferred: debug live metadata on existing surfaces

If live-adapter debugging needs more visibility, add verbose/debug metadata to the
surfaces AWF already has instead of creating a separate `awf live` command. Candidate
shape: live session key, lease id, provider turn id, stale lease status, provider
probe errors, and redacted event previews in terminal verbose output, `awf trace`,
and `awf ui` projection. Priority: P2. Effort: S human / S with CC+gstack. Depends
on stable `awf.live.*` obs attributes.

## Deferred: raw transcript export for live adapters

Raw provider transcripts can help deep debugging, but they may contain prompts,
file contents, tool output, credentials, and other sensitive local data. Keep raw
transcripts provider-local by default. If export is needed later, design explicit
opt-in flags, retention rules, redaction boundaries, and tests proving no raw
transcript content enters AWF blobs or traces accidentally. Priority: P3. Effort:
M human / S with CC+gstack. Depends on redaction helpers and live adapter transcript
parsers.
