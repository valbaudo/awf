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

// mapWithReduceBody constructs a workflow with a single Map whose body is the
// given NodeList and whose reduce is a valid run: form. Used by the AWF5007
// nesting tests below.
func mapWithReduceBody(body NodeList) *LoadedDefinition {
	r := &Reduce{Run: "./merge.sh", Container: "lab"}
	return makeLD(&Workflow{
		ID: "reduce-nesting-wf", Version: 1,
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Map{
				Over: Expr("{{ input.items }}"), As: "item", Container: "lab", Concurrency: 1,
				Reduce: r,
				Body:   body,
			},
		},
	})
}

// codeProducer is a code step that declares output_files — qualifies as a
// fan-in producer that reduce would collect.
func codeProducer(id string) *CodeStep {
	return &CodeStep{
		ID:          id,
		Container:   "lab",
		Run:         "true",
		OutputFiles: OutputFiles{{Name: "out", Path: "/out/out.txt"}},
	}
}

func TestValidateReduceLoopNestedProducerAWF5007(t *testing.T) {
	// A producer nested under a loop in a reduce body → AWF5007.
	body := NodeList{
		&Loop{
			MaxIters: intPtr(3),
			Body:     NodeList{codeProducer("producer")},
		},
	}
	assertErrorAt(t, Validate(mapWithReduceBody(body)), "AWF5007", "map[0].reduce")
}

func TestValidateReduceNestedGateProducerAWF5007(t *testing.T) {
	// A producer nested under two gates (gate within gate's generate) → AWF5007.
	innerCode := codeProducer("producer")
	innerGate := &Gate{
		Generate:    NodeList{innerCode},
		Evaluate:    NodeList{&CodeStep{ID: "inner-eval", Container: "lab", Run: "true", OutputSchema: boolSchema("ok")}},
		Until:       Expr("{{ evaluate.ok }}"),
		MaxAttempts: 2,
	}
	outerGate := &Gate{
		Generate:    NodeList{innerGate},
		Evaluate:    NodeList{&CodeStep{ID: "outer-eval", Container: "lab", Run: "true", OutputSchema: boolSchema("ok")}},
		Until:       Expr("{{ evaluate.ok }}"),
		MaxAttempts: 2,
	}
	body := NodeList{outerGate}
	assertErrorAt(t, Validate(mapWithReduceBody(body)), "AWF5007", "map[0].reduce")
}

func TestValidateReduceSingleGateProducerNoAWF5007(t *testing.T) {
	// A producer nested under exactly one gate → allowed (Task-1 handles it).
	innerCode := codeProducer("producer")
	gate := &Gate{
		Generate:    NodeList{innerCode},
		Evaluate:    NodeList{&CodeStep{ID: "eval", Container: "lab", Run: "true", OutputSchema: boolSchema("ok")}},
		Until:       Expr("{{ evaluate.ok }}"),
		MaxAttempts: 2,
	}
	body := NodeList{gate}
	assertNoErrorCode(t, Validate(mapWithReduceBody(body)), "AWF5007")
}

func TestValidateReducePlainProducerNoAWF5007(t *testing.T) {
	// A producer directly in the map body (no nesting) → allowed.
	body := NodeList{codeProducer("producer")}
	assertNoErrorCode(t, Validate(mapWithReduceBody(body)), "AWF5007")
}

func TestValidateReduceGateInIfProducerNoAWF5007(t *testing.T) {
	// A gate with a producer inside an if branch → if adds no runtime multiplicity,
	// only the single enclosing gate counts. No AWF5007.
	innerCode := codeProducer("producer")
	gate := &Gate{
		Generate:    NodeList{innerCode},
		Evaluate:    NodeList{&CodeStep{ID: "eval", Container: "lab", Run: "true", OutputSchema: boolSchema("ok")}},
		Until:       Expr("{{ evaluate.ok }}"),
		MaxAttempts: 2,
	}
	body := NodeList{
		&If{
			Cond: Expr("{{ input.flag }}"),
			Then: NodeList{gate},
		},
	}
	assertNoErrorCode(t, Validate(mapWithReduceBody(body)), "AWF5007")
}
