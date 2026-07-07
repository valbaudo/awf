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
		Containers:  map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		InputSchema: &JSONSchema{"type": "object", "properties": map[string]any{"xs": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}},
		Graph: NodeList{
			&Map{Over: Expr("{{ input.xs }}"), As: "x", Container: "c", Concurrency: intPtr(2),
				Body: NodeList{&AgentStep{ID: "per_item", Container: "c", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}}}},
			&AgentStep{ID: "after", Uses: "awf/llm", Continues: "per_item", With: RawConfig{"model": "m", "prompt": "p"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1027", "after")
}

// --- AWF1032: concurrent parallel-sibling reject (race fix) ---

// Two distinct direct children of the SAME parallel[N] run concurrently
// (engine/parallel.go fans them out as goroutines), so a `continues:` from one
// sibling to the other is NOT guaranteed to have run before this turn assembles
// its thread — at run time LookupCompleted(target) nondeterministically finds the
// target committed or not (race → nondeterministic permanent_failure). The
// dominator rule (AWF1027) is fooled because a parallel child has a bare path with
// no branch label, so A's scope prefix IS a boundary prefix of B's path and A
// precedes B in document order. AWF1032 closes that hole.
func TestContinuesParallelSiblingRejected(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&Parallel{Children: NodeList{
				&AgentStep{ID: "branch_a", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
				&AgentStep{ID: "branch_b", Uses: "awf/llm", Continues: "branch_a", With: RawConfig{"model": "m", "prompt": "p"}},
			}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1032", "parallel[0].branch_b")
}

// T10 fan-out (must stay VALID): two parallel branches continuing a common
// ancestor OUTSIDE the parallel. `seed` precedes the parallel and is not a
// concurrent sibling of either branch, so AWF1032 must NOT fire. (Mirror of the
// existing TestContinuesFanOutCommonAncestorClean, asserting the new code too.)
func TestContinuesParallelFanOutFromOutsideClean(t *testing.T) {
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
	assertNoCode(t, Validate(ld), "AWF1032")
	assertNoCode(t, Validate(ld), "AWF1027")
}

// Sequential within the SAME parallel branch (must stay VALID): a single parallel
// child is itself a control node (here a loop) holding a 2-step sub-sequence. The
// two steps share the same child sub-path (loop[0]) immediately under parallel[0]
// — they do NOT diverge at the parallel boundary, so they are sequential within
// one branch, not concurrent siblings. AWF1032 must NOT fire. (A bare 2-step
// sub-sequence cannot be a single parallel child: each array element is exactly
// one node, so a control node is the only way to nest a sequence in one branch.)
func TestContinuesWithinParallelBranchClean(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&Parallel{Children: NodeList{
				&Loop{
					MaxIters: intPtr(3),
					Body: NodeList{
						&AgentStep{ID: "t1", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}},
						&AgentStep{ID: "t2", Uses: "awf/llm", Continues: "t1", With: RawConfig{"model": "m", "prompt": "p"}},
					},
				},
			}},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1032")
	assertNoCode(t, Validate(ld), "AWF1027")
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

// --- AWF1030: evaluate-block context evidence guard (A.3.5) ---

// The evaluator stays fresh, but may receive prior non-evaluator source context
// as context evidence.
func TestContinuesInEvaluateAllowsSourceContext(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Agents: map[string]AgentRole{
			"writer": {Uses: "awf/llm"},
			"judge":  {Uses: "awf/llm"},
		},
		Graph: NodeList{
			&AgentStep{ID: "draft", Uses: "writer", With: RawConfig{"model": "m", "prompt": "p"}},
			&AgentStep{ID: "critique", Uses: "writer", Continues: "draft", With: RawConfig{"model": "m", "prompt": "p"}},
			&Gate{
				Generate: NodeList{&AgentStep{ID: "gen", Uses: "writer", With: RawConfig{"model": "m", "prompt": "p"}}},
				Evaluate: NodeList{
					&AgentStep{ID: "judge", Uses: "judge", Continues: "critique",
						With:         RawConfig{"model": "m", "prompt": "p"},
						OutputSchema: &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}},
				},
				Until:       Expr("{{ step.judge.ok }}"),
				MaxAttempts: 3,
			},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1030")
	assertNoCode(t, Validate(ld), "AWF1029")
}

func TestContinuesInEvaluateRejectsDirectEvaluatorTarget(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Graph: NodeList{
			&Gate{
				Generate: NodeList{&AgentStep{ID: "gen", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "p"}}},
				Evaluate: NodeList{
					&AgentStep{ID: "judge_a", Uses: "awf/llm",
						With:         RawConfig{"model": "m", "prompt": "p"},
						OutputSchema: &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}},
					&AgentStep{ID: "judge_b", Uses: "awf/llm", Continues: "judge_a",
						With:         RawConfig{"model": "m", "prompt": "p"},
						OutputSchema: &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}},
				},
				Until:       Expr("{{ step.judge_b.ok }}"),
				MaxAttempts: 3,
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1030", "gate[0].evaluate.judge_b")
}

func TestContinuesInEvaluateDifferentResolvedBaseAdapterRejected(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Agents: map[string]AgentRole{
			"writer": {Uses: "awf/llm"},
			"judge":  {Uses: "anthropic/claude-code"},
		},
		Graph: NodeList{
			&AgentStep{ID: "draft", Uses: "writer", With: RawConfig{"model": "m", "prompt": "p"}},
			&Gate{
				Generate: NodeList{&AgentStep{ID: "gen", Uses: "writer", With: RawConfig{"model": "m", "prompt": "p"}}},
				Evaluate: NodeList{
					&AgentStep{ID: "judge", Uses: "judge", Continues: "draft",
						With:         RawConfig{"model": "m", "prompt": "p"},
						OutputSchema: &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}},
				},
				Until:       Expr("{{ step.judge.ok }}"),
				MaxAttempts: 3,
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1029", "gate[1].evaluate.judge")
}

func TestContinuesChainTouchesEvaluate(t *testing.T) {
	agents := map[string]*AgentStep{
		"draft":   {ID: "draft"},
		"judge":   {ID: "judge"},
		"relay":   {ID: "relay", Continues: "judge"},
		"outside": {ID: "outside", Continues: "draft"},
	}
	paths := map[string]string{
		"draft":   "draft",
		"judge":   "gate[0].evaluate.judge",
		"relay":   "relay",
		"outside": "outside",
	}
	if !continuesChainTouchesEvaluate("relay", agents, paths) {
		t.Fatal("relay chain should touch gate.evaluate through judge")
	}
	if continuesChainTouchesEvaluate("outside", agents, paths) {
		t.Fatal("outside chain should not touch gate.evaluate")
	}
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

// --- Cross-check: validator (dominates) vs resolver (engine/scope.go) ---

// TestDominatesResolverCrossCheck pins the validator side (ir.dominates) against
// the KNOWN accept/reject rules of engine/scope.go's stepRuntimePath, so future
// drift between the two is caught here. We cannot call engine's unexported
// stepRuntimePath from package ir, so this is a documented table: each row maps a
// representative (target static path, source static path) pair to the dominance
// verdict dominates() must return, annotated with the corresponding resolver rule.
//
// dominates() implements only the SCOPE-PREFIX + document-order half of
// addressability. The loop-DEPTH rule (stepRuntimePath rejects loopCount>1) is a
// separate gate (AWF1031) layered AFTER dominates in validateContinues, so rows
// that exercise it are NOT asserted here (they would pass dominates but be
// rejected downstream — see TestContinuesNestedLoopTargetRejected).
func TestDominatesResolverCrossCheck(t *testing.T) {
	cases := []struct {
		name             string
		tgtPath, srcPath string
		tgtOrd, srcOrd   int
		want             bool
		resolverRule     string // the stepRuntimePath behavior this mirrors
	}{
		{
			name: "top-level sequential", tgtPath: "first", srcPath: "second",
			tgtOrd: 0, srcOrd: 1, want: true,
			resolverRule: "no multiplicity boundary — resolves from anywhere",
		},
		{
			name: "forward ref rejected", tgtPath: "second", srcPath: "first",
			tgtOrd: 1, srcOrd: 0, want: false,
			resolverRule: "target not yet committed; document order guards it",
		},
		{
			name: "self ref rejected", tgtPath: "x", srcPath: "x",
			tgtOrd: 0, srcOrd: 0, want: false,
			resolverRule: "a step does not enclose itself (ordinal equal → false)",
		},
		{
			name: "gate-internal from outside rejected", tgtPath: "gate[1].generate.inner", srcPath: "after",
			tgtOrd: 1, srcOrd: 5, want: false,
			resolverRule: "stepRuntimePath: gate[N]→attempt-M unmatched from outside → reject",
		},
		{
			name: "same gate.generate scope", tgtPath: "gate[0].generate.ask", srcPath: "gate[0].generate.refine",
			tgtOrd: 1, srcOrd: 2, want: true,
			resolverRule: "stepRuntimePath: same attempt-M instance → resolves",
		},
		{
			name: "cross-item (map body from outside) rejected", tgtPath: "map[0].body.per_item", srcPath: "after",
			tgtOrd: 1, srcOrd: 4, want: false,
			resolverRule: "stepRuntimePath: map[N].body→item-K unmatched from outside → reject",
		},
		{
			name: "if sibling-branch rejected", tgtPath: "if[1].then.rethink", srcPath: "if[1].else.revise",
			tgtOrd: 2, srcOrd: 4, want: false,
			resolverRule: ".then vs .else diverge — sibling branch never co-runs",
		},
		{
			name: "nested-loop target passes dominates (AWF1031 rejects downstream)", tgtPath: "loop[0].body.loop[0].body.inner_draft", srcPath: "loop[0].body.loop[0].body.inner_revise",
			tgtOrd: 2, srcOrd: 3, want: true,
			resolverRule: "scope-prefix OK; loopCount>1 is the SEPARATE AWF1031 gate, not dominates",
		},
		{
			name: "parallel sibling passes dominates (AWF1032 rejects downstream)", tgtPath: "parallel[0].branch_a", srcPath: "parallel[0].branch_b",
			tgtOrd: 1, srcOrd: 2, want: true,
			resolverRule: "bare parallel[N] child paths — scope-prefix OK; concurrency is the SEPARATE AWF1032 gate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dominates(tc.tgtPath, tc.srcPath, tc.tgtOrd, tc.srcOrd)
			if got != tc.want {
				t.Errorf("dominates(%q, %q, %d, %d) = %v, want %v\n  resolver rule: %s",
					tc.tgtPath, tc.srcPath, tc.tgtOrd, tc.srcOrd, got, tc.want, tc.resolverRule)
			}
		})
	}
}

// TestParallelSiblingsCrossCheck pins the concurrent-parallel-sibling detector
// (parallelSiblings) directly — the predicate AWF1032 keys on. It mirrors the
// engine/parallel.go fact that DISTINCT direct children of one parallel[N] run as
// concurrent goroutines, while a target reachable through the SAME child sub-path
// (or outside the parallel entirely) does not.
func TestParallelSiblingsCrossCheck(t *testing.T) {
	cases := []struct {
		name             string
		tgtPath, srcPath string
		want             bool
	}{
		{"distinct children of same parallel", "parallel[0].branch_a", "parallel[0].branch_b", true},
		{"distinct children, nested under outer", "loop[0].body.parallel[1].a", "loop[0].body.parallel[1].b", true},
		{"same child sub-path (sequential in one branch)", "parallel[0].loop[0].body.t1", "parallel[0].loop[0].body.t2", false},
		{"target outside parallel (fan-out ancestor)", "seed", "parallel[1].branch_a", false},
		{"no parallel anywhere", "first", "second", false},
		{"same parallel child step (identical)", "parallel[0].a", "parallel[0].a", false},
		{"inner-parallel sibling under shared outer-parallel child", "parallel[0].x.parallel[0].a", "parallel[0].x.parallel[0].b", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parallelSiblings(tc.tgtPath, tc.srcPath); got != tc.want {
				t.Errorf("parallelSiblings(%q, %q) = %v, want %v", tc.tgtPath, tc.srcPath, got, tc.want)
			}
		})
	}
}
