# TODOS

## DONE (2026-06-08): runtime instance object model + data edges

Both shipped in the `graph` package and `awf ui`:
- **Data edges:** `graph.producerRefs` derives `step.<id>` references from templated
  fields (reusing `template.Slots`/`ParseRef`/`References`/`ParseArtifactRef`) and emits
  `Edge{kind:"data"}` producer → consumer. Excluded from ELK layout; rendered dashed.
- **Instance object model:** `graph.BuildWithRun` emits `node_class:"instance"` nodes for
  every runtime path with no template node (map items / gate attempts / loop iterations
  and their children), from `obs.Project` spans, nested via the runtime parent, with
  `instance_of` set to the template node. `templateOf` handles the map body→item-N
  replacement vs the gate/loop attempt/iter append; `instanceEdges` projects each
  template control edge into every instance context. The SPA relays-out when the node set
  changes and restyles on state-only ticks.

Not yet done (smaller follow-ups, uncaptured before): nested-loop instances remain
unsupported upstream (`engine/scope.go` rejects them), so deeply nested loop expansions
won't appear; `instance_of` is omitted for scope nodes whose template is a branch path
(only leaf instances carry it).

## Considered and cut: `--run=latest`

Run IDs are random 128-bit hex and `awf ls` sorts lexicographically, not by time; there
is no created-at index, so `latest` has no cheap well-defined meaning. `awf graph` / `awf
ui` take `--run=<id>` only. If CLI ergonomics later demand it, define `latest` as the run
with the newest `run.started` timestamp (requires scanning each run's first frame).
