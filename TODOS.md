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
