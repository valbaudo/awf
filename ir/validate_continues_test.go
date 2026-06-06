package ir

import "testing"

// Tests for the AWF1026-AWF1031 continues: pass — see validate_continues.go.
// NOTE: AWF1025 is reserved for the map-image-target-static-pin rule; continues:
// validation starts at AWF1026 (next free code after the existing AWF1025).

func TestContinuesTargetMustExist(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "critique", Uses: "awf/llm", Continues: "nope", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1026", "critique")
}

func TestContinuesTargetMustBeAgentStep(t *testing.T) {
	// A continues: pointing at a CodeStep (or any non-agent node) is AWF1026.
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "build", Container: "c", Run: "true"},
			&AgentStep{ID: "critique", Uses: "awf/llm", Continues: "build", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1026", "critique")
}

func TestContinuesEmptyIsClean(t *testing.T) {
	// No continues: → no AWF1026-1031 of any kind.
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "draft", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	for _, code := range []string{"AWF1026", "AWF1027", "AWF1028", "AWF1029", "AWF1030", "AWF1031"} {
		assertNoCode(t, Validate(ld), code)
	}
}

// --- AWF1027: dominator (A.3.2, incl. multiplicity) ---
// NOTE: the plan calls this rule AWF1026, but AWF1025 (map-image-static-pin) and
// AWF1026 (continues-target-exists, just landed) are already taken — so the dominator
// rule is AWF1027 (the next free code).

// A valid linear chain dominates: each turn precedes the next at top level.
func TestContinuesLinearChainClean(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "first", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
			&AgentStep{ID: "second", Uses: "awf/llm", Continues: "first", With: RawConfig{"model": "m", "prompt": "p"}},
			&AgentStep{ID: "third", Uses: "awf/llm", Continues: "second", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1027")
}

// A.3.2 dominator: later / forward refs are rejected.
func TestContinuesForwardRefRejected(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "critique", Uses: "awf/llm", Continues: "revise", With: RawConfig{"model": "m", "prompt": "p"}},
			&AgentStep{ID: "revise", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1027", "critique")
}

func TestContinuesSelfRefRejected(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "loopy", Uses: "awf/llm", Continues: "loopy", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1027", "loopy")
}

// T9 (validation half): a continues: into the SIBLING if-branch is rejected (the
// sibling does not dominate); both forks continuing the common ancestor is CLEAN.
func TestContinuesSiblingBranchRejected(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "draft", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
			&If{Cond: Expr("{{ step.draft.exit_code == 0 }}"),
				Then: NodeList{&AgentStep{ID: "rethink", Uses: "awf/llm", Continues: "draft", With: RawConfig{"model": "m", "prompt": "p"}}},
				Else: NodeList{&AgentStep{ID: "revise", Uses: "awf/llm", Continues: "rethink", With: RawConfig{"model": "m", "prompt": "p"}}}, // sibling branch — not dominating
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1027", "if[1].else.revise")
}

func TestContinuesCommonAncestorFromBothForksClean(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "draft", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
			&If{Cond: Expr("{{ step.draft.exit_code == 0 }}"),
				Then: NodeList{&AgentStep{ID: "rethink", Uses: "awf/llm", Continues: "draft", With: RawConfig{"model": "m", "prompt": "p"}}},
				Else: NodeList{&AgentStep{ID: "revise", Uses: "awf/llm", Continues: "draft", With: RawConfig{"model": "m", "prompt": "p"}}},
			},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1027")
}

// intra-scope is admitted: an earlier turn in the SAME gate.generate dominates a later one.
func TestContinuesSameGenerateScopeClean(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Gate{
				Generate: NodeList{
					&AgentStep{ID: "ask", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
					&AgentStep{ID: "refine", Uses: "awf/llm", Continues: "ask", With: RawConfig{"model": "m", "prompt": "p"}},
				},
				Evaluate:    NodeList{&CodeStep{ID: "j", Container: "c", Run: "true", OutputSchema: &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}}},
				Until:       Expr("{{ step.j.ok }}"),
				MaxAttempts: 3,
			},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1027")
}

// A valid fan-out: two parallel branches continue a common pre-fork ancestor — clean.
func TestContinuesFanOutCommonAncestorClean(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "seed", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
			&Parallel{Children: NodeList{
				&AgentStep{ID: "branch_a", Uses: "awf/llm", Continues: "seed", With: RawConfig{"model": "m", "prompt": "p"}},
				&AgentStep{ID: "branch_b", Uses: "awf/llm", Continues: "seed", With: RawConfig{"model": "m", "prompt": "p"}},
			}},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1027")
}

// continuing FROM a gate-internal turn (gate encloses T, not the outside S) → AWF1027.
func TestContinuesGateInternalFromOutsideRejected(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Gate{
				Generate:    NodeList{&AgentStep{ID: "inner", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}}},
				Evaluate:    NodeList{&CodeStep{ID: "j", Container: "c", Run: "true", OutputSchema: &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}}},
				Until:       Expr("{{ step.j.ok }}"),
				MaxAttempts: 3,
			},
			&AgentStep{ID: "after", Uses: "awf/llm", Continues: "inner", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1027", "after")
}

// T11 — cross-item: a map item continuing a step inside a map body from OUTSIDE the
// map (map[0].body encloses T but not S) → AWF1027.
func TestContinuesMapItemFromOutsideRejected(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Input:      &JSONSchema{"type": "object", "properties": map[string]any{"xs": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}},
		Graph: NodeList{
			&Map{Over: Expr("{{ input.xs }}"), As: "x", Container: "c", Concurrency: 2,
				Body: NodeList{&AgentStep{ID: "per_item", Container: "c", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}}}},
			&AgentStep{ID: "after", Uses: "awf/llm", Continues: "per_item", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1027", "after")
}
