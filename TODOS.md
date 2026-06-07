# TODOS

## Runtime instance object model for the graph projection (deferred from Slice 1)

**What:** Design the runtime instance object model for `awf graph` — first-class
instance nodes (`map[0].item-3`, `gate[2].attempt-1`, `loop[0].body.iter-2`), a
`node_class` distinction (template vs runtime scope), both `parent_path` (runtime tree)
and `template_parent_path`, runtime-expanded edges (scope → its runtime children), and
the runtime→template mapping for nested expansion.

**Why:** The live-overlay UI must render nested runtime scopes (e.g. map item → loop
iter → step). Slice 1 deliberately ships only a *flat* `run_overlay` (state keyed by
runtime path) and defers this object model — proving it belongs in the UI slice, not
frozen into the contract before any renderer validates it.

**Context / where to start:**
- The hard case is nested expansion: `gate[0].attempt-1.generate.map[0].item-3.loop[0].body.iter-2.step`.
  `instance_of` cannot be derived by naive prefix-trimming.
- `engine/scope.go:591` has the existing runtime→template path conversion logic — reuse
  it, don't reinvent.
- `engine/runstate.go:215` — `RunState.MapItems` is append/log order, NOT `N` order;
  sort by `N` when expanding map instances.
- Render state for instances needs the event stream (`node.started`/`failed`/`skipped`/
  timing) plus `RunState` (committed facts) — `RunState` alone cannot show running/failed.

**Depends on / blocked by:** Slice 1 (`awf graph --json`: static graph + flat overlay)
shipping first.

**Surfaced by:** /plan-eng-review cross-model (Codex) review, 2026-06-08.

## Considered and cut: `--run=latest`

Run IDs are random 128-bit hex and `awf ls` sorts lexicographically, not by time; there
is no created-at index, so `latest` has no cheap well-defined meaning. Slice 1 takes
`--run=<id>` only. If CLI ergonomics later demand it, define `latest` as the run with the
newest `run.started` timestamp (requires scanning each run's first frame).
