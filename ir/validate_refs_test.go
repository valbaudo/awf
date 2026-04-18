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
