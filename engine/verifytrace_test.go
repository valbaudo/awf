package engine

import (
	"testing"

	"github.com/valbaudo/awf/ir"
)

// helper: build a RunState with two completed CodeStep paths ("a", "b"),
// both with the NodeSubtreeDigest matching the provided workflow nodes.
func makeVerifyingTraceRS(t *testing.T, wf *ir.Workflow, completedPaths []string) *RunState {
	t.Helper()
	rs := &RunState{
		Completed: make(map[string]NodeResult),
	}
	static := make(map[string]ir.Node)
	ir.WalkNodes(wf.Graph, "", func(n ir.Node, path string) {
		static[path] = n
	})
	for _, p := range completedPaths {
		n, ok := static[p]
		if !ok {
			t.Fatalf("makeVerifyingTraceRS: node %q not in workflow", p)
		}
		d, err := ir.NodeSubtreeDigest(n)
		if err != nil {
			t.Fatalf("NodeSubtreeDigest(%q): %v", p, err)
		}
		rs.Completed[p] = NodeResult{
			Outcome:           OutcomeOK,
			NodeSubtreeDigest: d,
		}
	}
	return rs
}

// abWorkflow builds a two-code-step sequential workflow (a → b).
func abWorkflow(runA, runB string) *ir.Workflow {
	return &ir.Workflow{
		ID:      "ab-test",
		Version: 1,
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "a", Container: "lab", Run: runA},
			&ir.CodeStep{ID: "b", Container: "lab", Run: runB},
		},
	}
}

// TestComputeVerifyingTraceTarget_AllReusable: both steps unchanged → returns "".
func TestComputeVerifyingTraceTarget_AllReusable(t *testing.T) {
	t.Parallel()
	wf := abWorkflow("./a.sh", "./b.sh")
	rs := makeVerifyingTraceRS(t, wf, []string{"a", "b"})

	target, err := ComputeVerifyingTraceTarget(wf, rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != "" {
		t.Fatalf("target = %q, want %q (all reusable)", target, "")
	}
}

// TestComputeVerifyingTraceTarget_EditDownstream: step b changed → returns "b" (not "a").
func TestComputeVerifyingTraceTarget_EditDownstream(t *testing.T) {
	t.Parallel()
	// Run 1 workflow: both a and b with their original run bodies.
	origWf := abWorkflow("./a.sh", "./b.sh")
	rs := makeVerifyingTraceRS(t, origWf, []string{"a", "b"})

	// Edit only b's run body.
	newWf := abWorkflow("./a.sh", "./b-CHANGED.sh")

	target, err := ComputeVerifyingTraceTarget(newWf, rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != "b" {
		t.Fatalf("target = %q, want %q (b changed, a reusable)", target, "b")
	}
}

// TestComputeVerifyingTraceTarget_EditUpstream: step a changed → returns "a".
func TestComputeVerifyingTraceTarget_EditUpstream(t *testing.T) {
	t.Parallel()
	origWf := abWorkflow("./a.sh", "./b.sh")
	rs := makeVerifyingTraceRS(t, origWf, []string{"a", "b"})

	// Edit a's run body.
	newWf := abWorkflow("./a-CHANGED.sh", "./b.sh")

	target, err := ComputeVerifyingTraceTarget(newWf, rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != "a" {
		t.Fatalf("target = %q, want %q (a changed, earliest slot)", target, "a")
	}
}

// TestComputeVerifyingTraceTarget_AgentNotReusable: agent step → always non-reusable
// → returns first agent path's root seg.
func TestComputeVerifyingTraceTarget_AgentNotReusable(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{
		ID:      "agent-test",
		Version: 1,
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "triage", Uses: "awf/llm"},
			&ir.CodeStep{ID: "report", Container: "lab", Run: "./report.sh"},
		},
	}
	// Both steps "committed": agent step has no NodeSubtreeDigest (agents never get one)
	// but we simulate a committed record (NodeSubtreeDigest="" as per commit.go behavior).
	rs := &RunState{
		Completed: map[string]NodeResult{
			"triage": {Outcome: OutcomeOK, NodeSubtreeDigest: ""},
			"report": func() NodeResult {
				d, _ := ir.NodeSubtreeDigest(&ir.CodeStep{ID: "report", Container: "lab", Run: "./report.sh"})
				return NodeResult{Outcome: OutcomeOK, NodeSubtreeDigest: d}
			}(),
		},
	}

	target, err := ComputeVerifyingTraceTarget(wf, rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// "triage" is at slot 0 (first), is non-reusable (AgentStep), so target = "triage".
	if target != "triage" {
		t.Fatalf("target = %q, want %q (agent always non-reusable)", target, "triage")
	}
}

// TestComputeVerifyingTraceTarget_AddressingShift: committed path "gone" not in
// current graph → error.
func TestComputeVerifyingTraceTarget_AddressingShift(t *testing.T) {
	t.Parallel()
	// Current workflow only has "a".
	currentWf := &ir.Workflow{
		ID:      "shift-test",
		Version: 1,
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "a", Container: "lab", Run: "./a.sh"},
		},
	}
	// Run state has committed path "gone" which no longer exists.
	rs := &RunState{
		Completed: map[string]NodeResult{
			"gone": {Outcome: OutcomeOK, NodeSubtreeDigest: "awf-d1:sha256:abc"},
		},
	}

	_, err := ComputeVerifyingTraceTarget(currentWf, rs)
	if err == nil {
		t.Fatal("expected error for addressing shift, got nil")
	}
}
