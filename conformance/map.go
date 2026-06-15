package conformance

import (
	"encoding/json"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// testMap runs Bucket 7 sub-tests for the map executor (Phase 3 slice 3.4).
// Each sub-test pins a load-bearing invariant from spec §5.7 + design §E:
//
//   - map_per_item_commits: 3 items pass; log has 3 distinct map.item events
//     at addressable per-item paths.
//   - map_resume_skips_committed_items: round-1 commits 3 items as passed;
//     round-2 resumes against a BARE fake (no programmed Exec entries) and
//     completes ok — proving committed items are REPLAYED, not re-executed
//     (any re-execution would error against the bare fake and produce a
//     duplicate map.item event, which the assertions catch).
//   - map_skip_in_item_records_passed: skip inside an item commits item_passed
//     (design §E step 5).
//   - map_retryable_item_recovers_on_resume: round-1 has item b exit 1
//     (retryable_failure → item_failed); the map returns retryable_failure.
//     Round-2 resumes: b is re-run and succeeds; a,c replay from the log
//     (not re-executed). Map completes ok.
//
// Per CLAUDE.md ("the conformance suite is the definition of done... new
// durability behavior needs a conformance test"). Slice-3.4 deliberate
// deviation from Phase 3 design decision 11 ("no new bucket for 3.4").
func testMap(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("map_per_item_commits", func(t *testing.T) { testMapPerItemCommits(t, factory) })
	t.Run("map_resume_skips_committed_items", func(t *testing.T) { testMapResumeSkipsCommittedItems(t, factory) })
	t.Run("map_skip_in_item_records_passed", func(t *testing.T) { testMapSkipInItemRecordsPassed(t, factory) })
	t.Run("map_retryable_item_recovers_on_resume", func(t *testing.T) { testMapRetryableItemRecoversOnResume(t, factory) })
}

func testMapPerItemCommits(t *testing.T, factory BackendFactory) {
	t.Helper()
	programmedFactory := preProgramFake(t, factory, []execProgram{
		{cmd: "./process.sh a", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./process.sh b", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./process.sh c", res: container.ExecResult{ExitCode: 0}},
	})
	// newHarnessWithInput pre-binds the `items` array so the map's
	// `over: "{{ input.items }}"` resolves at runtime (harness extension
	// added in Task 10 Step 3).
	h := newHarnessWithInput(t, programmedFactory, mapStandardWorkflow,
		map[string]any{"items": []any{"a", "b", "c"}})
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("Bucket 7 map_per_item_commits: err = %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Bucket 7 map_per_item_commits: outcome = %q, want ok", oc)
	}
	events := mustFoldEvents(t, h)
	var mapItems int
	itemPaths := map[string]bool{}
	for _, e := range events {
		if e.Type == engine.EventMapItem {
			mapItems++
		}
		if e.Type == engine.EventNodeCompleted {
			itemPaths[e.Path] = true
		}
	}
	if mapItems != 3 {
		t.Errorf("Bucket 7 map_per_item_commits: map.item events = %d, want 3", mapItems)
	}
	for _, want := range []string{"map[0].item-0.process", "map[0].item-1.process", "map[0].item-2.process"} {
		if !itemPaths[want] {
			t.Errorf("Bucket 7 map_per_item_commits: missing per-item commit path %q; got paths %v", want, itemPaths)
		}
	}
}

func testMapResumeSkipsCommittedItems(t *testing.T, factory BackendFactory) {
	t.Helper()
	// Per spec §8 + design Q6: committed items (item_passed OR item_failed)
	// are REPLAYED on resume, not re-executed. This sub-test pins the
	// "no re-execution of committed items" half of that contract; an engine
	// unit test (TestRunMapResumeFailedItemsStayFailed) covers the failed-
	// item case.
	//
	// Strategy: Round 1 runs to completion (all items pass; 3 map.item
	// events committed). Round 2 resumes against a BARE FAKE (no programmed
	// Exec entries). If runMap correctly skips committed items, NO body
	// Exec calls happen on round 2; if it incorrectly re-executes, the
	// bare fake's "no programmed result" error fails the steps → produces
	// NEW map.item events (item_failed) — the duplicate-N assertion below
	// catches that.

	// Round 1: program all 3 to succeed → run completes ok with 3 map.item events.
	factory1 := preProgramFake(t, factory, []execProgram{
		{cmd: "./process.sh a", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./process.sh b", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./process.sh c", res: container.ExecResult{ExitCode: 0}},
	})
	h := newHarnessWithInput(t, factory1, mapStandardWorkflow,
		map[string]any{"items": []any{"a", "b", "c"}})
	oc1, err1 := h.runWorkflow(t)
	if err1 != nil {
		t.Fatalf("Bucket 7 map_resume: round-1 err = %v", err1)
	}
	if oc1 != engine.OutcomeOK {
		t.Fatalf("Bucket 7 map_resume: round-1 outcome = %q, want ok", oc1)
	}

	preEvents := mustFoldEvents(t, h)
	var preMapItems int
	for _, e := range preEvents {
		if e.Type == engine.EventMapItem {
			preMapItems++
		}
	}
	if preMapItems != 3 {
		t.Fatalf("Bucket 7 map_resume: round-1 map.item count = %d, want 3", preMapItems)
	}

	// Round 2: BARE fake (no programmed entries). If runMap skips committed
	// items correctly, no Exec calls → no errors. If it incorrectly re-runs
	// item bodies, the bare fake errors them → new map.item events.
	h.factory = factory // raw factory; preProgramFake NOT applied → no programs
	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Fatalf("Bucket 7 map_resume: round-2 err = %v (resume should have skipped committed items, not re-executed)", err2)
	}
	if oc2 != engine.OutcomeOK {
		t.Errorf("Bucket 7 map_resume: round-2 outcome = %q, want ok (committed items should replay-as-passed, not re-execute)", oc2)
	}

	// Verify: total map.item events still == 3 (no new commits from re-exec).
	// AND each N appears exactly once (the duplicate-N invariant from the
	// original slice-3.4 design).
	postEvents := mustFoldEvents(t, h)
	postNs := map[int]int{}
	for _, e := range postEvents {
		if e.Type == engine.EventMapItem {
			var d engine.MapItemData
			_ = json.Unmarshal(e.Data, &d)
			postNs[d.N]++
		}
	}
	totalMapItems := 0
	for _, count := range postNs {
		totalMapItems += count
	}
	if totalMapItems != 3 {
		t.Errorf("Bucket 7 map_resume: post-resume map.item total = %d, want 3 (resume must NOT re-execute committed items)", totalMapItems)
	}
	for n, count := range postNs {
		if count != 1 {
			t.Errorf("Bucket 7 map_resume: post-resume N=%d committed %d times; want 1 (committed items replay, do not duplicate)", n, count)
		}
	}
	for _, want := range []int{0, 1, 2} {
		if postNs[want] != 1 {
			t.Errorf("Bucket 7 map_resume: post-resume missing item N=%d", want)
		}
	}
}

func testMapSkipInItemRecordsPassed(t *testing.T, factory BackendFactory) {
	t.Helper()
	programmedFactory := preProgramFake(t, factory, []execProgram{
		{cmd: "./process.sh a", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./process.sh c", res: container.ExecResult{ExitCode: 0}},
		// "./process.sh b" deliberately NOT programmed — item-1's body
		// takes the `then: skip` branch, never reaching the else branch's
		// process step.
	})
	h := newHarnessWithInput(t, programmedFactory, mapSkipInItemWorkflow,
		map[string]any{"items": []any{"a", "b", "c"}})
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("Bucket 7 map_skip_in_item: err = %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Bucket 7 map_skip_in_item: outcome = %q, want ok (skip in item ends item as passed)", oc)
	}
	events := mustFoldEvents(t, h)
	itemNStatus := map[int]string{}
	for _, e := range events {
		if e.Type == engine.EventMapItem {
			var d engine.MapItemData
			_ = json.Unmarshal(e.Data, &d)
			itemNStatus[d.N] = d.Status
		}
	}
	if len(itemNStatus) != 3 {
		t.Fatalf("Bucket 7 map_skip_in_item: map.item count = %d, want 3", len(itemNStatus))
	}
	for n, status := range itemNStatus {
		if status != engine.ItemPassed {
			t.Errorf("Bucket 7 map_skip_in_item: item N=%d status=%q, want item_passed (skip ends item as ok per design §E step 5)", n, status)
		}
	}
}

func testMapRetryableItemRecoversOnResume(t *testing.T, factory BackendFactory) {
	t.Helper()
	// §6.5 resume recovery: round-1 has item b exit 1 (transient → item_failed /
	// retryable_failure); the map returns retryable_failure (default min_success
	// requires ALL items). Round-2 resumes with RunOptions.Resume=true (wired in
	// harness Step 1): shouldRerunItem selects b; a and c REPLAY from the log
	// without re-executing. The map completes ok.
	//
	// Round 1: a,c succeed; b exits 1.
	factory1 := preProgramFake(t, factory, []execProgram{
		{cmd: "./process.sh a", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./process.sh b", res: container.ExecResult{ExitCode: 1}},
		{cmd: "./process.sh c", res: container.ExecResult{ExitCode: 0}},
	})
	h := newHarnessWithInput(t, factory1, mapStandardWorkflow,
		map[string]any{"items": []any{"a", "b", "c"}})
	oc1, err1 := h.runWorkflow(t)
	if oc1 != engine.OutcomeRetryableFailure {
		t.Fatalf("map_retryable_item_recovers: round-1 outcome = %q (err=%v), want retryable_failure", oc1, err1)
	}

	// Count map.item events after round-1: a,b,c all committed (a=passed,
	// b=failed, c=passed).
	events1 := mustFoldEvents(t, h)
	var round1Items int
	for _, e := range events1 {
		if e.Type == engine.EventMapItem {
			round1Items++
		}
	}
	if round1Items != 3 {
		t.Fatalf("map_retryable_item_recovers: round-1 map.item count = %d, want 3", round1Items)
	}

	// Round 2: re-program b to succeed; a,c should NOT be re-executed (they are
	// committed as item_passed and will replay). Provide only process.sh b in the
	// fresh factory — a bare factory for a,c so any re-execution would error,
	// catching any regression.
	factory2 := preProgramFake(t, factory, []execProgram{
		{cmd: "./process.sh b", res: container.ExecResult{ExitCode: 0}},
	})
	h.factory = factory2
	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Fatalf("map_retryable_item_recovers: round-2 err = %v", err2)
	}
	if oc2 != engine.OutcomeOK {
		t.Errorf("map_retryable_item_recovers: round-2 outcome = %q, want ok (b recovered)", oc2)
	}

	// Post-resume: a (N=0) and c (N=2) each have exactly 1 map.item event (not
	// re-executed); b (N=1) has 2 map.item events (failed in round-1, passed in
	// round-2). Total = 4.
	events2 := mustFoldEvents(t, h)
	nCounts := map[int]int{}
	for _, e := range events2 {
		if e.Type == engine.EventMapItem {
			var d engine.MapItemData
			_ = json.Unmarshal(e.Data, &d)
			nCounts[d.N]++
		}
	}
	total := 0
	for _, cnt := range nCounts {
		total += cnt
	}
	if total != 4 {
		t.Errorf("map_retryable_item_recovers: post-resume map.item total = %d, want 4 (3 round-1 + 1 re-run of b)", total)
	}
	if nCounts[0] != 1 {
		t.Errorf("map_retryable_item_recovers: item a (N=0) re-executed on resume (count=%d, want 1)", nCounts[0])
	}
	if nCounts[2] != 1 {
		t.Errorf("map_retryable_item_recovers: item c (N=2) re-executed on resume (count=%d, want 1)", nCounts[2])
	}
	if nCounts[1] != 2 {
		t.Errorf("map_retryable_item_recovers: item b (N=1) events = %d, want 2 (fail in round-1, pass in round-2)", nCounts[1])
	}
}
