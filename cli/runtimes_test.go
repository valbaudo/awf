package cli

import (
	"reflect"
	"testing"

	"github.com/valbaudo/awf/ir"
)

func TestWalkAgentRefs_EmptyGraph(t *testing.T) {
	wf := &ir.Workflow{}
	got := walkAgentRefs(wf)
	if len(got) != 0 {
		t.Errorf("walkAgentRefs(empty) = %v, want empty", got)
	}
}

func TestWalkAgentRefs_CodeStepOnly(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "a", Container: "lab", Run: "echo"},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 0 {
		t.Errorf("walkAgentRefs(code-only) = %v, want empty (no `uses:` refs)", got)
	}
}

func TestWalkAgentRefs_TopLevelAgentStep(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "triage", Container: "lab", Uses: "anthropic/claude-code"},
		},
	}
	got := walkAgentRefs(wf)
	want := []agentRef{{Uses: "anthropic/claude-code", Container: "lab"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walkAgentRefs = %v, want %v", got, want)
	}
}

func TestWalkAgentRefs_Deduplicated(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "a", Container: "lab", Uses: "anthropic/claude-code"},
			&ir.AgentStep{ID: "b", Container: "lab", Uses: "anthropic/claude-code"},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 1 {
		t.Errorf("len(walkAgentRefs) = %d, want 1 (deduplicated)", len(got))
	}
}

func TestWalkAgentRefs_DistinctContainers(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "a", Container: "lab", Uses: "anthropic/claude-code"},
			&ir.AgentStep{ID: "b", Container: "scratch", Uses: "anthropic/claude-code"},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 2 {
		t.Fatalf("len(walkAgentRefs) = %d, want 2 (distinct containers)", len(got))
	}
	// Results MUST be sorted by (Uses, Container) for deterministic golden tests.
	want := []agentRef{
		{Uses: "anthropic/claude-code", Container: "lab"},
		{Uses: "anthropic/claude-code", Container: "scratch"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walkAgentRefs = %v, want %v (sorted)", got, want)
	}
}

func TestWalkAgentRefs_NestedInGate(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Gate{
				Generate: ir.NodeList{
					&ir.AgentStep{ID: "g", Container: "lab", Uses: "anthropic/claude-code"},
				},
				Evaluate: ir.NodeList{
					&ir.AgentStep{ID: "e", Container: "lab", Uses: "anthropic/claude-code"},
				},
				Until:       "evaluate.ok",
				MaxAttempts: 3,
			},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 1 {
		t.Errorf("len(walkAgentRefs nested-in-gate) = %d, want 1 (same ref + container, dedup)", len(got))
	}
}

func TestWalkAgentRefs_NestedInTryCatchFinally(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Try{
				Do:      ir.NodeList{&ir.AgentStep{ID: "d", Container: "lab", Uses: "x"}},
				Catch:   ir.NodeList{&ir.AgentStep{ID: "c", Container: "lab", Uses: "y"}},
				Finally: ir.NodeList{&ir.AgentStep{ID: "f", Container: "lab", Uses: "z"}},
			},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 3 {
		t.Errorf("len(walkAgentRefs try/catch/finally) = %d, want 3 (x/y/z)", len(got))
	}
}

func TestWalkAgentRefs_NestedInParallelOnly(t *testing.T) {
	// NOTE: ir.Parallel has a single `Children NodeList` field (flat list — the
	// standard's `{"parallel":[<node>,...]}` shape). Each child is conceptually
	// a branch. No nested [][]Node.
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Parallel{
				Children: ir.NodeList{
					&ir.AgentStep{ID: "p1", Container: "lab", Uses: "p"},
				},
			},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 1 {
		t.Errorf("len(walkAgentRefs parallel) = %d, want 1", len(got))
	}
}

// TestWalkAgentRefs_MapBodySkipped pins the design decision: AgentSteps
// inside Map bodies are intentionally NOT pinned at run-start. Per-item
// containers are dispatch-time; the IR container's image: digest already
// pins the claude binary version (Phase 1.4 validation). See walkAgentRefsNodes
// doc-comment for the safety rationale.
func TestWalkAgentRefs_MapBodySkipped(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Map{
				Over:        "input.items",
				As:          "item",
				Container:   "lab",
				Concurrency: 1,
				Body:        ir.NodeList{&ir.AgentStep{ID: "m1", Container: "lab", Uses: "m"}},
			},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 0 {
		t.Errorf("len(walkAgentRefs map-body-only) = %d, want 0 (Map body is dispatch-time-pinned via image digest, NOT run-start-pinned via Adapter.Version)", len(got))
	}
}

// TestWalkAgentRefs_TopLevelAndMapSibling — when a workflow has BOTH a top-level
// AgentStep AND a Map-body AgentStep, only the top-level one is pinned.
func TestWalkAgentRefs_TopLevelAndMapSibling(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "top", Container: "lab", Uses: "anthropic/claude-code"},
			&ir.Map{
				Over:      "input.items",
				As:        "item",
				Container: "lab",
				Body:      ir.NodeList{&ir.AgentStep{ID: "inside", Container: "lab", Uses: "should-be-skipped"}},
			},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (top-level only)", len(got))
	}
	if got[0].Uses != "anthropic/claude-code" {
		t.Errorf("Uses = %q, want %q (Map-body ref leaked)", got[0].Uses, "anthropic/claude-code")
	}
}

// NOTE: There is intentionally no TestWalkAgentRefs_UnknownNodePanics test.
// ir.Node is interface{ isNode() } (see ir/node.go:17) — a closed sum type
// with an UNEXPORTED marker method. No type outside the ir package can
// satisfy ir.Node, so walkAgentRefsNodes's default arm is unreachable from
// cli/_test. The panic is defensive documentation only; if a future ir/
// node type lands without updating this switch, integration tests
// exercising the new node panic loudly. See walkAgentRefsNodes doc-comment.
