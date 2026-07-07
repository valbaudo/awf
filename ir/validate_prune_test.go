package ir

import (
	"encoding/json"
	"testing"
)

// Tests for the prune: frontier shape + score-binding pass — see validate_prune.go
// (AWF1037 = prune shape fault; AWF5008 = score-field binding fault). The prune
// clause is map-only (parallel-prune is unrepresentable in the wire form, see
// TestPruneOnParallelIsLoadError).

// numScoreSchema builds a body output_schema declaring `field` as a number — the
// typed score a prune: keep/stop_when reads.
func numScoreSchema(field string) *JSONSchema {
	s := JSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			field: map[string]any{"type": "number"},
		},
	})
	return &s
}

// mapWithPrune wraps a one-step map body (a code step declaring `score` as a
// number output) plus the given prune on a minimal valid workflow. The body's
// last step's output_schema is `schema` (nil for the no-schema case).
func mapWithPrune(p *Prune, schema *JSONSchema) *LoadedDefinition {
	return makeLD(&Workflow{
		ID: "prune-wf", Version: 1,
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Map{
				Over: Expr("{{ input.items }}"), As: "item", Container: "lab", Concurrency: intPtr(1),
				Prune: p,
				Body: NodeList{
					&CodeStep{ID: "gen", Container: "lab", Run: "true", OutputSchema: schema},
				},
			},
		},
	})
}

func TestValidatePruneNeither(t *testing.T) {
	// prune: declaring neither keep: nor stop_when: → AWF1037.
	p := &Prune{Score: "score"}
	assertErrorAt(t, Validate(mapWithPrune(p, numScoreSchema("score"))), "AWF1037", "map[0]")
}

func TestValidatePruneBoth(t *testing.T) {
	// prune: declaring BOTH keep: and stop_when: → AWF1037.
	p := &Prune{Score: "score", Keep: &PruneKeep{K: 2}, StopWhen: "{{ best.score >= 0.9 }}"}
	assertErrorAt(t, Validate(mapWithPrune(p, numScoreSchema("score"))), "AWF1037", "map[0]")
}

func TestValidatePruneEmptyScore(t *testing.T) {
	// prune: with an empty score: → AWF1037.
	p := &Prune{Keep: &PruneKeep{K: 2}}
	assertErrorAt(t, Validate(mapWithPrune(p, numScoreSchema("score"))), "AWF1037", "map[0]")
}

func TestValidatePruneKeepNonPositive(t *testing.T) {
	// A programmatically-built keep with k <= 0 → AWF1037 (the validator defends
	// even though the wire-form unmarshal rejects top(0) earlier).
	p := &Prune{Score: "score", Keep: &PruneKeep{K: 0}}
	assertErrorAt(t, Validate(mapWithPrune(p, numScoreSchema("score"))), "AWF1037", "map[0]")
}

func TestValidatePruneNoOutputSchema(t *testing.T) {
	// prune: on a body whose last step declares no output_schema → AWF5008.
	p := &Prune{Score: "score", Keep: &PruneKeep{K: 2}}
	assertErrorAt(t, Validate(mapWithPrune(p, nil)), "AWF5008", "map[0]")
}

func TestValidatePruneScoreNotInSchema(t *testing.T) {
	// prune: score: names a field the last step's output_schema does not declare → AWF5008.
	p := &Prune{Score: "nope", Keep: &PruneKeep{K: 2}}
	assertErrorAt(t, Validate(mapWithPrune(p, numScoreSchema("score"))), "AWF5008", "map[0]")
}

func TestValidatePruneScoreNotNumeric(t *testing.T) {
	// prune: score: names a field declared as a non-numeric type → AWF5008.
	strSchema := JSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"score": map[string]any{"type": "string"},
		},
	})
	p := &Prune{Score: "score", Keep: &PruneKeep{K: 2}}
	assertErrorAt(t, Validate(mapWithPrune(p, &strSchema)), "AWF5008", "map[0]")
}

func TestValidatePruneValidKeep(t *testing.T) {
	// Valid keep: 2 over a declared numeric score field → no error.
	p := &Prune{Score: "score", Keep: &PruneKeep{K: 2}}
	diags := Validate(mapWithPrune(p, numScoreSchema("score")))
	assertNoErrorCode(t, diags, "AWF1037")
	assertNoErrorCode(t, diags, "AWF5008")
}

func TestValidatePruneValidStopWhen(t *testing.T) {
	// Valid stop_when over a declared numeric score field → no error. The
	// expression is NOT statically type-checked (it fails at runtime via
	// EvalBoolString, like loop.until).
	p := &Prune{Score: "score", StopWhen: "{{ best.score >= 0.9 }}"}
	diags := Validate(mapWithPrune(p, numScoreSchema("score")))
	assertNoErrorCode(t, diags, "AWF1037")
	assertNoErrorCode(t, diags, "AWF5008")
}

func TestValidatePruneIntegerScoreIsNumeric(t *testing.T) {
	// An integer-typed score field is numeric → no AWF5008.
	intSchema := JSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"score": map[string]any{"type": "integer"},
		},
	})
	p := &Prune{Score: "score", Keep: &PruneKeep{K: 1}}
	diags := Validate(mapWithPrune(p, &intSchema))
	assertNoErrorCode(t, diags, "AWF5008")
}

// TestPruneOnParallelIsLoadError confirms parallel-prune is unrepresentable: a
// parallel: written as an object form ({children:[...],prune:{...}}) fails to
// unmarshal (Parallel decodes the bare array only) — there is no AWF-coded
// diagnostic for parallel-prune because it never reaches the validator.
func TestPruneOnParallelIsLoadError(t *testing.T) {
	_, err := unmarshalNode(json.RawMessage(`{"parallel":{"children":[{"id":"a","run":"x"}],"prune":{"score":"s","keep":"top(2)"}}}`))
	if err == nil {
		t.Fatal("want a load error for the parallel object form (parallel-prune unrepresentable); got nil")
	}
}

// TestValidatePruneIgnoresParallel confirms validatePrune only inspects *Map
// nodes — a hand-built *Parallel never trips it (the type switch's default arm
// returns), so there is no panic and no spurious diagnostic.
func TestValidatePruneIgnoresParallel(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "par-wf", Version: 1,
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Parallel{Children: NodeList{&CodeStep{ID: "a", Container: "lab", Run: "true"}}},
		},
	})
	diags := Validate(ld)
	assertNoCode(t, diags, "AWF1037")
	assertNoCode(t, diags, "AWF5008")
}
