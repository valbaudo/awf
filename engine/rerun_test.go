package engine

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
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
	want := []string{"parallel[1].branchA", "s2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("set = %v, want %v", got, want)
	}
}

func TestComputeRerunInvalidation_TopLevelStep(t *testing.T) {
	wf := rerunTestWF()
	rs := rsWithCompleted("s0", "parallel[1].branchA", "parallel[1].branchB", "s2")
	got, _ := ComputeRerunInvalidation(wf, rs, "s0")
	want := []string{"parallel[1].branchA", "parallel[1].branchB", "s0", "s2"}
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

func TestComputeRerunInvalidation_RefusesStructuralDrift(t *testing.T) {
	wf := rerunTestWF() // graph has s0, parallel[1], s2 — NOT "gone"
	rs := rsWithCompleted("s0", "gone", "s2")
	if _, err := ComputeRerunInvalidation(wf, rs, "s0"); err == nil {
		t.Fatal("expected refusal: committed node \"gone\" has no top-level node in the current graph")
	}
}

func TestComputeRerunInvalidation_MapWholly(t *testing.T) {
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{
		&ir.CodeStep{ID: "s0", Run: "x", Container: "c"},
		&ir.Map{Over: ir.Expr("{{ input.xs }}"), As: "x", Container: "c",
			Body: ir.NodeList{&ir.CodeStep{ID: "scan", Run: "x", Container: "c"}}},
		&ir.CodeStep{ID: "s2", Run: "x", Container: "c"},
	}}
	rs := rsWithCompleted("s0", "map[1].item-0.scan", "map[1].item-1.scan", "s2")
	rs.MapItems["map[1]"] = []MapItemRecord{{N: 0, Status: ItemPassed}, {N: 1, Status: ItemPassed}}
	got, err := ComputeRerunInvalidation(wf, rs, "s0")
	if err != nil {
		t.Fatalf("ComputeRerunInvalidation: %v", err)
	}
	for _, p := range []string{"map[1].item-0.scan", "map[1].item-1.scan", "map[1]"} {
		if !sliceContains(got, p) {
			t.Fatalf("map path %q must be in the whole-map invalidation set; got %v", p, got)
		}
	}
}

func sliceContains(xs []string, x string) bool {
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
	if got, err := ResolveRerunTarget(wf, rs, nil, "parallel[1].branchA"); err != nil || got != "parallel[1].branchA" {
		t.Fatalf("exact: (%q,%v)", got, err)
	}
	if got, err := ResolveRerunTarget(wf, rs, nil, "parallel[1]"); err != nil || got != "parallel[1]" {
		t.Fatalf("container: (%q,%v)", got, err)
	}
	if got, err := ResolveRerunTarget(wf, rs, nil, "branchA"); err != nil || got != "parallel[1].branchA" {
		t.Fatalf("bare-id: (%q,%v)", got, err)
	}
	if _, err := ResolveRerunTarget(wf, rs, nil, "nope"); err == nil {
		t.Fatal("expected error for absent arg")
	}
}

// TestResolveRerunTarget_FailedFrontier verifies that --from can target the
// trailing node.failed event (uncommitted frontier node). Exact-path and
// bare-id forms must both resolve.
func TestResolveRerunTarget_FailedFrontier(t *testing.T) {
	wf := rerunTestWF()
	// only "a" is committed; "b" failed (uncommitted frontier)
	rs := rsWithCompleted("s0")
	events := []state.Event{
		{Type: EventNodeFailed, Path: "parallel[1].branchB"},
	}
	// exact full path
	if got, err := ResolveRerunTarget(wf, rs, events, "parallel[1].branchB"); err != nil || got != "parallel[1].branchB" {
		t.Fatalf("exact-path failed frontier: (%q,%v)", got, err)
	}
	// bare trailing id
	if got, err := ResolveRerunTarget(wf, rs, events, "branchB"); err != nil || got != "parallel[1].branchB" {
		t.Fatalf("bare-id failed frontier: (%q,%v)", got, err)
	}
}

// clearInvalidatedPaths must clear a path from ALL nine path-keyed indices (a
// missed one would leak a stale gate verdict / map item / etc. on re-run), and
// must NOT touch the name/container-keyed maps (Signals, SnapshotRefs).
func TestClearInvalidatedPaths_AllNineIndices(t *testing.T) {
	rs := NewRunState("r", "d", nil)
	rs.Completed["p"] = NodeResult{Outcome: OutcomeOK}
	rs.Branches["p"] = "then"
	rs.LoopIters["p"] = 1
	rs.GateAttempts["p"] = nil
	rs.ReactRounds["p"] = nil
	rs.MapItems["p"] = nil
	rs.CallStarted["p"] = CallStartedRecord{}
	rs.SignalReceivedAt["p"] = SignalReceivedEntry{}
	rs.SelectedSkills["p"] = SkillsSelectedData{}
	rs.SnapshotRefs["c"] = "snap"         // container-NAME-keyed; a path sweep must NOT clear it
	rs.Signals["sig"] = []SignalEntry{{}} // signal-NAME-keyed; a path sweep must NOT clear it

	clearInvalidatedPaths(rs, []string{"p"})

	for name, present := range map[string]bool{
		"Completed":        mapHas(rs.Completed, "p"),
		"Branches":         mapHas(rs.Branches, "p"),
		"LoopIters":        mapHas(rs.LoopIters, "p"),
		"GateAttempts":     mapHas(rs.GateAttempts, "p"),
		"ReactRounds":      mapHas(rs.ReactRounds, "p"),
		"MapItems":         mapHas(rs.MapItems, "p"),
		"CallStarted":      mapHas(rs.CallStarted, "p"),
		"SignalReceivedAt": mapHas(rs.SignalReceivedAt, "p"),
		"SelectedSkills":   mapHas(rs.SelectedSkills, "p"),
	} {
		if present {
			t.Errorf("index %s still has %q after clearInvalidatedPaths", name, "p")
		}
	}
	if !mapHas(rs.SnapshotRefs, "c") {
		t.Error("SnapshotRefs is container-name-keyed and must survive a path-clear")
	}
	if !mapHas(rs.Signals, "sig") {
		t.Error("Signals is signal-name-keyed and must survive a path-clear")
	}
}

func mapHas[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}
