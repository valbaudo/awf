package ir

import (
	"strings"
	"testing"
)

// Tests for the AWF3001/AWF3002 reference pass — see validate_refs.go.

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

// TestValidateRefsMapAggregationDeferred exercises AWF5002: refs of the form
// `step.<map_id>.items[*]` / `step.<map_id>.summary.<field>` are deferred per spec §11
// item 4. The runtime ships per-item dispatch + commits but NOT aggregation; the
// validator catches the syntax at lint time so authors don't write Phase-N-only syntax
// expecting it to work in slice 3.4.
//
// HI-A precedence: AWF5002 fires ONLY when the id is not already a known step producer.
// A workflow with both `step.id: "map"` AND a literal Map kind sharing the leaf-name
// "map" resolves to the step (producer wins) → no AWF5002 / no AWF3001.
func TestValidateRefsMapAggregationDeferred(t *testing.T) {
	digest := "oci://example.com/r@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	// inputSchema declares `list` so the map's `over: {{ input.list }}` resolves cleanly
	// without dragging in unrelated AWF3001 noise that would muddy the negative-case
	// assertion (HI-A asserts no AWF5002 AND no AWF3001).
	inputSchema := &JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"list"},
		"properties":           map[string]any{"list": map[string]any{"type": "array"}},
	}
	cases := []struct {
		name     string
		graph    NodeList
		wantCode string
	}{
		{
			name: "step.<map>.items[N] rejected",
			graph: NodeList{
				&Map{
					Over: "{{ input.list }}", As: "x", Container: "c0", Concurrency: 1,
					Body: NodeList{
						&CodeStep{ID: "triage", Container: "c0", Run: "echo {{ x }}"},
					},
				},
				&CodeStep{ID: "post", Container: "c0", Run: "echo {{ step.map.items.0.id }}"},
			},
			wantCode: "AWF5002",
		},
		{
			name: "step.<map>.summary.succeeded rejected",
			graph: NodeList{
				&Map{
					Over: "{{ input.list }}", As: "x", Container: "c0", Concurrency: 1,
					Body: NodeList{
						&CodeStep{ID: "triage", Container: "c0", Run: "echo {{ x }}"},
					},
				},
				&CodeStep{ID: "post", Container: "c0", Run: "echo {{ step.map.summary.succeeded }}"},
			},
			wantCode: "AWF5002",
		},
		// Note: refs to a step INSIDE the map body (step.triage.<field>) are NOT aggregation
		// refs — they still resolve via the normal AWF3001 pathway. We don't test that here
		// because AWF3001's "field not in output_schema" would fire first (the body step has
		// no schema in this test). A separate positive test would need a schema'd inner step;
		// pinned by TestRunMapAsBindingThreaded at the engine level.
		{
			name: "HI-A: step ID shadowing map leaf does NOT trip AWF5002",
			graph: NodeList{
				// First step has id "map" AND a real output_schema with a `count` field.
				&CodeStep{
					ID: "map", Container: "c0", Run: "./step.sh",
					OutputSchema: &JSONSchema{
						"type":                 "object",
						"additionalProperties": false,
						"required":             []any{"count"},
						"properties":           map[string]any{"count": map[string]any{"type": "integer"}},
					},
				},
				// A literal Map kind whose leaf-name is also "map" (map[1]).
				&Map{
					Over: "{{ input.list }}", As: "x", Container: "c0", Concurrency: 1,
					Body: NodeList{
						&CodeStep{ID: "triage", Container: "c0", Run: "echo {{ x }}"},
					},
				},
				// step.map.count: id="map" IS a producer (the first step) AND matches map[1]'s
				// leaf-name. Per HI-A precedence, producer wins → AWF5002 does NOT fire →
				// AWF3001 resolves normally (count IS in the step's schema) → no diagnostic.
				&CodeStep{ID: "post", Container: "c0", Run: "echo {{ step.map.count }}"},
			},
			wantCode: "", // empty = assert NO AWF5002 / AWF3001 emitted
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ld := makeLD(&Workflow{
				ID: "agg", Version: 1,
				Containers: map[string]Container{"c0": {Image: digest}},
				Input:      inputSchema,
				Graph:      c.graph,
			})
			diags := Validate(ld)
			if c.wantCode == "" {
				// Negative case: assert NO AWF5002 (and NO AWF3001).
				// AWF3002 (warning: agent schema unreferenced) is acceptable.
				for _, d := range diags {
					if d.Code == "AWF5002" || d.Code == "AWF3001" {
						t.Errorf("expected no AWF5002/AWF3001; got %s at %s: %s", d.Code, d.Path, d.Message)
					}
				}
				return
			}
			var found bool
			for _, d := range diags {
				if d.Code == c.wantCode {
					found = true
				}
			}
			if !found {
				t.Errorf("missing %s; diagnostics = %v", c.wantCode, diags)
			}
		})
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
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Input:      &JSONSchema{"type": "object", "required": []any{"xs"}, "properties": map[string]any{"xs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "additionalProperties": false},
		Graph: NodeList{
			&Map{Over: Expr("{{ input.xs }}"), As: "x", Container: "c", Concurrency: 1, Body: NodeList{
				awf5003Step("inner"),
			}},
			// A top-level step referencing a step inside the map body: "which item?"
			// is undefined → AWF5003.
			&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.inner.exit_code }}"},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF5003", "after.run")
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

// TestCodeStepWritingAwfOutputNoAWF3006 asserts that a code step that writes
// $AWF_OUTPUT does NOT trigger AWF3006.
func TestCodeStepWritingAwfOutputNoAWF3006(t *testing.T) {
	ld := makeLD(&Workflow{ID: "ok", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{&CodeStep{ID: "recon", Container: "c", Run: `echo '{"x":1}' > "$AWF_OUTPUT"`,
			OutputSchema: &JSONSchema{"type": "object", "required": []any{"x"}, "properties": map[string]any{"x": map[string]any{"type": "integer"}}, "additionalProperties": false}}}})
	assertNoCode(t, Validate(ld), "AWF3006")
}
