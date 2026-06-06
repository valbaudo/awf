package ir

import "testing"

// Tests for the reduce: fan-in shape pass — see validate_reduce.go (AWF1035 =
// reduce shape fault; AWF5006 = quorum/over aggregation-scope fault).

// reduceRatio builds a *Ratio (= *json.Number) from a literal, the form a
// parsed quorum/min_success carries.
func reduceRatio(s string) *Ratio { r := Ratio(s); return &r }

// boolSchema is a body output_schema declaring one boolean field — the per-branch
// vote a quorum's over: counts.
func boolSchema(field string) *JSONSchema {
	s := JSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			field: map[string]any{"type": "boolean"},
		},
	})
	return &s
}

// mapWithReduce wraps a one-step map body (a code step declaring `votes` as a
// boolean output) plus the given reduce on a minimal valid workflow. minSuccess
// is set on the map when non-nil (for the mutual-exclusion case).
func mapWithReduce(r *Reduce, minSuccess *Ratio) *LoadedDefinition {
	return makeLD(&Workflow{
		ID: "reduce-wf", Version: 1,
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Map{
				Over: Expr("{{ input.items }}"), As: "item", Container: "lab", Concurrency: 1,
				MinSuccess: minSuccess,
				Reduce:     r,
				Body: NodeList{
					&CodeStep{ID: "vote", Container: "lab", Run: "true", OutputSchema: boolSchema("votes")},
				},
			},
		},
	})
}

func TestValidateReduceNeither(t *testing.T) {
	// reduce: {} declares neither run: nor quorum: → AWF1035.
	assertErrorAt(t, Validate(mapWithReduce(&Reduce{}, nil)), "AWF1035", "map[0].reduce")
}

func TestValidateReduceBoth(t *testing.T) {
	// reduce declaring BOTH quorum: and run: → AWF1035.
	r := &Reduce{Quorum: reduceRatio("2"), Over: "votes", Run: "./m.sh", Container: "lab"}
	assertErrorAt(t, Validate(mapWithReduce(r, nil)), "AWF1035", "map[0].reduce")
}

func TestValidateReduceQuorumNoOver(t *testing.T) {
	// quorum without over: → AWF1035.
	r := &Reduce{Quorum: reduceRatio("2")}
	assertErrorAt(t, Validate(mapWithReduce(r, nil)), "AWF1035", "map[0].reduce")
}

func TestValidateReduceRunNoContainer(t *testing.T) {
	// run: reducer with no container: → AWF1035.
	r := &Reduce{Run: "./m.sh"}
	assertErrorAt(t, Validate(mapWithReduce(r, nil)), "AWF1035", "map[0].reduce")
}

func TestValidateReduceRunUndeclaredContainer(t *testing.T) {
	// run: reducer naming an undeclared container → AWF1009 (via checkContainerRef).
	r := &Reduce{Run: "./m.sh", Container: "nope"}
	assertErrorAt(t, Validate(mapWithReduce(r, nil)), "AWF1009", "map[0].reduce")
}

func TestValidateReduceQuorumOverNotDeclared(t *testing.T) {
	// quorum over: a field no body step declares → AWF5006.
	r := &Reduce{Quorum: reduceRatio("2"), Over: "ghost"}
	assertErrorAt(t, Validate(mapWithReduce(r, nil)), "AWF5006", "map[0].reduce")
}

func TestValidateReduceQuorumAndMinSuccess(t *testing.T) {
	// min_success AND reduce:{quorum} on the same map → AWF5006 (mutually exclusive).
	r := &Reduce{Quorum: reduceRatio("2"), Over: "votes"}
	assertErrorAt(t, Validate(mapWithReduce(r, reduceRatio("2"))), "AWF5006", "map[0].reduce")
}

func TestValidateReduceValidQuorum(t *testing.T) {
	// Valid quorum over a declared body field → no error.
	r := &Reduce{Quorum: reduceRatio("2"), Over: "votes"}
	diags := Validate(mapWithReduce(r, nil))
	assertNoErrorCode(t, diags, "AWF1035")
	assertNoErrorCode(t, diags, "AWF5006")
}

func TestValidateReduceValidRun(t *testing.T) {
	// Valid run: reducer in a declared container → no error.
	r := &Reduce{Run: "./m.sh", Container: "lab"}
	diags := Validate(mapWithReduce(r, nil))
	assertNoErrorCode(t, diags, "AWF1035")
	assertNoErrorCode(t, diags, "AWF5006")
	assertNoErrorCode(t, diags, "AWF1009")
}
