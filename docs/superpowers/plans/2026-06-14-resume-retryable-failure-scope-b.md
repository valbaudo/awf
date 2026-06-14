# Resume transiently-failed runs — Scope B Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. **Depends on Scope A (the relaxed guard) being merged first.**

**Goal:** On resume of a `retryable_failure` run, re-run only the *transiently*-failed items of a plain (`map.item`) map — recovering the map — while replaying passed/pruned/permanently-failed/rejected items unchanged. Prune and reduce maps need no per-item re-run machinery.

**Architecture:** Record each failed item's underlying outcome on `map.item` (so retryable can be distinguished from permanent/rejected). Guarantee a single `MapItemRecord` per item-N in both the fold and the live `RunState`. Gate the re-run on a `Resume` flag threaded from the resume CLI call into the map reconciliation, with a pure predicate that excludes prune maps and `image_unavailable` items. Reduce maps are safe by construction (the reducer is always uncommitted on the retryable path and recomputes over the full recovered set).

**Tech Stack:** Go 1.26; module `github.com/valbaudo/awf`. Verification is **`make lint test`** (golangci-lint). Engine tests are white-box (`package engine`); the conformance harness drives engine-level resume (it bypasses `cli/resume.go`, so guard behavior is Scope A's concern).

**Spec:** `docs/superpowers/specs/2026-06-14-resume-retryable-failure-design.md` (§6.1 outcome field, §6.2 single-record, §6.3 predicate/resume-flag/image, §6.4 prune, §6.5 reduce).

---

### Task 1: Record the item outcome on `map.item`

`MapItemData`/`MapItemRecord` have no `Outcome` field today; `dispatchItem` collapses every non-ok body to `ItemFailed` and discards `bodyOC`. Add the field and thread the body outcome through (non-prune path only — prune is excluded in Task 3).

**Files:**
- Modify: `engine/events.go` (`MapItemData`)
- Modify: `engine/runstate.go` (`MapItemRecord`)
- Modify: `engine/map.go` (`dispatchItem`, `commitMapItem`)
- Modify: `engine/fold.go` (`map.item` arm)
- Test: `engine/map_test.go`

- [ ] **Step 1: Write the failing test**

In `engine/map_test.go`:
```go
func TestMapItemRecordsRetryableOutcome(t *testing.T) {
	rig := newMapRig(t, fail("echo a")) // exit 1 → retryable_failure
	input := runOverItems("a")
	seedRunStartedWithInput(t, rig.lg, rig.blobs, input)
	minSuccess := ir.Ratio("1")
	wf := staticOverWorkflow("x", echoStep("x", &ir.RetryPolicy{Attempts: 1}), 1, &minSuccess)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, input)

	_, _ = runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)

	// Fold the log (resume's path) — the folded record must carry the outcome.
	rs2 := foldFromRig(t, rig)
	items := rs2.LookupMapItems(testMapPath)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].Status != ItemFailed {
		t.Errorf("Status = %q, want %q", items[0].Status, ItemFailed)
	}
	if items[0].Outcome != string(OutcomeRetryableFailure) {
		t.Errorf("Outcome = %q, want %q", items[0].Outcome, OutcomeRetryableFailure)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./engine/ -run TestMapItemRecordsRetryableOutcome -v`
Expected: FAIL — `items[0].Outcome undefined` (field missing).

- [ ] **Step 3: Add the `Outcome` field to `MapItemData`**

In `engine/events.go`, `MapItemData` — add after `Status`:
```go
	// Outcome is the item body's rolled-up mechanical outcome when Status ==
	// item_failed: "retryable_failure" | "permanent_failure" | "rejected".
	// Empty for item_passed / item_pruned, for the image_unavailable path, and
	// for pre-this-change logs. Resume re-run selection (see runMap reconciliation)
	// re-runs ONLY a retryable item; permanent / rejected / absent replay-as-failed.
	Outcome string `json:"outcome,omitempty"`
```

- [ ] **Step 4: Add the `Outcome` field to `MapItemRecord`**

In `engine/runstate.go`, `MapItemRecord` — add after `Status`:
```go
	// Outcome mirrors MapItemData.Outcome (the item body's mechanical outcome on
	// failure). Drives resume re-run selection; empty for passed/pruned/image.
	Outcome string
```

- [ ] **Step 5: Capture `bodyOC` in `dispatchItem` and pass it to `commitMapItem`**

In `engine/map.go`, `dispatchItem`, replace the status-collapse block + the two `commitMapItem` returns:
```go
	status := ItemPassed // default optimistic; revised below
	itemOutcome := ""    // set only on a body failure (spec §6.1)
	var su *SkipUnwind
	if errors.As(bodyErr, &su) {
		if appErr := appendNodeSkipped(ictx.log, itemPath, su.Reason); appErr != nil {
			return "", fmt.Errorf("append node.skipped for item-%d: %w", itemN, appErr)
		}
	} else if bodyErr != nil || bodyOC != OutcomeOK {
		status = ItemFailed
		if bodyOC != OutcomeOK {
			itemOutcome = string(bodyOC) // retryable_failure | permanent_failure | rejected
		}
	}

	if pr != nil {
		return status, nil
	}
	return commitMapItem(ictx.log, ictx.runstate, mapPath, itemN, status, imageDigest, "", itemOutcome)
```
And the `image_unavailable` path (`map.go:108`) gains a trailing empty outcome (image_unavailable is non-rerunnable, §6.3):
```go
				return commitMapItem(ictx.log, ictx.runstate, mapPath, itemN, ItemFailed, "", ReasonImageUnavailable, "")
```

- [ ] **Step 6: Add the `outcome` param to `commitMapItem`**

In `engine/map.go`, `commitMapItem`:
```go
func commitMapItem(log state.Log, runstate *RunState, mapPath string, itemN int, status, imageDigest, reason, outcome string) (string, error) {
	data, mErr := json.Marshal(MapItemData{N: itemN, Status: status, ImageDigest: imageDigest, Reason: reason, Outcome: outcome})
	if mErr != nil {
		return "", fmt.Errorf("marshal map.item for item-%d: %w", itemN, mErr)
	}
	if err := log.Append(state.Event{Type: EventMapItem, Path: mapPath, Data: data}); err != nil {
		return "", fmt.Errorf("append map.item for item-%d: %w", itemN, err)
	}
	if err := log.Sync(); err != nil {
		return "", fmt.Errorf("sync log after map.item for item-%d: %w", itemN, err)
	}
	updateMapItemStatus(runstate, mapPath, itemN, status)
	return status, nil
}
```

- [ ] **Step 7: Fold the `Outcome` on the `map.item` arm**

In `engine/fold.go`, the `case EventMapItem:` arm — add `Outcome: d.Outcome,` to the appended record:
```go
			rs.MapItems[e.Path] = append(rs.MapItems[e.Path], MapItemRecord{
				N:           d.N,
				Status:      d.Status,
				Outcome:     d.Outcome,
				ImageDigest: d.ImageDigest,
				Reason:      d.Reason,
			})
```
(Leave the `EventMapFrontier` arm unchanged — prune frontier items carry no outcome; §6.4.)

- [ ] **Step 8: Run to verify pass**

Run: `go test ./engine/ -run TestMapItemRecordsRetryableOutcome -v`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add engine/events.go engine/runstate.go engine/map.go engine/fold.go engine/map_test.go
git commit -m "feat(engine): record per-item outcome on map.item (resume selection input)"
```

---

### Task 2: One `MapItemRecord` per item-N (fold + live)

A re-run item appends a second `map.item{N}` and a second live `RecordMapItem`. Make both the fold and the live insert single-valued per N (last-wins), which also fixes the value-binding staleness (`scope.go` first-match).

**Files:**
- Modify: `engine/runstate.go` (`RecordMapItem` → upsert)
- Modify: `engine/fold.go` (both arms route through `RecordMapItem`)
- Test: `engine/runstate_test.go`, `engine/fold_test.go` (or `engine/map_test.go`)

- [ ] **Step 1: Write the failing tests**

In `engine/runstate_test.go`:
```go
func TestRecordMapItemUpsertByN(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordMapItem("map[0]", MapItemRecord{N: 0, Status: ItemFailed, Outcome: "retryable_failure"})
	// Re-run re-records the same N with the recovered status + re-derived value.
	rs.RecordMapItem("map[0]", MapItemRecord{N: 0, Status: ItemPassed, ItemValue: "v"})

	got := rs.LookupMapItems("map[0]")
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (upsert by N, not append)", len(got))
	}
	if got[0].Status != ItemPassed || got[0].ItemValue != "v" {
		t.Errorf("record = %+v, want Status=item_passed ItemValue=v", got[0])
	}
}
```
In `engine/map_test.go` (white-box: build a log with two `map.item{N=0}` events, fold, assert one record):
```go
func TestFoldMapItemLastWinsByN(t *testing.T) {
	clk := &clock.Fake{T: testClockEpoch}
	lg := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	seedRunStartedWithInput(t, lg, blobs, runOverItems("a"))
	for _, d := range []MapItemData{
		{N: 0, Status: ItemFailed, Outcome: "retryable_failure"},
		{N: 0, Status: ItemPassed},
	} {
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := lg.Append(state.Event{Type: EventMapItem, Path: testMapPath, Data: b}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	events, err := lg.Fold()
	if err != nil {
		t.Fatalf("Fold log: %v", err)
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := rs.LookupMapItems(testMapPath)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (last-wins by N)", len(got))
	}
	if got[0].Status != ItemPassed {
		t.Errorf("Status = %q, want %q (last event wins)", got[0].Status, ItemPassed)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./engine/ -run 'TestRecordMapItemUpsertByN|TestFoldMapItemLastWinsByN' -v`
Expected: FAIL — both see `len = 2` (blind append).

- [ ] **Step 3: Make `RecordMapItem` upsert by N**

In `engine/runstate.go`:
```go
func (rs *RunState) RecordMapItem(mapPath string, mr MapItemRecord) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	items := rs.MapItems[mapPath]
	for i := range items {
		if items[i].N == mr.N {
			items[i] = mr // upsert: a re-run item replaces its prior record in place
			return
		}
	}
	rs.MapItems[mapPath] = append(items, mr)
}
```

- [ ] **Step 4: Route both fold arms through `RecordMapItem`**

In `engine/fold.go`, replace the inline `rs.MapItems[e.Path] = append(...)` in BOTH the `EventMapItem` arm and the `EventMapFrontier` loop with `rs.RecordMapItem(e.Path, MapItemRecord{...})` (same field sets as today, plus `Outcome: d.Outcome` on the `map.item` arm from Task 1). Fold is single-threaded; `RecordMapItem`'s lock is harmless. This makes fold last-wins-by-N identical to the live upsert.

- [ ] **Step 5: Run to verify pass**

Run: `go test ./engine/ -run 'TestRecordMapItemUpsertByN|TestFoldMapItemLastWinsByN' -v`
Expected: PASS. Then `go test ./engine/ -run 'TestRunStateMapItems|TestLookupMapItems|TestRunStateUpdateMapItemValue'` to confirm existing record round-trip/order tests still pass (upsert preserves position for existing N and append order for new N).

- [ ] **Step 6: Commit**

```bash
git add engine/runstate.go engine/fold.go engine/runstate_test.go engine/map_test.go
git commit -m "fix(engine): single MapItemRecord per N (upsert + fold last-wins)"
```

---

### Task 3: Resume flag + re-run predicate

Thread a `Resume` flag from the resume CLI into the map reconciliation, and re-run committed retryable items on resume via a pure predicate (prune-excluded, image_unavailable-excluded).

**Files:**
- Modify: `engine/interpreter.go` (`RunOptions`, `engine.Run` ictx)
- Modify: `engine/interpreter_context.go` (`resume` field)
- Modify: `engine/map.go` (`committedItem`, `shouldRerunItem`, reconciliation, predicate)
- Modify: `cli/execute.go` (`runAndFinish` param + `RunOptions`)
- Modify: `cli/run.go:360` (pass `false`), `cli/resume.go:401` (pass `true`)
- Test: `engine/map_test.go`

- [ ] **Step 1: Write the failing tests**

In `engine/map_test.go`:
```go
// Pure predicate: who re-runs on resume.
func TestShouldRerunItem(t *testing.T) {
	plain := &ir.Map{}                 // n.Prune == nil
	prune := &ir.Map{Prune: &ir.Prune{}} // any non-nil Prune
	cases := []struct {
		name   string
		n      *ir.Map
		resume bool
		ci     committedItem
		want   bool
	}{
		{"retryable-on-resume", plain, true, committedItem{status: ItemFailed, outcome: "retryable_failure"}, true},
		{"permanent-stays", plain, true, committedItem{status: ItemFailed, outcome: "permanent_failure"}, false},
		{"rejected-stays", plain, true, committedItem{status: ItemFailed, outcome: "rejected"}, false},
		{"absent-outcome-stays", plain, true, committedItem{status: ItemFailed, outcome: ""}, false},
		{"image-unavailable-stays", plain, true, committedItem{status: ItemFailed, outcome: "", reason: ReasonImageUnavailable}, false},
		{"passed-stays", plain, true, committedItem{status: ItemPassed}, false},
		{"pruned-stays", plain, true, committedItem{status: ItemPruned}, false},
		{"prune-map-never", prune, true, committedItem{status: ItemFailed, outcome: "retryable_failure"}, false},
		{"not-resume-never", plain, false, committedItem{status: ItemFailed, outcome: "retryable_failure"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRerunItem(c.n, c.resume, c.ci); got != c.want {
				t.Errorf("shouldRerunItem = %v, want %v", got, c.want)
			}
		})
	}
}

// Integration: a retryable item recovers on resume → map ok, single record per N.
func runMapResumeTrue(ctx context.Context, n *ir.Map, mapPath string, wf *ir.Workflow, rs *RunState, rig *mapRig) (Outcome, error) {
	return runMapWithContext(ctx, n, mapPath, interpreterContext{
		wf: wf, runstate: rs, dispatcher: rig.ld, log: rig.lg, blobs: rig.blobs, clk: rig.clk, resume: true,
	})
}

func TestRunMapResumeRetryableItemReRuns(t *testing.T) {
	rig1 := newMapRig(t, ok("echo a"), fail("echo b"), ok("echo c"))
	input := runOverItems("a", "b", "c")
	seedRunStartedWithInput(t, rig1.lg, rig1.blobs, input)
	minSuccess := ir.Ratio("3")
	wf := staticOverWorkflow("x", echoStep("x", &ir.RetryPolicy{Attempts: 1}), 3, &minSuccess)
	mapNode := wf.Graph[0].(*ir.Map)
	rs1 := NewRunState(testRunID, testDigest, input)

	oc1, _ := runMap(context.Background(), mapNode, testMapPath, wf, rs1, rig1.ld, rig1.lg, rig1.blobs, rig1.clk, nil, nil)
	if oc1 != OutcomeRetryableFailure {
		t.Fatalf("round-1 outcome = %q, want OutcomeRetryableFailure", oc1)
	}

	rig2 := bareRig(t, rig1, ok("echo a"), ok("echo b"), ok("echo c"))
	rs2 := foldFromRig(t, rig2)

	oc2, err2 := runMapResumeTrue(context.Background(), mapNode, testMapPath, wf, rs2, rig2)
	if oc2 != OutcomeOK || err2 != nil {
		t.Fatalf("resume outcome = %q err = %v, want OutcomeOK/nil (retryable item recovered)", oc2, err2)
	}
	if len(rig2.fake.Calls) == 0 {
		t.Error("resume did NOT re-run the failed item (fake.Calls empty)")
	}
	// Single record per N after re-run.
	seen := map[int]bool{}
	for _, mr := range rs2.LookupMapItems(testMapPath) {
		if seen[mr.N] {
			t.Errorf("duplicate MapItemRecord for N=%d", mr.N)
		}
		seen[mr.N] = true
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./engine/ -run 'TestShouldRerunItem|TestRunMapResumeRetryableItemReRuns' -v`
Expected: FAIL — `undefined: committedItem` / `shouldRerunItem` / `interpreterContext.resume`.

- [ ] **Step 3: Add the `resume` field to `interpreterContext` and `RunOptions`**

`engine/interpreter_context.go`, `interpreterContext` — add after `liveFinalizer`:
```go
	resume bool // true when re-entering a folded log (awf resume); gates map-item re-run
```
`engine/interpreter.go`, `RunOptions` — add a field:
```go
	// Resume is true on `awf resume` re-entry; the map reconciliation re-runs
	// transiently-failed items only when set (spec §6.3). false on fresh `awf run`.
	Resume bool
```
`engine/interpreter.go`, the `engine.Run` ictx literal (after `liveFinalizer: opts.LiveFinalizer,`):
```go
		resume:        opts.Resume,
```

- [ ] **Step 4: Add `committedItem` + `shouldRerunItem` and wire the reconciliation**

`engine/map.go` — add near `tallyResults`:
```go
// committedItem is the folded disposition the resume reconciliation needs to
// decide replay-vs-rerun for a map item.
type committedItem struct {
	status  string
	outcome string
	reason  string
}

// shouldRerunItem reports whether a committed map item must re-run on resume
// (spec §6.3). Only on resume, only for a NON-prune map (a prune disposition is
// an atomic frontier decision — never re-run one participating item, §6.4), only
// a retryable body failure; image_unavailable is digest-pinned and treated as
// non-rerunnable. Passed/pruned/permanent/rejected/absent all replay-as-committed.
func shouldRerunItem(n *ir.Map, resume bool, ci committedItem) bool {
	if !resume || n.Prune != nil {
		return false
	}
	if ci.status != ItemFailed || ci.reason == ReasonImageUnavailable {
		return false
	}
	return Outcome(ci.outcome) == OutcomeRetryableFailure
}
```
In `runMapWithContext`, change the `committed` map type + build (lines ~98-109):
```go
	committed := map[int]committedItem{}
	maxCommittedN := -1
	for _, mr := range ictx.runstate.LookupMapItems(mapPath) {
		committed[mr.N] = committedItem{status: mr.Status, outcome: mr.Outcome, reason: mr.Reason}
		if mr.N > maxCommittedN {
			maxCommittedN = mr.N
		}
		if mr.ItemValue == nil && mr.N < len(overArr) {
			ictx.runstate.UpdateMapItemValue(mapPath, mr.N, overArr[mr.N])
		}
	}
```
And the skip-and-replay short-circuit (lines ~154-162):
```go
		if ci, ok := committed[i]; ok {
			if !shouldRerunItem(n, ictx.resume, ci) {
				statuses[i] = ci.status
				continue
			}
			// retryable item on resume: fall through to re-dispatch. Task 2's
			// RecordMapItem upsert replaces the folded record by N.
		}
```
(The prune final-pass `if _, done := committed[i]; done` presence check is unaffected by the value-type change.)

- [ ] **Step 5: Thread the flag through the CLI**

`cli/execute.go`, `runAndFinish` — add a `resume bool` param after `successSuffix string`:
```go
	runID, opName, successSuffix string,
	resume bool,
```
and in its `engine.RunOptions{...}` literal add `Resume: resume,`.
`cli/run.go:360` — insert `false,` at the new param position:
```go
	return r.runAndFinish(ctx, backend, resolverOrEmpty(resolver), ld, rs, handles, log, blobs, stdout, stderr, id, "awf run", "", false, assetSnapshots, inputFileRefs, broker, liveRoot, &skipTeardown)
```
`cli/resume.go:401` — insert `true,` at the new param position:
```go
	return r.runAndFinish(ctx, backend, resolverOrEmpty(resolver), ld, rs, handles, log, blobs, stdout, stderr, runID, "awf resume", " (resumed)", true, recordedAssets, nil, broker, liveRoot, &skipTeardown)
```

- [ ] **Step 6: Run the engine + cli build/tests**

Run: `go test ./engine/ -run 'TestShouldRerunItem|TestRunMapResumeRetryableItemReRuns|TestRunMapResumeFailedItemsStayFailed' -v`
Expected: PASS — new tests green; `TestRunMapResumeFailedItemsStayFailed` (calls `runMap`, leaving `resume=false`) still asserts the failed item stays failed with no re-execution (the direct-caller contract).
Run: `go build ./... && go test ./cli/ -run TestCLIResume`
Expected: PASS (flag plumbing compiles; Scope A guard tests unaffected).

- [ ] **Step 7: Commit**

```bash
git add engine/interpreter.go engine/interpreter_context.go engine/map.go cli/execute.go cli/run.go cli/resume.go engine/map_test.go
git commit -m "feat: re-run transiently-failed map items on resume (Resume flag + predicate)"
```

---

### Task 4: Reduce safe-by-construction test, conformance, and stale-comment cleanup

**Files:**
- Modify: `conformance/harness.go` (set `RunOptions.Resume = isResume` in `runOrResume`)
- Test: `engine/reduce_test.go` (reduce recovery), `conformance/*_test.go` (map recovery)
- Modify: `engine/events.go` (two stale "two-value Status tally" comments)

- [ ] **Step 1: Wire the resume flag into the conformance harness**

In `conformance/harness.go`, `runOrResume(t, isResume)` constructs `engine.RunOptions` for its `engine.Run` call — add `Resume: isResume,` to that literal so a conformance `resumeWorkflow` exercises Scope B at the engine level. (Run `grep -n "engine.RunOptions" conformance/harness.go` to find the literal.)

- [ ] **Step 2: Reduce recovery test (engine)**

Add a test that a quorum reduce recovers on resume. **Construct the reduce map by mimicking the existing `engine/reduce_test.go` fixtures** (they show `ir.Map{Reduce: ...}` / `ir.Reduce{Over, Quorum}` shapes — this plan does not restate that IR verbatim to avoid drift; follow that file). The behavior to assert (spec §6.5):
```
round-1: quorum=N over N items, one item transiently fails → agree < N →
         runMap returns OutcomeRetryableFailure, reducer NOT committed.
resume (runMapResumeTrue): the failed item now passes → reducer re-runs over the
         full post-recovery passed set → agree == N → OutcomeOK.
```
Name it `TestRunMapReduceResumeQuorumRecovers`. Assert `oc2 == OutcomeOK` and that no committed reduce output from round-1 was blended (there is none — the reducer never committed on the retryable path; that is the safe-by-construction invariant).

- [ ] **Step 3: Map recovery conformance test**

Add a conformance test using the existing map fixture pattern (`conformance/fixtures.go` `mapStandardWorkflow`) + `preProgramFake` to fail one item transiently on the first run and succeed it on resume. Drive via `h.runWorkflow` then `h.resumeWorkflow` (engine-level; the Scope A guard is covered by `cli/resume_test.go`). Assert: first run → `OutcomeRetryableFailure`; resume → `OutcomeOK`; the passed items were not re-executed (check `fake.Calls` / per-item program counts). Follow the existing two-round conformance tests for the exact harness calls.

- [ ] **Step 4: Fix the stale "two-value Status tally" comments**

In `engine/events.go`: (1) the doc-comment above the status const block (~84-88) — change "a map item's body can succeed (item_passed) or fail (item_failed)" to acknowledge the third status `item_pruned`; (2) the `MapItemData.Reason` doc (~131-132) — change "the two-value Status tally" to "the three-value Status tally (item_passed / item_failed / item_pruned)".

- [ ] **Step 5: Full verification**

Run: `make lint test`
Expected: PASS — the whole suite green (golangci-lint clean: the new `resume` field and `Outcome` field are both read, so no unused-field findings).

- [ ] **Step 6: Commit**

```bash
git add conformance/ engine/reduce_test.go engine/events.go
git commit -m "test: reduce + map resume recovery; fix stale map-status comments"
```

---

## Self-Review

- **Spec coverage:** §6.1 outcome field → Task 1; §6.2 single-record → Task 2; §6.3 resume-flag + predicate + image_unavailable → Task 3; §6.4 prune exclusion → Task 3 (`shouldRerunItem` `n.Prune != nil`) + `TestShouldRerunItem` `prune-map-never`; §6.5 reduce safe-by-construction → Task 4 Steps 1-2. No gaps.
- **Type consistency:** `committedItem{status,outcome,reason}` defined in Task 3 Step 4, used in `TestShouldRerunItem` (Task 3 Step 1) and the reconciliation. `shouldRerunItem(n *ir.Map, resume bool, ci committedItem) bool` signature matches its test. `MapItemRecord.Outcome` (Task 1) read by Task 3's `committed` build. `commitMapItem(..., reason, outcome string)` (Task 1 Step 6) — both call sites in `dispatchItem` updated (Task 1 Step 5). `RunOptions.Resume`/`interpreterContext.resume`/`runAndFinish(... resume bool ...)` consistent across Task 3.
- **Uncertainty flagged (CLAUDE.md rule 4):** Task 4 Steps 1-3 (`conformance/harness.go` `RunOptions` literal location, the reduce IR shape, and the conformance map fixture) are specified by behavior + a pointer to the existing pattern files rather than verbatim, because those bodies were not captured during planning. The implementer must read `engine/reduce_test.go`, `conformance/harness.go`, and `conformance/fixtures.go` for the exact constructors. The engine reconciliation tasks (1-3) are fully verbatim.
- **Placeholders:** none (Task 4 pattern-pointers are explicit directives to existing files, not "TODO").
