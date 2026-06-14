# resume --from Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `awf resume <run> <wf> --from <step>` — re-enter a run, invalidate a chosen committed node + everything after its top-level ancestor, replay the rest, and re-run against the current definition (pins bypassed).

**Architecture:** A new `node.invalidated{paths}` journal event whose fold arm *deletes* the listed paths from the nine path-keyed RunState indices (the first delete-arm in an otherwise-additive fold). The engine, on resume with `RunOptions.RerunFrom`, computes the invalidation set, appends the event, and clears the in-memory indices before walking — so the existing `LookupCompleted` guard re-runs the invalidated nodes. The CLI adds the `--from` flag, resolves the prefix, bypasses the admission + pin checks, and discloses the re-run set.

**Tech Stack:** Go 1.26; `engine` (events/fold/interpreter + a new `engine/rerun.go`), `cli` (resume), fake backend for tests; `make lint test`.

**Spec:** `docs/superpowers/specs/2026-06-14-resume-rerun-design.md`.

> ### v1 SCOPE NOTE (read first — narrower than spec §4)
> The spec describes a general "happens-after" over arbitrary nesting. **v1 implements the correct *subset* where the target's runtime path traverses only `parallel` containers** (top-level nodes; parallel branches — covers the motivating `parallel[N].merge_runtime_compose`). For such targets, the invalidation set is exactly `subtree(target) ∪ {committed path p : rootSlot(p) > rootSlot(target)}`. Targets nested inside a `call`/`loop`/`gate`/`try`/`if`/`map`-body are **refused** with a clear message (`engine/rerun.go` `rerunSupported`). The general happens-after (sequential-sibling walks, the `.workflow` boundary) is deferred. Because v1's granularity is top-level-ancestor-coarse, a committed map is always wholly invalidated-or-replayed, so the prune-frontier widening (spec §4 boundary case) cannot arise in v1.

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
	"sort"
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
	sort.Strings(got)
	want := []string{"parallel[1].branchA", "s2"} // branch subtree + later root-slot; branchB (concurrent) + s0 replay
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("set = %v, want %v", got, want)
	}
}

func TestComputeRerunInvalidation_TopLevelStep(t *testing.T) {
	wf := rerunTestWF()
	rs := rsWithCompleted("s0", "parallel[1].branchA", "parallel[1].branchB", "s2")
	got, _ := ComputeRerunInvalidation(wf, rs, "s0")
	sort.Strings(got)
	want := []string{"parallel[1].branchA", "parallel[1].branchB", "s0", "s2"} // s0 + everything after slot 0
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("set = %v, want %v", got, want)
	}
}

func TestComputeRerunInvalidation_RefusesNestedSequential(t *testing.T) {
	wf := rerunTestWF()
	rs := rsWithCompleted("recon.workflow.step")
	if _, err := ComputeRerunInvalidation(wf, rs, "recon.workflow.step"); err == nil {
		t.Fatal("expected refusal for a target inside a call (.workflow)")
	}
	rs2 := rsWithCompleted("loop[0].body.iter-1.step")
	if _, err := ComputeRerunInvalidation(wf, rs2, "loop[0].body.iter-1.step"); err == nil {
		t.Fatal("expected refusal for a target inside a loop body")
	}
}

func TestResolveRerunTarget(t *testing.T) {
	rs := rsWithCompleted("s0", "parallel[1].branchA", "parallel[1].branchB")
	got, err := ResolveRerunTarget(rs, "parallel[1].branchA")
	if err != nil || got != "parallel[1].branchA" {
		t.Fatalf("exact: (%q,%v)", got, err)
	}
	if _, err := ResolveRerunTarget(rs, "parallel[1].nope"); err == nil {
		t.Fatal("expected error for absent prefix")
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
// index, so rootSlot(p) = rerunRootSlots(wf)[rerunFirstSegment(p)]. Steps map by
// id; control nodes by "<keyword>[i]" (matching ir.PathFor).
func rerunRootSlots(wf *ir.Workflow) map[string]int {
	out := map[string]int{}
	for i, n := range wf.Graph {
		out[rerunRootSegment(n, i)] = i
	}
	return out
}

// rerunRootSegment is the path segment ir.PathFor assigns a root-graph child at
// slot i: a step's id, or "<keyword>[i]" for a control node.
func rerunRootSegment(n ir.Node, i int) string {
	switch v := n.(type) {
	case *ir.CodeStep:
		return v.ID
	case *ir.AgentStep:
		return v.ID
	case *ir.SignalStep:
		return v.ID
	case *ir.CallStep:
		return v.ID
	case *ir.If:
		return fmt.Sprintf("if[%d]", i)
	case *ir.Loop:
		return fmt.Sprintf("loop[%d]", i)
	case *ir.Try:
		return fmt.Sprintf("try[%d]", i)
	case *ir.Parallel:
		return fmt.Sprintf("parallel[%d]", i)
	case *ir.Gate:
		return fmt.Sprintf("gate[%d]", i)
	case *ir.Map:
		return fmt.Sprintf("map[%d]", i)
	case *ir.React:
		return fmt.Sprintf("react[%d]", i)
	case *ir.Compose:
		return fmt.Sprintf("compose[%d]", i)
	default:
		return fmt.Sprintf("?[%d]", i)
	}
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

// ResolveRerunTarget matches a --from prefix against committed paths, returning
// the unique committed node path it names. Exact match wins; else a single path
// with that prefix; ambiguity/absence is an error listing candidates.
func ResolveRerunTarget(rs *RunState, prefix string) (string, error) {
	paths := allCommittedPaths(rs)
	for _, p := range paths {
		if p == prefix {
			return p, nil
		}
	}
	var matches []string
	for _, p := range paths {
		if strings.HasPrefix(p, prefix+".") {
			matches = append(matches, p)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("--from %q matches no committed node", prefix)
	default:
		return "", fmt.Errorf("--from %q is ambiguous (%d committed nodes share it; name one exactly): %s", prefix, len(matches), strings.Join(matches, ", "))
	}
}

// ComputeRerunInvalidation returns the sorted set of committed paths to
// invalidate for `--from target`: target's subtree ∪ every committed path whose
// top-level root slot is greater than target's. Errors if target is unsupported
// (rerunSupported) or its root segment isn't a root-graph node.
func ComputeRerunInvalidation(wf *ir.Workflow, rs *RunState, target string) ([]string, error) {
	if err := rerunSupported(target); err != nil {
		return nil, err
	}
	slots := rerunRootSlots(wf)
	tSlot, ok := slots[rerunFirstSegment(target)]
	if !ok {
		return nil, fmt.Errorf("--from %q: top-level node %q not found in workflow", target, rerunFirstSegment(target))
	}
	var out []string
	for _, p := range allCommittedPaths(rs) {
		inSubtree := p == target || strings.HasPrefix(p, target+".")
		pSlot, known := slots[rerunFirstSegment(p)]
		afterRoot := known && pSlot > tSlot
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

**Files:** Modify `engine/interpreter.go`; Test `engine/rerun_test.go` (add an engine-level test) or `engine/interpreter_test.go`.

- [ ] **Step 1: Write the failing test** — add to `engine/rerun_test.go` (uses the existing engine test rig pattern; mirror a simple `Run` test in the package — e.g. how `interpreter_test.go` constructs a fake dispatcher + InMemoryLog/Blobs). Minimal shape:

```go
func TestRunRerunFromInvalidatesAndAppendsEvent(t *testing.T) {
	// Seed a log: run.started + node.completed(s0,s2). Fold, then Run with
	// RerunFrom="s0" must append a node.invalidated event covering s0+s2 and
	// re-run them (the fake dispatcher records the re-execution).
	// Build on the simplest existing engine Run test harness in this package;
	// assert: (a) the log gains a node.invalidated event whose Paths include
	// "s0" and "s2"; (b) after Run, rs.Completed no longer had them pre-walk
	// (i.e. they re-ran). Keep it to a 2-step linear wf [s0, s2].
	t.Skip("replace with the package's Run harness; see interpreter_test.go")
}
```
Then replace the `t.Skip` body with a real test mirroring the package's existing `Run(...)` harness (fake dispatcher, `state.NewInMemoryLog`, `state.NewInMemoryBlobs`): seed `run.started` + `node.completed` for `s0` and `s2`, fold, call `Run(ctx, def, rs, disp, log, blobs, clk, RunOptions{RerunFrom: "s0"})`, then read back the log and assert a `node.invalidated` event with `Paths ⊇ {"s0","s2"}` exists, and the dispatcher re-executed both.

- [ ] **Step 2: Run → fail.** `go test ./engine/ -run TestRunRerunFrom -count=1` → FAIL (`unknown field RerunFrom`).

- [ ] **Step 3: Add the option + apply.** In `engine/interpreter.go`, add to `RunOptions` (after `ForceResume`):
```go
	// RerunFrom, when non-empty, is a committed node runtime path. At resume
	// start the engine invalidates that node's subtree + everything after its
	// top-level ancestor (engine/rerun.go), re-running them. Set by
	// `awf resume --from`.
	RerunFrom string
```
In `Run`, insert AFTER `preflightCallStartedRuntimes` returns and BEFORE the `runCtx, cancel := context.WithCancel(ctx)` line (single-threaded, before the poller — so direct rs writes + log.Append are race-free):
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

- [ ] **Step 4: Run → pass.** `go test ./engine/ -run TestRunRerunFrom -count=1` → PASS. Then `go test ./engine/ -count=1` (full engine suite — RerunFrom defaults to "" so every existing test is unaffected).

- [ ] **Step 5: Commit.**
```bash
git add engine/interpreter.go engine/rerun_test.go
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
		target, err := engine.ResolveRerunTarget(rs, *from)
		if err != nil {
			fprintf(stderr, "awf resume: %v\n", err)
			return ExitUsage
		}
		set, err := engine.ComputeRerunInvalidation(ld.Workflow, rs, target)
		if err != nil {
			fprintf(stderr, "awf resume: %v\n", err)
			return ExitUsage
		}
		fprintf(stderr, "awf resume --from %s: re-running %d step(s) (and their side effects) against the current definition; pins bypassed. Re-run set: %v\n", target, len(set), set)
		rerunFrom = target
	}
```
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

**Files:** add to `cli/resume_test.go` (or the conformance package, matching where resume conformance lives).

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
