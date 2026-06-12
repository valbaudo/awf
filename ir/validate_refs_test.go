package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tests for the AWF3001/AWF3002 reference pass — see validate_refs.go.

func TestSingleMapBodyShape(t *testing.T) {
	cases := []struct {
		path, wantMap, wantSuffix string
		wantOK                    bool
	}{
		{"map[0].body.scan", "map[0]", "scan", true},
		{"map[0].body.if[0].then.scan", "map[0]", "if[0].then.scan", true},
		{"scan", "", "", false},
		{"gate[0].generate.scan", "", "", false},
		{"map[0].body.map[1].body.scan", "", "", false},
		{"loop[0].body.map[1].body.scan", "", "", false},
	}
	for _, tc := range cases {
		gm, gs, ok := SingleMapBodyShape(tc.path)
		if ok != tc.wantOK || gm != tc.wantMap || gs != tc.wantSuffix {
			t.Errorf("SingleMapBodyShape(%q)=(%q,%q,%v); want (%q,%q,%v)", tc.path, gm, gs, ok, tc.wantMap, tc.wantSuffix, tc.wantOK)
		}
	}
}

func TestRefsStepFieldMustBeDeclared(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "refs", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{
				ID: "a", Container: "c", Run: "true",
				OutputSchema: &JSONSchema{"type": "object", "required": []any{"x"}, "properties": map[string]any{"x": map[string]any{"type": "integer"}}, "additionalProperties": false},
			},
			&CodeStep{
				ID: "b", Container: "c",
				Run: "echo {{ step.a.y }}", // y not declared by a — AWF3001
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3001", "b.run")
}

func TestRefsToUndeclaredStepReports(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "refs", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "c", Run: "echo {{ step.nope.x }}"}, // nope doesn't exist
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3001", "a.run")
}

func TestRefsExitCodeAndStdoutAlwaysOK(t *testing.T) {
	// step.<id>.exit_code and step.<id>.stdout are always allowed without an output_schema.
	ld := makeLD(&Workflow{
		ID: "ec", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "c", Run: "true"},
			&CodeStep{ID: "b", Container: "c", Run: "echo {{ step.a.exit_code }} {{ step.a.stdout }}"},
		},
	})
	for _, d := range Validate(ld) {
		if d.Code == "AWF3001" {
			t.Errorf("did not expect AWF3001 for exit_code/stdout: %v", d)
		}
	}
}

func TestRefsAgentSchemaWithoutRefWarnsAWF3002(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "iff", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code",
				With:         RawConfig{"skill": "x"},
				OutputSchema: &JSONSchema{"type": "object", "required": []any{"verdict"}, "properties": map[string]any{"verdict": map[string]any{"type": "boolean"}}, "additionalProperties": false},
			},
			// No downstream ref into step.a — AWF3002 warning
		},
	})
	found := false
	for _, d := range Validate(ld) {
		if d.Code == "AWF3002" && d.Severity == Warning && strings.Contains(d.Path, "a") {
			found = true
		}
	}
	if !found {
		t.Errorf("want AWF3002 warning for unreferenced agent output_schema; got %+v", Validate(ld))
	}
}

func TestRefsEscapedBraceIsNotARef(t *testing.T) {
	// Host-level escape (AWF §7): `\{{ … }}` is a literal `{{`, NOT a reference. The
	// validator must skip it — otherwise `\{{ step.ghost.x }}` would trip AWF3001
	// ("undeclared step") even though the author meant a literal brace. checkTemplateRefs
	// routes run / with-leaves / idempotency_key through template.Slots, so the escape is
	// uniform across all of them; run: is the representative case here.
	ld := makeLD(&Workflow{
		ID: "esc", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "c", Run: `echo \{{ step.ghost.x }} and \{{7*7}}`},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF3001")
}

func TestRefsExprConditionResolved(t *testing.T) {
	// An expression inside if.cond references step.a.exit_code — should not produce AWF3001.
	ld := makeLD(&Workflow{
		ID: "cond", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "c", Run: "true"},
			&If{Cond: Expr("{{ step.a.exit_code == 0 }}"), Then: NodeList{
				&CodeStep{ID: "b", Container: "c", Run: "true"},
			}},
		},
	})
	for _, d := range Validate(ld) {
		if d.Code == "AWF3001" {
			t.Errorf("did not expect AWF3001: %v", d)
		}
	}
}

func TestRefsMalformedTemplateProducesAWF3001(t *testing.T) {
	// A Template field with a malformed slot inner (parse fails) surfaces as AWF3001 with
	// the parser's message. The validator doesn't have its own "malformed template" code;
	// the iff-referenced check is the place where parser errors surface.
	ld := makeLD(&Workflow{
		ID: "bad", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "c", Run: "echo {{ a..b }}"}, // empty segment
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3001", "a.run")
}

func TestRefsExitCodeOnAgentStepRejected(t *testing.T) {
	// AWF §4.1 scopes exit_code/stdout to code steps. An agent step doesn't have these.
	ld := makeLD(&Workflow{
		ID: "ec-on-agent", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{ID: "a", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{}},
			&CodeStep{ID: "b", Container: "c", Run: "echo {{ step.a.exit_code }}"}, // not allowed
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3001", "b.run")
}

func TestRefsRunMustBeRunID(t *testing.T) {
	// Per AWF §7, the only defined `run` reference is run.id.
	ld := makeLD(&Workflow{
		ID: "run-typo", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "c", Run: "echo {{ run.typo }}"},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3001", "a.run")
}

func TestRefsRunIDIsAccepted(t *testing.T) {
	// Smoke: run.id alone is fine.
	ld := makeLD(&Workflow{
		ID: "run-id", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "c", Run: "echo {{ run.id }}"},
		},
	})
	for _, d := range Validate(ld) {
		if d.Code == "AWF3001" {
			t.Errorf("did not expect AWF3001 for run.id: %v", d)
		}
	}
}

// TestValidateRefsEvaluateScope exercises AWF5001: evaluate.<field> is only legal inside
// gate.generate or gate.until. Anywhere else (top-level, gate.evaluate subtree) is a static
// error — the static counterpart of the runtime scope check in engine.Scope.resolveEvaluate.
func TestValidateRefsEvaluateScope(t *testing.T) {
	// Reusable producer schema for the gate's evaluator final node — must declare a `verified`
	// boolean so AWF1014 passes and the evaluate.verified reference also resolves.
	verifySchema := &JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"verified"},
		"properties":           map[string]any{"verified": map[string]any{"type": "boolean"}},
	}

	cases := []struct {
		name     string
		graph    NodeList
		wantCode string // "" = no AWF5001 emitted
	}{
		{
			name: "allowed in gate.generate",
			graph: NodeList{
				&Gate{
					Generate: NodeList{
						// Reference evaluate.feedback from inside gate.generate — allowed.
						&CodeStep{ID: "gen1", Container: "c0", Run: "echo {{ evaluate.feedback }}"},
					},
					Evaluate: NodeList{
						&CodeStep{ID: "eval1", Container: "c0", Run: "eval", OutputSchema: verifySchema},
					},
					Until:       "{{ evaluate.verified }}",
					MaxAttempts: 3,
				},
			},
			wantCode: "",
		},
		{
			name: "allowed in gate.until",
			graph: NodeList{
				&Gate{
					Generate: NodeList{
						&CodeStep{ID: "gen1", Container: "c0", Run: "gen"},
					},
					Evaluate: NodeList{
						&CodeStep{ID: "eval1", Container: "c0", Run: "eval", OutputSchema: verifySchema},
					},
					Until:       "{{ evaluate.verified }}", // until permits evaluate.*
					MaxAttempts: 3,
				},
			},
			wantCode: "",
		},
		{
			name: "rejected outside any gate",
			graph: NodeList{
				&CodeStep{ID: "step1", Container: "c0", Run: "echo {{ evaluate.feedback }}"},
			},
			wantCode: "AWF5001",
		},
		{
			name: "rejected in gate.evaluate",
			graph: NodeList{
				&Gate{
					Generate: NodeList{
						&CodeStep{ID: "gen1", Container: "c0", Run: "gen"},
					},
					Evaluate: NodeList{
						// evaluate.* INSIDE the evaluator sub-tree — rejected (the evaluator
						// cannot reference its own in-flight output).
						&CodeStep{ID: "eval1", Container: "c0", Run: "echo {{ evaluate.verified }}", OutputSchema: verifySchema},
					},
					Until:       "{{ evaluate.verified }}",
					MaxAttempts: 3,
				},
			},
			wantCode: "AWF5001",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ld := makeLD(&Workflow{
				ID: "x", Version: 1,
				Containers: map[string]Container{"c0": {Image: "oci://e.com/r@sha256:0000000000000000000000000000000000000000000000000000000000000000"}},
				Graph:      c.graph,
			})
			diags := Validate(ld)
			var found bool
			for _, d := range diags {
				if d.Code == "AWF5001" {
					found = true
				}
			}
			if c.wantCode == "AWF5001" && !found {
				t.Errorf("want AWF5001 emitted; diags = %v", diags)
			}
			if c.wantCode == "" && found {
				t.Errorf("AWF5001 emitted unexpectedly; diags = %v", diags)
			}
		})
	}
}

// --- AWF5003: cross-scope references into gate/map bodies ---

// codeStep is a tiny fixture helper for the AWF5003 tests.
func awf5003Step(id string) *CodeStep { return &CodeStep{ID: id, Container: "c", Run: "true"} }

func awf5003Container() map[string]Container {
	return map[string]Container{"c": {Image: "oci://x@sha256:0000000000000000000000000000000000000000000000000000000000000000"}}
}

func TestRefsCrossScopeIntoMapBodyRejected(t *testing.T) {
	// Under Approach A (map output aggregation), a `step.<id>` ref to a producer inside a
	// single map, from a site outside that map, is an AGGREGATE (array-typed) ref. It is
	// legal only as another map's `over:`. Here the ref site is a `run:` host (scalar) →
	// AWF5004, not the old opaque-scope AWF5003. (exit_code is checked only after the
	// over-sink gate, so the over-sink rejection wins.)
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Input:      &JSONSchema{"type": "object", "required": []any{"xs"}, "properties": map[string]any{"xs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "additionalProperties": false},
		Graph: NodeList{
			&Map{Over: Expr("{{ input.xs }}"), As: "x", Container: "c", Concurrency: 1, Body: NodeList{
				awf5003Step("inner"),
			}},
			// A top-level step referencing a single-map-body producer from a scalar host.
			&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.inner.exit_code }}"},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF5004", "after.run")
}

func TestRefsCrossScopeIntoGateRejected(t *testing.T) {
	schema := &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&Gate{
				Generate:    NodeList{awf5003Step("gen")},
				Evaluate:    NodeList{&CodeStep{ID: "judge", Container: "c", Run: "true", OutputSchema: schema}},
				Until:       Expr("{{ step.judge.exit_code == 0 }}"),
				MaxAttempts: 2,
			},
			// A top-level step reaching into the gate's generate: from outside the
			// gate there's no defined attempt → AWF5003.
			&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.gen.exit_code }}"},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF5003", "after.run")
}

func TestRefsSameItemMapSiblingAllowed(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Input:      &JSONSchema{"type": "object", "required": []any{"xs"}, "properties": map[string]any{"xs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "additionalProperties": false},
		Graph: NodeList{
			&Map{Over: Expr("{{ input.xs }}"), As: "x", Container: "c", Concurrency: 1, Body: NodeList{
				awf5003Step("a"),
				&CodeStep{ID: "b", Container: "c", Run: "echo {{ step.a.exit_code }}"}, // same item → allowed
			}},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF5003")
}

func TestRefsSameAttemptGateSiblingAllowed(t *testing.T) {
	schema := &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&Gate{
				Generate: NodeList{awf5003Step("gen")},
				// evaluate referencing a generate step: same gate → allowed BY DESIGN. The
				// README flagship example relies on this (the judge reads step.draft.reply_path
				// to locate the artifact); see the independence note in awf-workflow(5). Do not
				// "fix" this to AWF5003 without a deliberate format revision.
				Evaluate:    NodeList{&CodeStep{ID: "judge", Container: "c", Run: "echo {{ step.gen.exit_code }}", OutputSchema: schema}},
				Until:       Expr("{{ step.judge.exit_code == 0 }}"),
				MaxAttempts: 2,
			},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF5003")
}

func TestRefsIntoTryIsTransparent(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&Try{Do: NodeList{awf5003Step("work")}},
			// try introduces no multiplicity → a step inside it is referenceable
			// from outside, exactly like a top-level step.
			&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.work.exit_code }}"},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF5003")
}

// --- Task 1.1: AWF3001/AWF3002 for with: string leaves ---

// TestRefsAgentSchemaReferencedInWithPromptNoAWF3002 asserts that a valid
// step.<id>.<field> reference inside a with: string value counts as an inbound
// ref and suppresses AWF3002 (the schema IS referenced).
func TestRefsAgentSchemaReferencedInWithPromptNoAWF3002(t *testing.T) {
	ld := makeLD(&Workflow{ID: "withref", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{ID: "scan", Container: "c", With: RawConfig{"prompt": "scan it"},
				OutputSchema: &JSONSchema{"type": "object", "required": []any{"finding"}, "properties": map[string]any{"finding": map[string]any{"type": "string"}}, "additionalProperties": false}},
			&AgentStep{ID: "verify", Container: "c", With: RawConfig{"prompt": "verify {{ step.scan.finding }}"}},
		}})
	assertNoCode(t, Validate(ld), "AWF3002")
}

func TestRefsAgentSchemaReferencedInWorkflowOutputsNoAWF3002(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "exports-ref", Version: 1,
		Containers:   map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		OutputSchema: objectSchema("finding"),
		Outputs: map[string]TemplateValue{
			"finding": []byte(`"{{ step.scan.finding }}"`),
		},
		Graph: NodeList{
			&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{"prompt": "scan"},
				OutputSchema: objectSchema("finding")},
		},
	})

	assertNoCode(t, Validate(ld), "AWF3002")
}

func TestRefsAgentSchemaReferencedInCallInputNoAWF3002(t *testing.T) {
	root := validCallingRoot()
	root.Containers = map[string]Container{"c": {Image: "oci://x@sha256:abc"}}
	root.Graph = NodeList{
		&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{"prompt": "scan"},
			OutputSchema: objectSchema("finding")},
		&CallStep{
			ID:   "child",
			Call: "scan",
			Input: map[string]TemplateValue{
				"query": []byte(`"{{ step.scan.finding }}"`),
			},
		},
	}
	child := childWorkflowWithTypedOutput("child", "summary")
	child.Input = objectSchema("query")
	ld := loadedWithChild(root, child)

	assertNoCode(t, Validate(ld), "AWF3002")
}

// TestRefsBrokenRefInWithPromptReportsAWF3001 asserts that a broken reference
// inside a with: string value emits AWF3001 at the path "<step-id>.with.<key>".
func TestRefsBrokenRefInWithPromptReportsAWF3001(t *testing.T) {
	ld := makeLD(&Workflow{ID: "withbroken", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{ID: "scan", Container: "c", With: RawConfig{"prompt": "scan"},
				OutputSchema: &JSONSchema{"type": "object", "required": []any{"finding"}, "properties": map[string]any{"finding": map[string]any{"type": "string"}}, "additionalProperties": false}},
			&AgentStep{ID: "verify", Container: "c", With: RawConfig{"prompt": "verify {{ step.scan.nope }}"}},
		}})
	assertErrorAt(t, Validate(ld), "AWF3001", "verify.with.prompt")
}

func TestRefsCrossScopeNestedMapInGateRejected(t *testing.T) {
	// A step inside a gate's generate but OUTSIDE the map nested within it
	// references a step inside that map's body: the innermost opaque scope (the
	// map) doesn't enclose the reference site → AWF5003, even though both sit in
	// the same gate.
	schema := &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Input:      &JSONSchema{"type": "object", "required": []any{"xs"}, "properties": map[string]any{"xs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "additionalProperties": false},
		Graph: NodeList{
			&Gate{
				Generate: NodeList{
					&Map{Over: Expr("{{ input.xs }}"), As: "x", Container: "c", Concurrency: 1, Body: NodeList{
						awf5003Step("inner"),
					}},
					// same gate attempt, but outside the map item → cross-item.
					&CodeStep{ID: "sibling", Container: "c", Run: "echo {{ step.inner.exit_code }}"},
				},
				Evaluate:    NodeList{&CodeStep{ID: "judge", Container: "c", Run: "true", OutputSchema: schema}},
				Until:       Expr("{{ step.judge.exit_code == 0 }}"),
				MaxAttempts: 2,
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF5003", "gate[0].generate.sibling.run")
}

// --- Task 1.2: AWF3006 — code step with output_schema but no $AWF_OUTPUT write ---

// TestCodeStepWithSchemaButNoAwfOutputWarnsAWF3006 asserts that a code step
// declaring output_schema but whose run script never writes $AWF_OUTPUT gets a
// Warning with code AWF3006 (the output will silently be missing at runtime).
func TestCodeStepWithSchemaButNoAwfOutputWarnsAWF3006(t *testing.T) {
	ld := makeLD(&Workflow{ID: "noout", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{&CodeStep{ID: "recon", Container: "c", Run: "echo hi",
			OutputSchema: &JSONSchema{"type": "object", "required": []any{"x"}, "properties": map[string]any{"x": map[string]any{"type": "integer"}}, "additionalProperties": false}}}})
	assertWarningAt(t, Validate(ld), "AWF3006", "recon")
}

// TestCodeStepWithoutSchemaNoAWF3006 asserts that AWF3006 fires ONLY when output_schema
// is declared — a code step with no schema and no $AWF_OUTPUT write must stay silent.
func TestCodeStepWithoutSchemaNoAWF3006(t *testing.T) {
	ld := makeLD(&Workflow{ID: "noschema", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph:      NodeList{&CodeStep{ID: "recon", Container: "c", Run: "echo hi"}}}) // no output_schema
	assertNoCode(t, Validate(ld), "AWF3006")
}

// TestCodeStepWritingAwfOutputNoAWF3006 asserts that a code step that writes
// $AWF_OUTPUT does NOT trigger AWF3006.
func TestCodeStepWritingAwfOutputNoAWF3006(t *testing.T) {
	ld := makeLD(&Workflow{ID: "ok", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{&CodeStep{ID: "recon", Container: "c", Run: `echo '{"x":1}' > "$AWF_OUTPUT"`,
			OutputSchema: &JSONSchema{"type": "object", "required": []any{"x"}, "properties": map[string]any{"x": map[string]any{"type": "integer"}}, "additionalProperties": false}}}})
	assertNoCode(t, Validate(ld), "AWF3006")
}

// --- Task 5.2: map output aggregation (Approach A — step-ref lift) ---

// scanSchema is the body-step schema for the aggregation tests: a `finding` string and an
// `index` integer (the per-item correlation field, per spec §1.2 decision 3).
func aggScanSchema() *JSONSchema {
	return &JSONSchema{
		"type": "object", "additionalProperties": false,
		"required":   []any{"finding", "index"},
		"properties": map[string]any{"finding": map[string]any{"type": "string"}, "index": map[string]any{"type": "integer"}},
	}
}

func aggContainer() map[string]Container {
	return map[string]Container{"c": {Image: "oci://x@sha256:0000000000000000000000000000000000000000000000000000000000000000"}}
}

// aggFindURLs is a seed code step producing an array `urls` field, used as mapA's over.
func aggFindURLs() *CodeStep {
	return &CodeStep{ID: "find_urls", Container: "c", Run: `echo '{"urls":[]}' > "$AWF_OUTPUT"`,
		OutputSchema: &JSONSchema{"type": "object", "additionalProperties": false,
			"required": []any{"urls"}, "properties": map[string]any{"urls": map[string]any{"type": "array"}}}}
}

// TestAggregateRefIntoOverIsAccepted: seed code step → mapA(body agent `scan`) → mapB
// `over: "{{ step.scan }}"`. The 2-seg whole-object aggregate ref into another map's `over:`
// is the chaining primitive — valid, no AWF5002/AWF5003/AWF5004.
func TestAggregateRefIntoOverIsAccepted(t *testing.T) {
	ld := makeLD(&Workflow{ID: "agg", Version: 1,
		Containers: aggContainer(),
		Graph: NodeList{
			aggFindURLs(),
			&Map{Over: Expr("{{ step.find_urls.urls }}"), As: "u", Container: "c", Concurrency: 1, Body: NodeList{
				&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code",
					With: RawConfig{"prompt": "Scan {{ u }}"}, OutputSchema: aggScanSchema()},
			}},
			&Map{Over: Expr("{{ step.scan }}"), As: "f", Container: "c", Concurrency: 1, Body: NodeList{
				&AgentStep{ID: "verify", Container: "c", Uses: "anthropic/claude-code",
					With: RawConfig{"prompt": "Verify {{ f.finding }} (item {{ f.index }})"}},
			}},
		}})
	diags := Validate(ld)
	assertNoCode(t, diags, "AWF5002")
	assertNoCode(t, diags, "AWF5003")
	assertNoCode(t, diags, "AWF5004")
}

// TestAggregateRefInRunHostRejectedAWF5004: the same producer referenced from a scalar
// `run:` host (a code step after the map) is an aggregate used outside `over:` → AWF5004.
func TestAggregateRefInRunHostRejectedAWF5004(t *testing.T) {
	ld := makeLD(&Workflow{ID: "agg", Version: 1,
		Containers: aggContainer(),
		Graph: NodeList{
			aggFindURLs(),
			&Map{Over: Expr("{{ step.find_urls.urls }}"), As: "u", Container: "c", Concurrency: 1, Body: NodeList{
				&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code",
					With: RawConfig{"prompt": "Scan {{ u }}"}, OutputSchema: aggScanSchema()},
			}},
			&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.scan.finding }}"},
		}})
	assertErrorAt(t, Validate(ld), "AWF5004", "after.run")
}

// TestAggregateExitCodeRejectedAWF5005: exit_code/stdout are not defined on an aggregate
// (a map-internal step aggregates to []output / []field only). Even in an `over:` sink,
// `step.scan.exit_code` → AWF5005.
func TestAggregateExitCodeRejectedAWF5005(t *testing.T) {
	ld := makeLD(&Workflow{ID: "agg", Version: 1,
		Containers: aggContainer(),
		Graph: NodeList{
			aggFindURLs(),
			&Map{Over: Expr("{{ step.find_urls.urls }}"), As: "u", Container: "c", Concurrency: 1, Body: NodeList{
				&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code",
					With: RawConfig{"prompt": "Scan {{ u }}"}, OutputSchema: aggScanSchema()},
			}},
			&Map{Over: Expr("{{ step.scan.exit_code }}"), As: "f", Container: "c", Concurrency: 1, Body: NodeList{
				&AgentStep{ID: "verify", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{"prompt": "v"}},
			}},
		}})
	assertErrorAt(t, Validate(ld), "AWF5005", "map[2].over")
}

// TestAggregateLoopMultipliedMapDeferredAWF5002: a producer inside a loop body's map is
// referenced from a consuming map OUTSIDE the loop. The producer path contains "loop["
// so SingleMapBodyShape returns false; with no "gate[" in the path, opaqueScopePrefix
// finds the inner map's body as the opaque scope and routes to AWF5002.
func TestAggregateLoopMultipliedMapDeferredAWF5002(t *testing.T) {
	maxIters := 3
	ld := makeLD(&Workflow{ID: "agg-loop", Version: 1,
		Containers: aggContainer(),
		Input: &JSONSchema{
			"type": "object", "additionalProperties": false,
			"required":   []any{"items"},
			"properties": map[string]any{"items": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
		},
		Graph: NodeList{
			// loop[0]: body contains map[0] producing a typed `finding` field.
			&Loop{MaxIters: &maxIters, Body: NodeList{
				&Map{Over: Expr("{{ input.items }}"), As: "x", Container: "c", Concurrency: 1, Body: NodeList{
					&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code",
						With: RawConfig{"prompt": "scan {{ x }}"}, OutputSchema: aggScanSchema()},
				}},
			}},
			// map[1] (outside the loop): tries to use the loop-multiplied map's producer as
			// its over. Producer path = "loop[0].body.map[0].body.scan" → loop[ present →
			// SingleMapBodyShape=false → opaque scope → no gate[ → AWF5002.
			&Map{Over: Expr("{{ step.scan.finding }}"), As: "f", Container: "c", Concurrency: 1, Body: NodeList{
				&AgentStep{ID: "verify", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{"prompt": "v"}},
			}},
		}})
	assertErrorAt(t, Validate(ld), "AWF5002", "map[1].over")
}

// TestAggregateNestedMapDeferredAWF5002: a producer enclosed by TWO maps (map-in-map),
// referenced from a map outside both → not the v1 single-map shape → AWF5002 ("deferred").
func TestAggregateNestedMapDeferredAWF5002(t *testing.T) {
	ld := makeLD(&Workflow{ID: "agg", Version: 1,
		Containers: aggContainer(),
		Graph: NodeList{
			aggFindURLs(),
			&Map{Over: Expr("{{ step.find_urls.urls }}"), As: "u", Container: "c", Concurrency: 1, Body: NodeList{
				&Map{Over: Expr("{{ u }}"), As: "v", Container: "c", Concurrency: 1, Body: NodeList{
					&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code",
						With: RawConfig{"prompt": "Scan {{ v }}"}, OutputSchema: aggScanSchema()},
				}},
			}},
			// A 3-segment field ref into the map-in-map producer's declared `finding`
			// field. The non-v1-shape producer (two enclosing maps) makes this the
			// opaque-scope reject; with no gate in the producer path → AWF5002.
			&Map{Over: Expr("{{ step.scan.finding }}"), As: "f", Container: "c", Concurrency: 1, Body: NodeList{
				&AgentStep{ID: "verify", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{"prompt": "v"}},
			}},
		}})
	assertErrorAt(t, Validate(ld), "AWF5002", "map[2].over")
}

// --- Task 4: AWF1048 conditional-scope WARNING ---

func TestConditionallyScoped(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"summarize", false},
		{"recon.scan", false},
		{"if[0].then.draft", true},
		{"loop[0].body.work", true},
		// gate[ and map[ are OPAQUE: a top-level output ref into them is ALREADY a
		// hard error (AWF5003 / AWF5004), so the warning must NOT fire there.
		{"gate[0].generate.draft", false},
		{"map[0].body.x", false},
	}
	for _, c := range cases {
		if got := conditionallyScoped(c.path); got != c.want {
			t.Errorf("conditionallyScoped(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestOutputStepRefs(t *testing.T) {
	got := outputStepRefs(TemplateValue(`"{{ step.foo.bar }} and {{ step.baz.qux }}"`))
	want := []string{"foo", "baz"}
	if len(got) != len(want) {
		t.Fatalf("outputStepRefs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outputStepRefs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Parser-accuracy: a literal `step.x.` OUTSIDE a {{ }} slot must NOT be harvested.
func TestOutputStepRefsIgnoresLiteralText(t *testing.T) {
	got := outputStepRefs(TemplateValue(`"see step.foo.bar in the docs"`))
	if len(got) != 0 {
		t.Fatalf("outputStepRefs = %v, want none (literal text, no slot)", got)
	}
}

func TestOutputStepRefsDedups(t *testing.T) {
	got := outputStepRefs(TemplateValue(`"{{ step.draft.x }} and {{ step.draft.y }}"`))
	if len(got) != 1 || got[0] != "draft" {
		t.Fatalf("outputStepRefs = %v, want [draft] (deduped)", got)
	}
}

func TestValidateWarnsOutputBindsIfNestedStep(t *testing.T) {
	// A top-level output binding a step inside if.then validates CLEAN (if is
	// transparent) but may not commit at runtime. After Step 5 wiring, expect a
	// non-fatal AWF1048 WARNING at outputs.summary. Mutation-grade: deleting the
	// Step-5 warning turns this RED.
	strObj := func(field string) *JSONSchema {
		return &JSONSchema{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{field},
			"properties":           map[string]any{field: map[string]any{"type": "string"}},
		}
	}
	wf := &Workflow{
		ID:         "cond-out",
		Version:    1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		Input: &JSONSchema{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"do_it"},
			"properties":           map[string]any{"do_it": map[string]any{"type": "boolean"}},
		},
		OutputSchema: strObj("summary"),
		Outputs:      map[string]TemplateValue{"summary": json.RawMessage(`"{{ step.draft.summary }}"`)},
		Graph: NodeList{
			&If{
				Cond: Expr("{{ input.do_it }}"),
				Then: NodeList{
					&CodeStep{ID: "draft", Container: "c", Run: "true", OutputSchema: strObj("summary")},
				},
			},
		},
	}
	ld := makeLD(wf)
	diags := Validate(ld)
	if HasErrors(diags) {
		t.Fatalf("workflow should validate clean (if is transparent); got errors: %+v", diags)
	}
	found := false
	for _, d := range diags {
		if d.Severity == Warning && d.Code == "AWF3012" && d.Path == "outputs.summary" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected AWF3012 WARNING at outputs.summary; got %+v", diags)
	}
}
