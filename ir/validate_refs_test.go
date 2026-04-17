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
