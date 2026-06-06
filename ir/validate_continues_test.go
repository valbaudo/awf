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

// --- AWF1028: acyclic continues chain (A.3.3) ---

// A.3.3 — 2-cycle: a→b→a. The dominator rule (AWF1027) only rejects the backward
// edge (a continues: b, where b is after a in document order). The forward edge
// (b continues: a, where a is before b) passes AWF1027 — so the cycle guard must
// fire independently via the dedicated chain walk. AWF1028 is emitted once per
// detected cycle (at the entry node, i.e. the first continuing step in document order
// whose chain loops back to itself).
func TestContinuesCycleRejected(t *testing.T) {
	// a → b → a. In document order [a, b]:
	// • "a continues: b" trips AWF1027 (b is after a), so a returns early — the cycle
	//   walk catches it from b's perspective instead.
	// AWF1028 must fire exactly once.
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "a", Uses: "awf/llm", Continues: "b", With: RawConfig{"model": "m", "prompt": "p"}},
			&AgentStep{ID: "b", Uses: "awf/llm", Continues: "a", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertOneError(t, Validate(ld), "AWF1028")
}

// A clean linear chain produces no AWF1028.
func TestContinuesLinearChainNoCycle(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "first", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
			&AgentStep{ID: "second", Uses: "awf/llm", Continues: "first", With: RawConfig{"model": "m", "prompt": "p"}},
			&AgentStep{ID: "third", Uses: "awf/llm", Continues: "second", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1028")
}

// --- AWF1029: same-uses rule (A.3.4) ---

// A continuing step whose uses differs from its target's uses must be rejected.
func TestContinuesDifferentUsesRejected(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "first", Uses: "anthropic/claude-code", With: RawConfig{"prompt": "p"}},
			&AgentStep{ID: "second", Uses: "awf/llm", Continues: "first", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1029", "second")
}

// A continuing step with the same uses as its target must be clean.
func TestContinuesSameUsesClean(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "first", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
			&AgentStep{ID: "second", Uses: "awf/llm", Continues: "first", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1029")
}

// A three-step chain where all uses match must be clean.
func TestContinuesChainSameUsesClean(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "first", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
			&AgentStep{ID: "second", Uses: "awf/llm", Continues: "first", With: RawConfig{"model": "m", "prompt": "p"}},
			&AgentStep{ID: "third", Uses: "awf/llm", Continues: "second", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1029")
}

// --- AWF1030: evaluate-block prohibition (A.3.5) ---

// A step inside a gate's evaluate: block that declares continues: is AWF1030.
// The evaluator must judge in a fresh context; a conversation thread would couple
// the evaluator to the history of the generate side, violating gate independence.
func TestContinuesInEvaluateRejected(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&AgentStep{ID: "draft", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
			&Gate{
				Generate: NodeList{&AgentStep{ID: "gen", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}}},
				Evaluate: NodeList{
					&AgentStep{ID: "judge", Uses: "awf/llm", Continues: "draft", // forbidden in evaluate
						With:         RawConfig{"model": "m", "prompt": "p"},
						OutputSchema: &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}},
				},
				Until:       Expr("{{ step.judge.ok }}"),
				MaxAttempts: 3,
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1030", "gate[1].evaluate.judge")
}

// A step inside a gate's generate: block is allowed to use continues:
// (generate is not a fresh-context scope; only evaluate: is forbidden).
func TestContinuesInGenerateAllowed(t *testing.T) {
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
	assertNoCode(t, Validate(ld), "AWF1030")
}

// --- AWF1031: single-loop-scope rule (A.3.6) ---

// A target inside TWO nested loops is unaddressable at runtime (engine/scope.go
// stepRuntimePath rejects loopCount > 1). Both target and source are inside the SAME
// inner-loop body, so the dominator rule (AWF1027) passes — proving AWF1031 closes a
// gap that AWF1027 alone leaves open.
func TestContinuesNestedLoopTargetRejected(t *testing.T) {
	// Doubly-nested loop: outer loop body → inner loop body → [inner_draft, inner_revise].
	// inner_revise continues: inner_draft. The target "inner_draft" is at static path
	// "loop[0].body.loop[0].body.inner_draft" (two loop[ segments) → AWF1031.
	// AWF1027 passes because inner_draft precedes inner_revise inside the same scope.
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&Loop{
				MaxIters: intPtr(3),
				Body: NodeList{
					&Loop{
						MaxIters: intPtr(3),
						Body: NodeList{
							&AgentStep{ID: "inner_draft", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
							&AgentStep{ID: "inner_revise", Uses: "awf/llm", Continues: "inner_draft", With: RawConfig{"model": "m", "prompt": "p"}},
						},
					},
				},
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1031", "loop[0].body.loop[0].body.inner_revise")
}

// A target inside ONE loop body is addressable → no AWF1031.
// intra-iteration link: both steps live in the same single-loop body.
func TestContinuesSingleLoopTargetClean(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&Loop{
				MaxIters: intPtr(3),
				Body: NodeList{
					&AgentStep{ID: "loop_draft", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
					&AgentStep{ID: "loop_revise", Uses: "awf/llm", Continues: "loop_draft", With: RawConfig{"model": "m", "prompt": "p"}},
				},
			},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1031")
}
