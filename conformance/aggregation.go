package conformance

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// testAggregation is Bucket 17 (Phase 5 slice 5.5) — the end-to-end proof that
// map output aggregation (design spec 2026-06-03 §1) works against the fake
// backend, including resume. It pins the format-standard contract added to
// awf-workflow(5) §"TEMPLATING AND TYPED OUTPUTS":
//
//   - map_chains_to_map: map A scans 3 items into typed {finding,index}
//     outputs; map B's `over: "{{ step.scan }}"` lifts A's committed per-item
//     `scan` outputs to an index-ordered array, fanning B out over exactly the
//     aggregate length (3). Map B's body consumes `{{ f.finding }}` (object
//     item → field access). Run completes ok.
//   - map_chain_resume_replays: a post-completion resume against a BARE fake
//     (no programmed Exec) re-folds the log and completes ok — A's aggregate
//     replays identically and map B does NOT re-execute A's body (a re-run
//     would error against the bare fake). Total map.item / node.completed
//     counts are unchanged across resume.
//
// concurrency: 1 on both maps (fixture choice): aggregation is
// concurrency-independent, and the in-memory fake's shared Blobs race when
// multiple output_schema item bodies commit concurrently (a known fake
// limitation) — serializing sidesteps it without weakening what this pins.
func testAggregation(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("map_chains_to_map", func(t *testing.T) { testMapChainsToMap(t, factory) })
	t.Run("map_chain_resume_replays", func(t *testing.T) { testMapChainResumeReplays(t, factory) })
}

// aggregationPrograms returns the per-command exec programs for a successful
// run of mapAggregationWorkflow over items ["a","b","c"]:
//   - map A: `./scan.sh <item>` emits a distinct typed {finding,index} output.
//   - map B: `./verify.sh <finding>` runs once per aggregated finding.
//
// The findings are "finding-a"/"finding-b"/"finding-c"; map B's body templates
// `{{ f.finding }}` into the verify command, so the verify commands are keyed
// by those exact strings.
func aggregationPrograms() []execProgram {
	mk := func(item string, idx int) []byte {
		raw, _ := json.Marshal(map[string]any{"finding": "finding-" + item, "index": idx})
		return raw
	}
	return []execProgram{
		{cmd: "./scan.sh a", res: container.ExecResult{ExitCode: 0, AWFOutput: mk("a", 0)}},
		{cmd: "./scan.sh b", res: container.ExecResult{ExitCode: 0, AWFOutput: mk("b", 1)}},
		{cmd: "./scan.sh c", res: container.ExecResult{ExitCode: 0, AWFOutput: mk("c", 2)}},
		{cmd: "./verify.sh finding-a", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./verify.sh finding-b", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./verify.sh finding-c", res: container.ExecResult{ExitCode: 0}},
	}
}

func testMapChainsToMap(t *testing.T, factory BackendFactory) {
	t.Helper()
	programmedFactory := preProgramFake(t, factory, aggregationPrograms())
	h := newHarnessWithInput(t, programmedFactory, mapAggregationWorkflow,
		map[string]any{"items": []any{"a", "b", "c"}})
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("Bucket 17 map_chains_to_map: err = %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Bucket 17 map_chains_to_map: outcome = %q, want ok", oc)
	}

	events := mustFoldEvents(t, h)
	mapAItems, mapBItems := countMapItems(events)
	if mapAItems != 3 {
		t.Errorf("Bucket 17 map_chains_to_map: map A (map[0]) item count = %d, want 3", mapAItems)
	}
	// The load-bearing assertion: map B fanned out over A's AGGREGATE — one
	// item per committed scan output. All 3 of A's items committed, so the
	// aggregate length is 3 and map B runs 3 items.
	if mapBItems != 3 {
		t.Errorf("Bucket 17 map_chains_to_map: map B (map[1]) item count = %d, want 3 (must equal A's aggregate length)", mapBItems)
	}

	// Each verify body committed at its own per-item path, proving B's body
	// actually consumed the aggregated findings (not an empty fan-out).
	completedPaths := map[string]bool{}
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted {
			completedPaths[e.Path] = true
		}
	}
	for _, want := range []string{"map[1].item-0.verify", "map[1].item-1.verify", "map[1].item-2.verify"} {
		if !completedPaths[want] {
			t.Errorf("Bucket 17 map_chains_to_map: missing map-B per-item commit %q; got %v", want, completedPaths)
		}
	}
}

func testMapChainResumeReplays(t *testing.T, factory BackendFactory) {
	t.Helper()
	// Round 1 runs to completion (both maps fully commit). Round 2 resumes
	// against a BARE fake (no programmed Exec). If A's aggregate replays
	// identically and map B does not re-execute A's body, NO Exec calls happen
	// on round 2 → the run completes ok. Any recomputation of A (or re-run of
	// B's body) would error against the bare fake and change the item/commit
	// counts, which the post-resume assertions catch.
	factory1 := preProgramFake(t, factory, aggregationPrograms())
	h := newHarnessWithInput(t, factory1, mapAggregationWorkflow,
		map[string]any{"items": []any{"a", "b", "c"}})
	oc1, err1 := h.runWorkflow(t)
	if err1 != nil {
		t.Fatalf("Bucket 17 map_chain_resume: round-1 err = %v", err1)
	}
	if oc1 != engine.OutcomeOK {
		t.Fatalf("Bucket 17 map_chain_resume: round-1 outcome = %q, want ok", oc1)
	}

	preEvents := mustFoldEvents(t, h)
	preA, preB := countMapItems(preEvents)
	if preA != 3 || preB != 3 {
		t.Fatalf("Bucket 17 map_chain_resume: round-1 item counts = (A=%d, B=%d), want (3, 3)", preA, preB)
	}

	// Round 2: BARE fake. Committed maps replay; nothing re-executes.
	h.factory = factory
	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Fatalf("Bucket 17 map_chain_resume: round-2 err = %v (resume must replay committed maps, not recompute A or re-run B)", err2)
	}
	if oc2 != engine.OutcomeOK {
		t.Errorf("Bucket 17 map_chain_resume: round-2 outcome = %q, want ok", oc2)
	}

	postEvents := mustFoldEvents(t, h)
	postA, postB := countMapItems(postEvents)
	if postA != 3 {
		t.Errorf("Bucket 17 map_chain_resume: post-resume map A item count = %d, want 3 (resume must NOT recompute A's aggregate)", postA)
	}
	if postB != 3 {
		t.Errorf("Bucket 17 map_chain_resume: post-resume map B item count = %d, want 3 (resume must NOT re-run B's body)", postB)
	}

	// Per-item commit-once: each (mapPath, N) appears exactly once across the
	// resume — replay must not duplicate commits.
	itemSeen := map[string]int{}
	for _, e := range postEvents {
		if e.Type != engine.EventMapItem {
			continue
		}
		var d engine.MapItemData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("unmarshal map.item: %v", err)
		}
		itemSeen[e.Path+"#"+strconv.Itoa(d.N)]++
	}
	for key, count := range itemSeen {
		if count != 1 {
			t.Errorf("Bucket 17 map_chain_resume: item %q committed %d times; want 1 (replay must not duplicate)", key, count)
		}
	}
}

// countMapItems folds the log and returns the number of committed map.item
// events for map A (map[0]) and map B (map[1]). map.item's Path is the map's
// static path (engine/map.go:264), so the two maps are distinguishable by Path.
func countMapItems(events []state.Event) (mapA, mapB int) {
	for _, e := range events {
		if e.Type != engine.EventMapItem {
			continue
		}
		switch e.Path {
		case "map[0]":
			mapA++
		case "map[1]":
			mapB++
		}
	}
	return mapA, mapB
}
