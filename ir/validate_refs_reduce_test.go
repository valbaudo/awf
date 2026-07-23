package ir

import "testing"

// Tests for the reduce-aware half of the downstream-ref validator (SP2 Task 11,
// validator half). A reference INTO a map that declares a reduce: resolves against
// the REDUCER's output, not the per-item aggregate — so AWF5004 must NOT fire, and
// the field must instead be cross-checked against the reducer's output shape
// (engine/scope.go aggregateMapOutputs short-circuits to LookupCompleted(mapStatic)
// at runtime; the validator must accept EXACTLY what that resolves).

// reduceRunSchema is a run: reducer's output_schema declaring a `summary` field.
func reduceRunSchema() *JSONSchema {
	return &JSONSchema{
		"type": "object", "additionalProperties": false,
		"required":   []any{"summary"},
		"properties": map[string]any{"summary": map[string]any{"type": "string"}},
	}
}

// TestReducedMapRunReducerFieldRefAccepted: a downstream `{{ step.scan.summary }}`
// ref to a run:-reducer-declared field VALIDATES with no AWF5004 (the ref resolves
// against the reducer's output_schema, which declares `summary`).
func TestReducedMapRunReducerFieldRefAccepted(t *testing.T) {
	ld := makeLD(&Workflow{ID: "agg", Version: 1,
		Containers: aggContainer(),
		Graph: NodeList{
			aggFindURLs(),
			&Map{Over: Expr("{{ step.find_urls.urls }}"), As: "u", Container: "c", Concurrency: intPtr(1),
				Body: NodeList{
					&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code",
						With: RawConfig{"prompt": "Scan {{ u }}"}, OutputSchema: aggScanSchema()},
				},
				Reduce: &Reduce{Run: "true", Container: "c", OutputSchema: reduceRunSchema()},
			},
			// Downstream scalar host references the reducer's `summary` field.
			&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.scan.summary }}"},
		}})
	diags := Validate(ld)
	assertNoCode(t, diags, "AWF5004")
	assertNoCode(t, diags, "AWF3001")
}

// TestReducedMapWholeOutputRefAccepted: a 2-seg `{{ step.scan }}` whole-output ref
// into a reduce-declaring map resolves to the reducer's whole output (a scalar
// object, not an array) — no AWF5004.
func TestReducedMapWholeOutputRefAccepted(t *testing.T) {
	ld := makeLD(&Workflow{ID: "agg", Version: 1,
		Containers: aggContainer(),
		Graph: NodeList{
			aggFindURLs(),
			&Map{Over: Expr("{{ step.find_urls.urls }}"), As: "u", Container: "c", Concurrency: intPtr(1),
				Body: NodeList{
					&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code",
						With: RawConfig{"prompt": "Scan {{ u }}"}, OutputSchema: aggScanSchema()},
				},
				Reduce: &Reduce{Run: "true", Container: "c", OutputSchema: reduceRunSchema()},
			},
			&Map{Over: Expr("{{ step.scan }}"), As: "f", Container: "c", Concurrency: intPtr(1), Body: NodeList{
				&AgentStep{ID: "verify", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{"prompt": "v"}},
			}},
		}})
	diags := Validate(ld)
	assertNoCode(t, diags, "AWF5004")
}

// TestReducedMapNonDeclaredRunReducerFieldErrors: a ref to a field the reducer's
// output_schema does NOT declare still errors (AWF3001), not AWF5004.
func TestReducedMapNonDeclaredRunReducerFieldErrors(t *testing.T) {
	ld := makeLD(&Workflow{ID: "agg", Version: 1,
		Containers: aggContainer(),
		Graph: NodeList{
			aggFindURLs(),
			&Map{Over: Expr("{{ step.find_urls.urls }}"), As: "u", Container: "c", Concurrency: intPtr(1),
				Body: NodeList{
					&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code",
						With: RawConfig{"prompt": "Scan {{ u }}"}, OutputSchema: aggScanSchema()},
				},
				Reduce: &Reduce{Run: "true", Container: "c", OutputSchema: reduceRunSchema()},
			},
			// `finding` is a BODY-step field, NOT a reducer field → AWF3001.
			&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.scan.finding }}"},
		}})
	diags := Validate(ld)
	assertNoCode(t, diags, "AWF5004")
	assertErrorAt(t, diags, "AWF3001", "after.run")
}

// TestReducedMapQuorumReducerFieldsAccepted: a ref to a quorum reducer's fixed
// {votes, agree, votes_detail} keys, or to the reduce's own field: name, VALIDATES
// (no AWF5004, no AWF3001).
func TestReducedMapQuorumReducerFieldsAccepted(t *testing.T) {
	body := func() NodeList {
		return NodeList{
			&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code",
				With: RawConfig{"prompt": "Scan {{ u }}"}, OutputSchema: &JSONSchema{
					"type": "object", "additionalProperties": false,
					"required":   []any{"concur"},
					"properties": map[string]any{"concur": map[string]any{"type": "boolean"}}}},
		}
	}
	// "concur" is the reduce's own field: name (not a reserved key — field: "agree"
	// would be rejected by AWF1068); "votes"/"agree"/"votes_detail" are accepted
	// purely via the fixed QuorumVerdictFields set.
	for _, field := range []string{"concur", "votes", "agree", "votes_detail"} {
		ld := makeLD(&Workflow{ID: "agg", Version: 1,
			Containers: aggContainer(),
			Graph: NodeList{
				aggFindURLs(),
				&Map{Over: Expr("{{ step.find_urls.urls }}"), As: "u", Container: "c", Concurrency: intPtr(1),
					Body:   body(),
					Reduce: &Reduce{Quorum: reduceRatio("2"), Field: "concur"},
				},
				&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.scan." + field + " }}"},
			}})
		diags := Validate(ld)
		assertNoCode(t, diags, "AWF5004")
		assertNoErrorCode(t, diags, "AWF3001")
		assertNoErrorCode(t, diags, "AWF1068")
	}
}

// TestReducedMapQuorumNonDeclaredFieldErrors: a ref to a field that is NEITHER
// the reduce's own field: NOR in the quorum reducer's fixed {votes, agree,
// votes_detail} shape still errors (AWF3001), not AWF5004.
func TestReducedMapQuorumNonDeclaredFieldErrors(t *testing.T) {
	ld := makeLD(&Workflow{ID: "agg", Version: 1,
		Containers: aggContainer(),
		Graph: NodeList{
			aggFindURLs(),
			&Map{Over: Expr("{{ step.find_urls.urls }}"), As: "u", Container: "c", Concurrency: intPtr(1),
				Body: NodeList{
					&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code",
						With: RawConfig{"prompt": "Scan {{ u }}"}, OutputSchema: &JSONSchema{
							"type": "object", "additionalProperties": false,
							"required":   []any{"concur"},
							"properties": map[string]any{"concur": map[string]any{"type": "boolean"}}}},
				},
				Reduce: &Reduce{Quorum: reduceRatio("2"), Field: "concur"},
			},
			// `summary` is neither field: concur nor in the quorum verdict's fixed
			// shape → AWF3001.
			&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.scan.summary }}"},
		}})
	diags := Validate(ld)
	assertNoCode(t, diags, "AWF5004")
	assertErrorAt(t, diags, "AWF3001", "after.run")
}

// TestNonReduceMapStillEmitsAWF5004: a ref into a NON-reduce map (no reduce:) still
// emits AWF5004 — unchanged behavior. (Mirror of TestAggregateRefInRunHostRejectedAWF5004
// without the reduce: clause, asserting the new branch leaves it alone.)
func TestNonReduceMapStillEmitsAWF5004(t *testing.T) {
	ld := makeLD(&Workflow{ID: "agg", Version: 1,
		Containers: aggContainer(),
		Graph: NodeList{
			aggFindURLs(),
			&Map{Over: Expr("{{ step.find_urls.urls }}"), As: "u", Container: "c", Concurrency: intPtr(1), Body: NodeList{
				&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code",
					With: RawConfig{"prompt": "Scan {{ u }}"}, OutputSchema: aggScanSchema()},
			}},
			&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.scan.finding }}"},
		}})
	assertErrorAt(t, Validate(ld), "AWF5004", "after.run")
}
