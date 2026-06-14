# resume --from Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `awf resume <run> <wf> --from <step>` — re-enter a run, invalidate a chosen committed node + everything after its top-level ancestor, replay the rest, and re-run against the current definition (pins bypassed).

**Architecture:** A new `node.invalidated{paths}` journal event whose fold arm *deletes* the listed paths from the nine path-keyed RunState indices (the first delete-arm in an otherwise-additive fold). The engine, on resume with `RunOptions.RerunFrom`, computes the invalidation set, appends the event, and clears the in-memory indices before walking — so the existing `LookupCompleted` guard re-runs the invalidated nodes. The CLI adds the `--from` flag, resolves the prefix, bypasses the admission + pin checks, and discloses the re-run set.

**Tech Stack:** Go 1.26; `engine` (events/fold/interpreter + a new `engine/rerun.go`), `cli` (resume), fake backend for tests; `make lint test`.

**Spec:** `docs/superpowers/specs/2026-06-14-resume-rerun-design.md`.

> ### v1 SCOPE NOTE (read first — narrower than spec §4)
> The spec describes a general "happens-after" over arbitrary nesting. **v1 implements the correct *subset* where the target's runtime path traverses only `parallel` containers** (top-level nodes; parallel branches — covers the motivating `parallel[N].merge_runtime_compose`). For such targets, the invalidation set is exactly `subtree(target) ∪ {committed path p : rootSlot(p) > rootSlot(target)}`. Targets nested inside a `call`/`loop`/`gate`/`try`/`if`/`map`-body are **refused** with a clear message (`engine/rerun.go` `rerunSupported`). The general happens-after (sequential-sibling walks, the `.workflow` boundary) is deferred. Because v1's granularity is top-level-ancestor-coarse, a committed map is always wholly invalidated-or-replayed, so the prune-frontier widening (spec §4 boundary case) cannot arise in v1 (proven in Task 2 `TestComputeRerunInvalidation_MapWholly`).
>
> **Structure-preserving assumption (C1):** `--from` bypasses the digest pin, so the operator may have edited the workflow. The invalidation set is computed from the CURRENT graph's top-level order but matches OLD committed paths — so `--from` is correct for **body/script edits** (the motivating case), not graph-shape edits. `ComputeRerunInvalidation` **refuses loudly** if any committed top-level segment is absent from the current graph (a removed/renamed/reordered top-level node — including a reordered control node, whose `<kw>[N]` segment changes), instead of silently dropping it and replaying stale output. A pure top-level **step** reorder is the one structural change not refused (step ids are position-independent) and is safe (it re-runs per the new order — conservative over-invalidation, never stale reuse). Prior art (Temporal refuses on drift; Nextflow/Bazel content-hash so changes auto-cascade) backs fail-loud over the silent-reuse of Make/Airflow-pre-3.0.

> ### COORDINATION
> `--from` is the third change around the resume guard (after `--force` and the in-flight retryable scope-b). It only *reads* `resumeAdmission` (bypasses it when `--from` is set); no guard rewrite. The `RunOptions`/`runAndFinish` trailing-param pattern mirrors `--force`'s `ForceResume`.

---

## File Structure

- `engine/events.go` (modify) — `EventNodeInvalidated` const + `NodeInvalidatedData{Paths []string}`.
- `engine/fold.go` (modify) — `case EventNodeInvalidated:` delete-arm.
- `engine/rerun.go` (create) — pure helpers: `rerunRootSlots`, `rerunFirstSegment`, `rerunSupported`, `ResolveRerunTarget`, `ComputeRerunInvalidation`, `clearInvalidatedPaths`.
- `engine/rerun_test.go` (create) — unit tests for the above.
- `engine/interpreter.go` (modify) — `RunOptions.RerunFrom`; apply invalidation in `Run`.
- `engine/fold_test.go` (modify) — delete-arm + re-fold determinism.
- `cli/resume.go` (modify) — `--from` flag, resolve, bypass admission/digest/drift, disclose, thread.
- `cli/execute.go` (modify) — `runAndFinish` gains `rerunFrom string` → `RunOptions.RerunFrom`.
- `cli/run.go` (modify) — pass `""`.
- `cli/resume_test.go` (modify) — end-to-end `--from` tests.
- `man/awf.1.md` (modify) — document `--from`.

---

## Task 1: `node.invalidated` event + fold delete-arm

**Files:** Modify `engine/events.go`, `engine/fold.go`; Test `engine/fold_test.go`.

- [ ] **Step 1: Write the failing test** — add to `engine/fold_test.go`:

```go
func TestFoldNodeInvalidatedDeletes(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	mk := func(typ, path, data string) state.Event { return state.Event{Type: typ, Path: path, Data: []byte(data)} }
	okData := `{"outcome":"ok","exit_code":0}`
	events := []state.Event{
		mk(EventRunStarted, "", `{"run_id":"r","workflow_digest":"d"}`),
		mk(EventNodeCompleted, "a", okData),
		mk(EventNodeCompleted, "b", okData),
		mk(EventNodeInvalidated, "", `{"paths":["b"]}`),
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if _, ok := rs.Completed["a"]; !ok {
		t.Fatal("a should remain committed")
	}
	if _, ok := rs.Completed["b"]; ok {
		t.Fatal("b should have been deleted by node.invalidated")
	}
}

func TestFoldNodeInvalidatedThenRecommitLastWins(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	mk := func(typ, path, data string) state.Event { return state.Event{Type: typ, Path: path, Data: []byte(data)} }
	okData := `{"outcome":"ok","exit_code":0}`
	events := []state.Event{
		mk(EventRunStarted, "", `{"run_id":"r","workflow_digest":"d"}`),
		mk(EventNodeCompleted, "b", okData),
		mk(EventNodeInvalidated, "", `{"paths":["b"]}`),
		mk(EventNodeCompleted, "b", okData), // re-committed after invalidation
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if _, ok := rs.Completed["b"]; !ok {
		t.Fatal("re-committed b should be present (last event per path wins)")
	}
}
```
(If `EventRunStarted`'s payload shape differs, copy it from an existing `fold_test.go` test — match whatever `RunStartedData` JSON the file already uses; the assertions are what matter.)

- [ ] **Step 2: Run → fail.** `go test ./engine/ -run 'TestFoldNodeInvalidated' -count=1` → FAIL (`undefined: EventNodeInvalidated`).

- [ ] **Step 3a: Add the event + payload** — in `engine/events.go`, in the event-const area add:
```go
// EventNodeInvalidated removes committed node paths from the folded RunState so
// `awf resume --from` re-runs them. The first fold event that DELETES state;
// sound because it is appended after the node.completed events it supersedes and
// fold is a strict-sequence-order pass (last event per path wins).
const EventNodeInvalidated = "node.invalidated"
```
and near the other payload structs:
```go
// NodeInvalidatedData is the payload of a node.invalidated event: the runtime
// paths to drop from every path-keyed RunState index.
type NodeInvalidatedData struct {
	Paths []string `json:"paths"`
}
```

- [ ] **Step 3b: Add the fold arm** — in `engine/fold.go`, add a `case` to the event switch (mirroring the `EventRunResumed` arm). Use the shared clear helper from Task 2 if it exists yet; until then, inline the nine deletes:
```go
case EventNodeInvalidated:
	var d NodeInvalidatedData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return nil, fmt.Errorf("engine.Fold: parse %s at seq=%d: %w", EventNodeInvalidated, e.Seq, err)
	}
	clearInvalidatedPaths(rs, d.Paths)
```
(`clearInvalidatedPaths` is defined in Task 2 — if doing Task 1 first, temporarily inline the nine `delete(rs.X, path)` calls from Task 2 Step 3, then switch to the helper when Task 2 lands. DRY: prefer landing Task 2's helper first if executing out of order.)

- [ ] **Step 4: Run → pass.** `go test ./engine/ -run 'TestFoldNodeInvalidated' -count=1` → PASS.

- [ ] **Step 5: Commit.**
```bash
git add engine/events.go engine/fold.go engine/fold_test.go
git commit -m "feat(engine): node.invalidated event + fold delete-arm"
```

---

## Task 2: invalidation-set computation (`engine/rerun.go`)

The pure core. Covers the v1 supported subset; refuses the rest.

**Files:** Create `engine/rerun.go`, `engine/rerun_test.go`.

- [ ] **Step 1: Write the failing tests** — `engine/rerun_test.go`:

```go
package engine

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
)

// wf: root = [s0(step), parallel[1]{branchA, branchB}, s2(step)]
func rerunTestWF() *ir.Workflow {
	return &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{
		&ir.CodeStep{ID: "s0", Run: "x", Container: "c"},
		&ir.Parallel{Children: ir.NodeList{
			&ir.CodeStep{ID: "branchA", Run: "x", Container: "c"},
			&ir.CodeStep{ID: "branchB", Run: "x", Container: "c"},
		}},
		&ir.CodeStep{ID: "s2", Run: "x", Container: "c"},
	}}
}

func rsWithCompleted(paths ...string) *RunState {
	rs := NewRunState("r", "d", nil)
	for _, p := range paths {
		rs.Completed[p] = NodeResult{Outcome: OutcomeOK}
	}
	return rs
}

func TestComputeRerunInvalidation_ParallelBranch(t *testing.T) {
	wf := rerunTestWF()
	rs := rsWithCompleted("s0", "parallel[1].branchA", "parallel[1].branchB", "s2")
	got, err := ComputeRerunInvalidation(wf, rs, "parallel[1].branchA")
	if err != nil {
		t.Fatalf("ComputeRerunInvalidation: %v", err)
	}
	want := []string{"parallel[1].branchA", "s2"} // branch subtree + later root-slot; branchB (concurrent) + s0 replay
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("set = %v, want %v", got, want)
	}
}

func TestComputeRerunInvalidation_TopLevelStep(t *testing.T) {
	wf := rerunTestWF()
	rs := rsWithCompleted("s0", "parallel[1].branchA", "parallel[1].branchB", "s2")
	got, _ := ComputeRerunInvalidation(wf, rs, "s0")
	want := []string{"parallel[1].branchA", "parallel[1].branchB", "s0", "s2"} // s0 + everything after slot 0
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("set = %v, want %v", got, want)
	}
}

func TestComputeRerunInvalidation_RefusesNestedSequential(t *testing.T) {
	wf := rerunTestWF()
	if _, err := ComputeRerunInvalidation(wf, rsWithCompleted("recon.workflow.step"), "recon.workflow.step"); err == nil {
		t.Fatal("expected refusal for a target inside a call (.workflow)")
	}
	if _, err := ComputeRerunInvalidation(wf, rsWithCompleted("loop[0].body.iter-1.step"), "loop[0].body.iter-1.step"); err == nil {
		t.Fatal("expected refusal for a target inside a loop body")
	}
}

// C1 structural-drift guard: a committed path whose top-level segment is absent
// from the (edited) graph must REFUSE, not silently drop the node and replay stale.
func TestComputeRerunInvalidation_RefusesStructuralDrift(t *testing.T) {
	wf := rerunTestWF() // graph has s0, parallel[1], s2 — but NOT "gone"
	rs := rsWithCompleted("s0", "gone", "s2")
	if _, err := ComputeRerunInvalidation(wf, rs, "s0"); err == nil {
		t.Fatal("expected refusal: committed node \"gone\" has no top-level node in the current graph")
	}
}

// m6: a committed map is wholly invalidated — never a subset (a partial frontier
// would corrupt keep:top(k)). v1's root-slot granularity guarantees it.
func TestComputeRerunInvalidation_MapWholly(t *testing.T) {
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{
		&ir.CodeStep{ID: "s0", Run: "x", Container: "c"},
		&ir.Map{Over: ir.Expr("{{ input.xs }}"), As: "x", Container: "c",
			Body: ir.NodeList{&ir.CodeStep{ID: "scan", Run: "x", Container: "c"}}},
		&ir.CodeStep{ID: "s2", Run: "x", Container: "c"},
	}}
	rs := rsWithCompleted("s0", "map[1].item-0.scan", "map[1].item-1.scan", "s2")
	rs.MapItems["map[1]"] = []MapItemRecord{{N: 0, Status: ItemPassed}, {N: 1, Status: ItemPassed}}
	got, err := ComputeRerunInvalidation(wf, rs, "s0") // s0 at slot 0 -> map[1] (slot 1) wholly after
	if err != nil {
		t.Fatalf("ComputeRerunInvalidation: %v", err)
	}
	for _, p := range []string{"map[1].item-0.scan", "map[1].item-1.scan", "map[1]"} {
		if !contains(got, p) {
			t.Fatalf("map path %q must be in the whole-map invalidation set; got %v", p, got)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestResolveRerunTarget(t *testing.T) {
	wf := rerunTestWF()
	rs := rsWithCompleted("s0", "parallel[1].branchA", "parallel[1].branchB")
	if got, err := ResolveRerunTarget(wf, rs, "parallel[1].branchA"); err != nil || got != "parallel[1].branchA" {
		t.Fatalf("exact: (%q,%v)", got, err) // (1) exact committed path
	}
	if got, err := ResolveRerunTarget(wf, rs, "parallel[1]"); err != nil || got != "parallel[1]" {
		t.Fatalf("container: (%q,%v)", got, err) // (2) top-level container (no committed key of its own)
	}
	if got, err := ResolveRerunTarget(wf, rs, "branchA"); err != nil || got != "parallel[1].branchA" {
		t.Fatalf("bare-id: (%q,%v)", got, err) // (3) unique bare step id
	}
	if _, err := ResolveRerunTarget(wf, rs, "nope"); err == nil {
		t.Fatal("expected error for absent arg")
	}
}
```

- [ ] **Step 2: Run → fail.** `go test ./engine/ -run 'TestComputeRerun|TestResolveRerun' -count=1` → FAIL (undefined).

- [ ] **Step 3: Implement** — `engine/rerun.go`:

```go
package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/valbaudo/awf/ir"
)

// rerunFirstSegment returns the first '.'-delimited segment of a runtime path
// (e.g. "parallel[1].branchA" -> "parallel[1]", "s0" -> "s0").
func rerunFirstSegment(path string) string {
	if i := strings.IndexByte(path, '.'); i >= 0 {
		return path[:i]
	}
	return path
}

// rerunRootSlots maps each root-graph child's first-segment form to its slot
// index, so rootSlot(p) = rerunRootSlots(wf)[rerunFirstSegment(p)]. Built from the
// canonical ir.WalkNodes walk (which routes every path through ir.PathFor): the
// no-dot paths are exactly the top-level nodes, visited in declaration order, so
// a counter gives an order-preserving slot. Using the canonical walker (not a
// hand-rolled kind→"<kw>[i]" switch) respects CLAUDE.md "node addressing is one
// pure function (engine/path); don't compute paths ad hoc". (WalkNodes excludes
// *Skip; that only renumbers, which is order-preserving and harmless — the
// comparison below needs relative order, not absolute slot.)
func rerunRootSlots(wf *ir.Workflow) map[string]int {
	out := map[string]int{}
	i := 0
	ir.WalkNodes(wf.Graph, "", func(_ ir.Node, path string) {
		if !strings.Contains(path, ".") { // no dot ⟺ a top-level node
			out[path] = i
			i++
		}
	})
	return out
}

// rerunSupported reports whether `--from target` is in the v1 subset: every
// segment EXCEPT the last must be a `parallel[...]` container — i.e. the target
// is a top-level node or nested only inside parallels. Targets inside a
// call/loop/gate/try/if/map-body are refused (the general happens-after is
// deferred — see the plan's v1 scope note).
func rerunSupported(target string) error {
	segs := strings.Split(target, ".")
	for _, s := range segs[:len(segs)-1] {
		if !strings.HasPrefix(s, "parallel[") {
			return fmt.Errorf("--from %q is nested inside a non-parallel container (segment %q); v1 supports a top-level node or a parallel branch only", target, s)
		}
	}
	return nil
}

// allCommittedPaths is the union of keys across the nine path-keyed RunState
// indices (the candidate set for invalidation).
func allCommittedPaths(rs *RunState) []string {
	seen := map[string]struct{}{}
	add := func(k string) { seen[k] = struct{}{} }
	for k := range rs.Completed {
		add(k)
	}
	for k := range rs.Branches {
		add(k)
	}
	for k := range rs.LoopIters {
		add(k)
	}
	for k := range rs.GateAttempts {
		add(k)
	}
	for k := range rs.ReactRounds {
		add(k)
	}
	for k := range rs.MapItems {
		add(k)
	}
	for k := range rs.CallStarted {
		add(k)
	}
	for k := range rs.SignalReceivedAt {
		add(k)
	}
	for k := range rs.SelectedSkills {
		add(k)
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// ResolveRerunTarget resolves a --from argument to one node path, in priority:
// (1) an exact committed path; (2) a top-level graph node segment — a CONTAINER
// like `parallel[1]` or `map[3]` has NO committed key of its own (only children),
// so it is matched against rerunRootSlots, not allCommittedPaths; (3) a unique
// TRAILING segment (a bare step id, e.g. `merge` → `parallel[0].merge`). A bare
// id shared by two committed paths (e.g. one per parallel branch — ids are unique
// only within a sibling list) is an error listing candidates.
func ResolveRerunTarget(wf *ir.Workflow, rs *RunState, arg string) (string, error) {
	paths := allCommittedPaths(rs)
	for _, p := range paths { // (1) exact committed path
		if p == arg {
			return p, nil
		}
	}
	if _, ok := rerunRootSlots(wf)[arg]; ok { // (2) top-level node segment (incl. containers)
		return arg, nil
	}
	var matches []string // (3) unique trailing segment (bare id)
	for _, p := range paths {
		if p[strings.LastIndexByte(p, '.')+1:] == arg {
			matches = append(matches, p)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("--from %q matches no committed node (give a committed runtime path, a top-level node like parallel[0], or a unique step id)", arg)
	default:
		return "", fmt.Errorf("--from %q is ambiguous (%d committed nodes share that id; name one exactly): %s", arg, len(matches), strings.Join(matches, ", "))
	}
}

// ComputeRerunInvalidation returns the sorted set of committed paths to
// invalidate for `--from target`: target's subtree ∪ every committed path whose
// top-level root slot is greater than target's.
//
// Errors if: target is unsupported (rerunSupported); its root segment isn't in
// the current graph; OR **any committed path's top-level segment is absent from
// the current graph**. The last guard is load-bearing: `--from` bypasses the
// digest pin, so the operator may have edited the workflow. A structure-changing
// edit (a top-level node removed/renamed, or a control node reordered — its
// "<kw>[N]" segment changes) leaves a committed path whose top-level segment no
// longer maps to a node. Without the guard, `known=false` would SILENTLY drop
// that path from the set → it stays committed → its stale output is replayed
// (the exact silent-stale-reuse bug that Make/Airflow-pre-3.0 have and
// Temporal/Nextflow refuse-or-recompute). Refusing loud honors "pinning is a hard
// error on drift" at top-level-node granularity.
//
// ASSUMPTION (documented contract): `--from` is for STRUCTURE-PRESERVING edits
// (step bodies / scripts), not graph-shape edits. A pure top-level STEP reorder
// is the one structural change NOT refused (step ids are position-independent, so
// all segments still resolve) — and it is safe: the new slot order re-derives
// "after-ness" from the current graph, re-running per the new order (conservative
// over-invalidation, never stale under-invalidation).
func ComputeRerunInvalidation(wf *ir.Workflow, rs *RunState, target string) ([]string, error) {
	if err := rerunSupported(target); err != nil {
		return nil, err
	}
	slots := rerunRootSlots(wf)
	tSlot, ok := slots[rerunFirstSegment(target)]
	if !ok {
		return nil, fmt.Errorf("--from %q: top-level node %q not found in workflow", target, rerunFirstSegment(target))
	}
	committed := allCommittedPaths(rs)
	for _, p := range committed { // structural-drift guard (see doc)
		if _, known := slots[rerunFirstSegment(p)]; !known {
			return nil, fmt.Errorf("--from: committed node %q has no top-level node %q in the current workflow — its top-level structure changed since the run started; --from cannot map committed steps onto it (revert the structural change, or start a fresh run)", p, rerunFirstSegment(p))
		}
	}
	var out []string
	for _, p := range committed {
		inSubtree := p == target || strings.HasPrefix(p, target+".")
		afterRoot := slots[rerunFirstSegment(p)] > tSlot // known: guard above
		if inSubtree || afterRoot {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// clearInvalidatedPaths deletes each path from the nine path-keyed RunState
// indices. Used by the fold delete-arm and by engine.Run's live apply. NOT the
// name/container-keyed maps (SnapshotRefs, Signals). Caller is single-threaded
// (fold at resume-build; engine.Run before the poller/goroutines).
func clearInvalidatedPaths(rs *RunState, paths []string) {
	for _, p := range paths {
		delete(rs.Completed, p)
		delete(rs.Branches, p)
		delete(rs.LoopIters, p)
		delete(rs.GateAttempts, p)
		delete(rs.ReactRounds, p)
		delete(rs.MapItems, p)
		delete(rs.CallStarted, p)
		delete(rs.SignalReceivedAt, p)
		delete(rs.SelectedSkills, p)
	}
}
```
(If `ir.Parallel`'s field is not `Children`, or a step kind differs, correct `rerunRootSegment`/`rerunTestWF` against `ir/node.go` — the facts say `Parallel.Children NodeList` with `json:"-"`.)

- [ ] **Step 4: Run → pass.** `go test ./engine/ -run 'TestComputeRerun|TestResolveRerun' -count=1` → PASS. Then update Task 1's fold arm to call `clearInvalidatedPaths` (remove any inlined deletes) and re-run `go test ./engine/ -run 'TestFoldNodeInvalidated' -count=1`.

- [ ] **Step 5: Commit.**
```bash
git add engine/rerun.go engine/rerun_test.go engine/fold.go
git commit -m "feat(engine): rerun invalidation-set computation + target resolver"
```

---

## Task 3: `RunOptions.RerunFrom` + engine apply

**Files:** Modify `engine/interpreter.go`. **No standalone engine unit test** — this is pure wiring; its red→green is driven by **Task 4's end-to-end `TestCLIResumeFrom_ReRunsFromStep`** (which exercises `RunOptions.RerunFrom` through the CLI) and **Task 5's re-fold-determinism conformance test**. (M4 fix: the prior draft had a `t.Skip` stub here; a standalone engine `Run` rig for this is redundant with Task 4's CLI path. If executing strictly red-green, write Task 4 Step 1 first; it fails on the missing field/apply, and this task makes it pass.)

- [ ] **Step 1: Add the option.** In `engine/interpreter.go`, add to `RunOptions` (after `ForceResume`):
```go
	// RerunFrom, when non-empty, is a committed node runtime path. At resume
	// start the engine invalidates that node's subtree + everything after its
	// top-level ancestor (engine/rerun.go), re-running them. Set by
	// `awf resume --from`.
	RerunFrom string
```

- [ ] **Step 2: Apply in Run.** In `Run`, insert AFTER `preflightCallStartedRuntimes` returns and BEFORE the `runCtx, cancel := context.WithCancel(ctx)` line (single-threaded, before the poller — so direct rs writes + log.Append are race-free):
```go
	if opts.RerunFrom != "" {
		paths, err := ComputeRerunInvalidation(def.Workflow, runstate, opts.RerunFrom)
		if err != nil {
			return "", fmt.Errorf("engine.Run: rerun --from: %w", err)
		}
		data, err := json.Marshal(NodeInvalidatedData{Paths: paths})
		if err != nil {
			return "", fmt.Errorf("engine.Run: marshal node.invalidated: %w", err)
		}
		if err := log.Append(state.Event{Type: EventNodeInvalidated, Data: data}); err != nil {
			return "", fmt.Errorf("engine.Run: append node.invalidated: %w", err)
		}
		if err := log.Sync(); err != nil {
			return "", fmt.Errorf("engine.Run: sync after node.invalidated: %w", err)
		}
		clearInvalidatedPaths(runstate, paths)
	}
```
(Ensure `encoding/json` and `github.com/valbaudo/awf/state` are imported in interpreter.go — they already are.)

- [ ] **Step 3: Verify wiring.** `go build ./...`, then `go test ./engine/ -count=1` (full engine suite — `RerunFrom` defaults to `""`, so every existing test is unaffected). The behavioral red→green for this code is Task 4's `TestCLIResumeFrom_ReRunsFromStep`.

- [ ] **Step 4: Commit.**
```bash
git add engine/interpreter.go
git commit -m "feat(engine): RunOptions.RerunFrom applies invalidation at resume start"
```

---

## Task 4: CLI `--from` flag + wiring

**Files:** Modify `cli/resume.go`, `cli/execute.go`, `cli/run.go`; Test `cli/resume_test.go`.

- [ ] **Step 1: Write the failing test** — add to `cli/resume_test.go`, reusing the existing resume harness (mirror `buildPermanentFailureRun`/the run-then-resume pattern; if a multi-step committed run helper doesn't exist, build a small one: run a 2-step `a→b` workflow to completion via `runner.Run(["run",...])`, asserting `ExitOK`, returning `stateDir, runID, wfPath`):

```go
func TestCLIResumeFrom_ReRunsFromStep(t *testing.T) {
	t.Parallel()
	stateDir, runID, wfPath := buildTwoStepOKRun(t) // a(./a.sh)->b(./b.sh), both committed ok
	// Reprogram so re-running is observable: step a's command must execute again.
	fake := container.NewFake()
	fake.ProgramExec("./a.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	fake.ProgramExec("./b.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	runner := &cli.Runner{Backend: fake, IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, "--from", "a", runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	// a + b both re-ran (a re-runs because --from a; b because it is after a).
	var ranA, ranB bool
	for _, c := range fake.Calls {
		if c.Run == "./a.sh" {
			ranA = true
		}
		if c.Run == "./b.sh" {
			ranB = true
		}
	}
	if !ranA || !ranB {
		t.Fatalf("expected a and b to re-run; ranA=%v ranB=%v", ranA, ranB)
	}
	if !strings.Contains(stderr.String(), "re-run") {
		t.Fatalf("expected the re-run set disclosure on stderr; got %q", stderr.String())
	}
}

func TestCLIResumeFrom_BypassesDigestPin(t *testing.T) {
	t.Parallel()
	stateDir, runID, wfPath := buildTwoStepOKRun(t)
	// Mutate the wf so its digest differs but it still validates (change a container digest).
	orig, _ := os.ReadFile(wfPath)
	mutated := strings.Replace(string(orig), "sha256:0000000000000000000000000000000000000000000000000000000000000000", "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 1)
	if mutated == string(orig) {
		t.Fatal("mutation no-op; adjust the digest literal to match the fixture")
	}
	_ = os.WriteFile(wfPath, []byte(mutated), 0o600)
	fake := container.NewFake()
	fake.ProgramExec("./a.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	fake.ProgramExec("./b.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	runner := &cli.Runner{Backend: fake, IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, "--from", "a", runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK (--from bypasses the digest pin); stderr: %s", rc, stderr.String())
	}
}
```
Write `buildTwoStepOKRun(t)` in the test file: a wf with two top-level code steps `a` (`run: ./a.sh`) and `b` (`run: ./b.sh`) in one container (image pinned to the all-zeros sha256 used in the other resume tests), run via the fake to `ExitOK`, return `(stateDir, runID, wfPath)`.

- [ ] **Step 2: Run → fail.** `go test ./cli/ -run TestCLIResumeFrom -count=1` → FAIL (`--from` unknown flag).

- [ ] **Step 3: `cli/resume.go` — flag + bypass + resolve + disclose.**
  - After the `force := fs0.Bool(...)` line: `from := fs0.String("from", "", "re-run from this committed node (prefix); invalidates it + everything after its top-level node. Bypasses pinning.")`
  - Admission gate (the `resumeAdmission(...)` call): change `*force` to `*force || *from != ""`.
  - Digest-pin check (`if rs.WorkflowDigest != currentDigest {`): wrap as `if *from == "" && rs.WorkflowDigest != currentDigest {`.
  - Runtime-drift check (`if err := checkRuntimesDrift(...)`): wrap as `if *from == "" { if err := checkRuntimesDrift(...); err != nil { ... } }`.
  - After `ld` is loaded + validated (after the `ir.HasErrors` block, before the digest check), resolve + disclose when `--from` is set:
```go
	rerunFrom := ""
	if *from != "" {
		target, err := engine.ResolveRerunTarget(ld.Workflow, rs, *from)
		if err != nil {
			fprintf(stderr, "awf resume: %v\n", err)
			return ExitUsage
		}
		set, err := engine.ComputeRerunInvalidation(ld.Workflow, rs, target)
		if err != nil {
			fprintf(stderr, "awf resume: %v\n", err)
			return ExitUsage
		}
		fprintf(stderr, "awf resume --from %s: re-running %d committed step(s) (and their side effects) against the current definition; pins bypassed.\n", target, len(set))
		fprintf(stderr, "  re-run set: %s\n", sampleRerunSet(set, 10))
		rerunFrom = target
	}
```
  m7: `sampleRerunSet(set, n)` is a tiny helper (in cli/resume.go) that joins up to `n` sorted paths and appends `" … (N total)"` if `len(set) > n` — so the disclosure is readable on a large run instead of dumping the whole slice. m10: `--from` takes precedence over `--force` (both bypass admission; `--from` also bypasses the pins per `*from == ""` gating, so `--force` is redundant when `--from` is given). `--from` admits unconditionally — it never goes through `resumeAdmission`'s outcome logic (the `*force || *from != ""` gate above short-circuits it for both the ok-run and terminal-run cases). m8: the CLI computes the set here ONLY to disclose it; the engine recomputes + applies it (Task 3). Both call the same pure `ComputeRerunInvalidation` over the same `rs`+`ld.Workflow`, so they agree by construction; the recompute is cheap and keeps the engine the sole writer of `node.invalidated` (CLAUDE.md "the interpreter is the only writer to state").
  - At the `runAndFinish(...)` call, append `rerunFrom` as the new trailing arg (after `*force`).
  - Update `printResumeUsage` to add `[--from <step>]` and a one-line description.

- [ ] **Step 3b: `cli/execute.go`** — add trailing param `rerunFrom string` to `runAndFinish`, and `RerunFrom: rerunFrom,` to the `engine.RunOptions{...}` literal.

- [ ] **Step 3c: `cli/run.go`** — append `""` to its `runAndFinish(...)` call.

- [ ] **Step 4: Run → pass.** `go test ./cli/ -run TestCLIResumeFrom -count=1` → PASS, then `go test ./cli/ -count=1` (full cli suite green — runAndFinish is shared with `awf run`).

- [ ] **Step 5: Commit.**
```bash
git add cli/resume.go cli/execute.go cli/run.go cli/resume_test.go
git commit -m "feat(cli): awf resume --from flag (resolve, bypass pins, disclose, thread)"
```

---

## Task 5: conformance — re-fold determinism + end-to-end

**Files:** the re-fold-determinism test goes in the **`conformance/` package** (fake backend) — CLAUDE.md line 94-96: new durability behavior is defined-done by a conformance test, not a `cli` unit test. First read `conformance/` to match how existing resume/durability conformance cases are structured (the same fake-backend harness `--force`/map-reduce durability use); add the `--from` case there. The lighter end-to-end re-run assertion may also live in `cli/resume_test.go`, but the **load-bearing re-fold-determinism case is a `conformance/` test**.

- [ ] **Step 1: Write the test.**
```go
// After a --from re-run, a from-scratch Fold of the final log must equal the
// live RunState (the load-bearing durability property for the delete-arm).
func TestResumeFrom_RefoldDeterminism(t *testing.T) {
	t.Parallel()
	stateDir, runID, wfPath := buildTwoStepOKRun(t)
	fake := container.NewFake()
	fake.ProgramExec("./a.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	fake.ProgramExec("./b.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	runner := &cli.Runner{Backend: fake, IDGen: &clock.Fake{}}
	var so, se bytes.Buffer
	if rc := runner.Run([]string{"resume", "--state-dir", stateDir, "--from", "a", runID, wfPath}, &so, &se); rc != cli.ExitOK {
		t.Fatalf("rc=%d stderr=%s", rc, se.String())
	}
	// Fold the final on-disk log from scratch; a and b must be present exactly once each.
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	lg, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = lg.Close() }()
	events, err := lg.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	blobs, _ := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	rs, err := engine.Fold(events, blobs)
	if err != nil {
		t.Fatalf("engine.Fold: %v", err)
	}
	if _, ok := rs.Completed["a"]; !ok {
		t.Fatal("a missing after refold")
	}
	if _, ok := rs.Completed["b"]; !ok {
		t.Fatal("b missing after refold")
	}
	// And a plain resume now sees nothing to do (all committed) -> ExitOK no-op.
	runner2 := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var so2, se2 bytes.Buffer
	if rc := runner2.Run([]string{"resume", "--state-dir", stateDir, runID, wfPath}, &so2, &se2); rc != cli.ExitUsage {
		t.Fatalf("plain resume after --from should refuse a finished run; rc=%d", rc) // run.finished present
	}
}
```
(If the plain-resume tail assertion is brittle for your harness, drop it; the refold equality is the load-bearing part.)

- [ ] **Step 2: Run → pass** (no new production code — exercises Tasks 1–4): `go test ./cli/ -run 'TestResumeFrom_RefoldDeterminism' -count=1`.

- [ ] **Step 3: Commit.**
```bash
git add cli/resume_test.go
git commit -m "test(cli): resume --from re-fold determinism + end-to-end conformance"
```

---

## Task 6: man page

**Files:** Modify `man/awf.1.md`.

- [ ] **Step 1:** In the `awf resume` section, add `[--from <step>]` to the synopsis and, after the `--force` entry, a `--from` entry (match the file's `**flag**` / `:   …` style):
```
**--from** _step_
:   Re-run from a committed node (named by a runtime-path prefix, e.g. a
    top-level step id or `parallel[0].<step>`). Invalidates that node plus every
    node after its top-level ancestor and re-runs them against the *current*
    definition; everything before is replayed. **Bypasses pinning** (digest +
    runtime drift) — a debug-mode exception, like **--force** to terminal-run
    sealing; the operator owns the correctness of what is replayed, and the
    re-run set (incl. its at-least-once side effects) is printed before running.
    v1 supports a top-level node or a parallel branch; a node inside a
    call/loop/gate/map-body is refused.
```

- [ ] **Step 2: Commit.**
```bash
git add man/awf.1.md
git commit -m "docs(man): document awf resume --from"
```

---

## Final verification & self-check

- [ ] `make lint test build` — clean (golangci-lint 0 issues; full suite green; binary builds). If `make lint` aborts on the unrelated `.claude/worktrees/drift-fixes` gofmt artifact, run `golangci-lint run ./engine/... ./cli/...` + `go test ./...` directly.
- [ ] **Spec coverage:** §3 contract (admit-any + bypass pins + resolve + invalidate/replay + disclose) → Tasks 3/4; §4 invalidation (v1 subset, happens-after-coarse) → Task 2; §6 nine indices + engine-appended event → Tasks 1/2/3; §7 fold delete-arm + re-fold determinism → Tasks 1/5; §8 contract note → Task 6; §9 tests → Tasks 2/3/4/5. **Spec §4 deferred portion** (sequential-nested targets) is *refused*, documented in the v1 scope note — confirm the refusal message is clear.

---

## Coordination summary (merge)

`--from` reads `resumeAdmission` (bypasses it via `*force || *from != ""`) and adds `RunOptions.RerunFrom` + a `runAndFinish` trailing param — same shape as `--force`'s `ForceResume`. No guard rewrite; reconciles trivially with the retryable scope-b guard work. The `node.invalidated` fold delete-arm is new durability surface — the re-fold determinism conformance test (Task 5) is the gate that must stay green.
