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
	if got, err := ResolveRerunTarget(wf, rs, "parallel[1].branchA"); err != nil || got != "parallel[1].branchA" {
		t.Fatalf("exact: (%q,%v)", got, err)
	}
	if got, err := ResolveRerunTarget(wf, rs, "parallel[1]"); err != nil || got != "parallel[1]" {
		t.Fatalf("container: (%q,%v)", got, err)
	}
	if got, err := ResolveRerunTarget(wf, rs, "branchA"); err != nil || got != "parallel[1].branchA" {
		t.Fatalf("bare-id: (%q,%v)", got, err)
	}
	if _, err := ResolveRerunTarget(wf, rs, "nope"); err == nil {
		t.Fatal("expected error for absent arg")
	}
}
