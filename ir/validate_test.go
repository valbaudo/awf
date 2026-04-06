package ir

import (
	"strings"
	"testing"
)

// TestValidateSkeletonReturnsEmpty pins that an empty/minimal LoadedDefinition produces no
// diagnostics — the entry point compiles and the no-op skeleton is wired correctly. Real
// rule coverage lands in the per-pass tests added by Tasks 2–5.
func TestValidateSkeletonReturnsEmpty(t *testing.T) {
	ld := &LoadedDefinition{
		Workflow: &Workflow{
			ID:         "empty",
			Version:    1,
			Containers: map[string]Container{},
			Graph:      NodeList{},
		},
		WorkflowPath: "/tmp/empty.yaml",
		ComposeFiles: map[string][]byte{},
	}
	diags := Validate(ld)
	if len(diags) != 0 {
		t.Errorf("Validate(empty) = %d diagnostics, want 0: %+v", len(diags), diags)
	}
}

// TestValidateNilSafe pins the contract: Validate(nil) does not panic.
func TestValidateNilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Validate(nil) panicked: %v", r)
		}
	}()
	diags := Validate(nil)
	// Skeleton returns a single diagnostic for nil so callers can surface it gracefully.
	if !HasErrors(diags) {
		t.Errorf("Validate(nil) should report an error; got %+v", diags)
	}
}

// helper: assert that diags contains exactly one Error of the given code, anywhere in the slice.
func assertOneError(t *testing.T, diags []Diagnostic, code string) {
	t.Helper()
	count := 0
	for _, d := range diags {
		if d.Code == code && d.Severity == Error {
			count++
		}
	}
	if count != 1 {
		t.Errorf("want exactly 1 Error with code %q; got %d in %+v", code, count, diags)
	}
}

// assertErrorAt asserts diags contains an Error with the given code at the EXACT given path.
// Exact-match (not substring) so short path components like "a" don't collide with substrings
// of unrelated paths (e.g. "containers.lab.image" contains "a" inside "lab").
func assertErrorAt(t *testing.T, diags []Diagnostic, code, exactPath string) {
	t.Helper()
	for _, d := range diags {
		if d.Code == code && d.Severity == Error && d.Path == exactPath {
			return
		}
	}
	t.Errorf("want Error %q at path %q in %+v", code, exactPath, diags)
}

func makeLD(wf *Workflow) *LoadedDefinition {
	return &LoadedDefinition{
		Workflow:     wf,
		WorkflowPath: "/tmp/test.yaml",
		ComposeFiles: map[string][]byte{},
	}
}

func TestStructuralStepIDUnique(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "dup", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "c", Run: "true"},
			&CodeStep{ID: "a", Container: "c", Run: "true"}, // duplicate
		},
	})
	assertOneError(t, Validate(ld), "AWF1004")
}

func TestStructuralContainerBothImageAndCompose(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "both", Version: 1,
		Containers: map[string]Container{
			"c": {Image: "oci://x@sha256:abc", Compose: "compose.yml"},
		},
		Graph: NodeList{},
	})
	assertErrorAt(t, Validate(ld), "AWF1005", "containers.c")
}

func TestStructuralContainerNeither(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "neither", Version: 1,
		Containers: map[string]Container{"c": {}},
		Graph:      NodeList{},
	})
	assertErrorAt(t, Validate(ld), "AWF1006", "containers.c")
}

func TestStructuralImageMustBeDigest(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "tag", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://nginx:latest"}},
		Graph:      NodeList{},
	})
	assertErrorAt(t, Validate(ld), "AWF1007", "containers.c.image")
}

func TestStructuralComposeMissingService(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "no-svc", Version: 1,
		Containers: map[string]Container{"c": {Compose: "./compose.yml"}},
		Graph:      NodeList{},
	})
	assertErrorAt(t, Validate(ld), "AWF1008", "containers.c")
}

func TestStructuralContainerRefMustResolve(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "bad-ref", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "undeclared", Run: "true"},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1009", "a")
}

func TestStructuralCodeStepEmptyContainerRejected(t *testing.T) {
	// I1: empty container ref on CodeStep / AgentStep is AWF1009 (missing). Only
	// SignalStep is container-less per AWF §4.3.
	ld := makeLD(&Workflow{
		ID: "empty-ctr", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "", Run: "true"},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1009", "a")
}

func TestStructuralSignalStepAllowsNoContainer(t *testing.T) {
	// SignalStep doesn't need a container (per AWF §4.3). No AWF1009 should fire.
	ld := makeLD(&Workflow{
		ID: "signal", Version: 1,
		Containers: map[string]Container{},
		Graph: NodeList{
			&SignalStep{ID: "approve", Await: "human_review"},
		},
	})
	for _, d := range Validate(ld) {
		if d.Code == "AWF1009" {
			t.Errorf("SignalStep must not require a container: %v", d)
		}
	}
}

func TestStructuralParallelDistinctContainers(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "par", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Parallel{Children: NodeList{
				&CodeStep{ID: "a", Container: "c", Run: "true"},
				&CodeStep{ID: "b", Container: "c", Run: "true"}, // SAME container — §5.4 violation
			}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1010", "parallel[0]")
}

func TestStructuralLoopRequiresUntilOrMaxIters(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "loop", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Loop{Body: NodeList{&CodeStep{ID: "a", Container: "c", Run: "true"}}}, // no until, no max_iters
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1011", "loop[0]")
}

func TestStructuralMapRequiresAllFields(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "map", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Map{Body: NodeList{&CodeStep{ID: "a", Container: "c", Run: "true"}}}, // missing over/as/container/concurrency
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1012", "map[0]")
}

func TestStructuralMapContainerMustResolve(t *testing.T) {
	// I5: Map.Container is treated like any step's container ref — must resolve.
	ld := makeLD(&Workflow{
		ID: "map-bad-ctr", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Map{
				Over:        Expr("{{ input.items }}"),
				As:          "item",
				Container:   "nope", // undeclared
				Concurrency: 2,
				Body:        NodeList{&CodeStep{ID: "a", Container: "c", Run: "true"}},
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1009", "map[0]")
}

func TestStructuralMapContainerRejectsTemplateSyntax(t *testing.T) {
	// I5 / AWF1019: Map.Container must be a STATIC name, not a {{ }} template.
	ld := makeLD(&Workflow{
		ID: "map-tmpl", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Map{
				Over:        Expr("{{ input.items }}"),
				As:          "item",
				Container:   "{{ injected }}", // template syntax forbidden here
				Concurrency: 2,
				Body:        NodeList{&CodeStep{ID: "a", Container: "c", Run: "true"}},
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1019", "map[0]")
}

func TestStructuralContainerServiceRejectsTemplateSyntax(t *testing.T) {
	// I5: Container.Service must also be a static name.
	ld := makeLD(&Workflow{
		ID: "svc-tmpl", Version: 1,
		Containers: map[string]Container{
			"lab": {Compose: "lab/compose.yml", Service: "{{ injected }}"},
		},
		Graph: NodeList{},
	})
	assertErrorAt(t, Validate(ld), "AWF1019", "containers.lab.service")
}

func TestStructuralContainerImageRejectsTemplateSyntax(t *testing.T) {
	// AWF1019: Container.Image must be a STATIC image reference, not a {{ }} template.
	// A crafted image like "{{ injected }}@sha256:abc" passes AWF1007 (contains @sha256:)
	// but must be rejected as a template-syntax misuse.
	ld := makeLD(&Workflow{
		ID: "img-tmpl", Version: 1,
		Containers: map[string]Container{
			"c": {Image: "{{ injected }}@sha256:abc"},
		},
		Graph: NodeList{},
	})
	assertErrorAt(t, Validate(ld), "AWF1019", "containers.c.image")
}

func TestStructuralVersionMustBe1(t *testing.T) {
	// I4 / AWF1017: AWF §2 defines version 1 as the only supported value.
	ld := makeLD(&Workflow{
		ID: "v2", Version: 2,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph:      NodeList{},
	})
	assertErrorAt(t, Validate(ld), "AWF1017", "")
}

func TestStructuralGateGenerateNonEmpty(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "gate", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Gate{
				Generate:    NodeList{}, // EMPTY — §5.5 violation
				Evaluate:    NodeList{&CodeStep{ID: "j", Container: "c", Run: "true", OutputSchema: &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}}},
				Until:       Expr("{{ step.j.ok }}"),
				MaxAttempts: 5,
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1013", "gate[0]")
}

func TestStructuralGateEvaluateFinalHasOutputSchema(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "gate", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Gate{
				Generate:    NodeList{&CodeStep{ID: "g", Container: "c", Run: "true"}},
				Evaluate:    NodeList{&CodeStep{ID: "j", Container: "c", Run: "true"}}, // no output_schema
				Until:       Expr("{{ step.j.ok }}"),
				MaxAttempts: 5,
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1014", "gate[0]")
}

func TestStructuralGateRequiresUntil(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "gate", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Gate{
				Generate:    NodeList{&CodeStep{ID: "g", Container: "c", Run: "true"}},
				Evaluate:    NodeList{&CodeStep{ID: "j", Container: "c", Run: "true", OutputSchema: &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}}},
				Until:       "", // EMPTY
				MaxAttempts: 5,
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1015", "gate[0]")
}

func TestStructuralExpressionSizeLimit(t *testing.T) {
	// AWF1016 hardens against adversarial workflows that try to OOM the parser via a 1MB
	// condition. Default cap: 64 KiB.
	big := strings.Repeat("a", 65*1024)
	ld := makeLD(&Workflow{
		ID: "big-expr", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&If{Cond: Expr("{{ " + big + " }}")},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1016", "if[0]")
}

// Happy-path smoke: a well-formed minimal workflow produces NO AWF1xxx structural errors.
func TestStructuralHappyMinimal(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "happy", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "c", Run: "true"},
			&If{Cond: Expr("{{ step.a.exit_code == 0 }}"), Then: NodeList{
				&CodeStep{ID: "b", Container: "c", Run: "true"},
			}},
			&Loop{Body: NodeList{&CodeStep{ID: "c", Container: "c", Run: "true"}}, MaxIters: intPtr(3)},
			&Parallel{Children: NodeList{
				&CodeStep{ID: "p1", Container: "c", Run: "true"},
				// Only one branch here, so distinct-container rule is satisfied by construction.
			}},
			&Gate{
				Generate:    NodeList{&CodeStep{ID: "g", Container: "c", Run: "true"}},
				Evaluate:    NodeList{&CodeStep{ID: "j", Container: "c", Run: "true", OutputSchema: &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}}},
				Until:       Expr("{{ step.j.ok }}"),
				MaxAttempts: 3,
			},
		},
	})
	for _, d := range Validate(ld) {
		if d.Severity == Error && strings.HasPrefix(d.Code, "AWF1") {
			t.Errorf("unexpected AWF1xxx error: %v", d)
		}
	}
}

func intPtr(n int) *int { return &n }

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

func TestSchemaWellFormednessAWF2001(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "bad", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{
				ID: "a", Container: "c", Run: "true",
				OutputSchema: &JSONSchema{"type": "not-a-type"}, // invalid metaschema
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF2001", "a.output_schema")
}

func TestSchemaFloorOneOfWarnsAWF2002(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "floor", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{},
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"oneOf":                []any{},
					"required":             []any{},
					"properties":           map[string]any{},
					"additionalProperties": false,
				},
			},
		},
	})
	found := false
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" && d.Severity == Warning && strings.Contains(d.Path, "a") {
			found = true
		}
	}
	if !found {
		t.Errorf("want AWF2002 warning for oneOf in agent output_schema; got %+v", Validate(ld))
	}
}

func TestSchemaFloorOnlyApppliesToAgents(t *testing.T) {
	// Code steps' output_schemas are NOT subject to the §7 floor — that's an
	// agent-specific cross-backend portability rule. A code step with oneOf is fine.
	ld := makeLD(&Workflow{
		ID: "code-floor", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{
				ID: "a", Container: "c", Run: "true",
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"required":             []any{"x"},
					"properties":           map[string]any{"x": map[string]any{"type": "integer", "minimum": 0}},
					"additionalProperties": false,
				},
			},
		},
	})
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" {
			t.Errorf("AWF2002 must not apply to code-step output_schema: %v", d)
		}
	}
}

func TestSchemaFloorMissingRequired(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "missing-req", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{},
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"required":             []any{}, // x not required
					"properties":           map[string]any{"x": map[string]any{"type": "integer"}},
					"additionalProperties": false,
				},
			},
		},
	})
	found := false
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" && strings.Contains(d.Message, "x") {
			found = true
		}
	}
	if !found {
		t.Errorf("want AWF2002 for missing-required property; got %+v", Validate(ld))
	}
}

func TestSchemaFloorRecursiveOnNestedObject(t *testing.T) {
	// Anthropic and OpenAI both enforce structural floor rules at every object level.
	// A nested object with additionalProperties: true or missing-required is a real
	// portability problem the validator must surface.
	ld := makeLD(&Workflow{
		ID: "nested-floor", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{},
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"outer"},
					"properties": map[string]any{
						"outer": map[string]any{
							"type":                 "object",
							"additionalProperties": true, // nested violation
							"required":             []any{},
							"properties": map[string]any{
								"x": map[string]any{"type": "string"},
								// "x" missing from required — second nested violation
							},
						},
					},
				},
			},
		},
	})
	warnings := 0
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" && d.Severity == Warning && strings.Contains(d.Path, "a.output_schema") {
			warnings++
		}
	}
	if warnings < 2 {
		t.Errorf("want at least 2 AWF2002 warnings (additionalProperties + required) on nested object; got %d: %+v", warnings, Validate(ld))
	}
}

func TestSchemaInputSchemaAlsoValidated(t *testing.T) {
	bad := JSONSchema{"type": "not-a-type"}
	ld := makeLD(&Workflow{
		ID: "input", Version: 1, Input: &bad,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph:      NodeList{},
	})
	assertErrorAt(t, Validate(ld), "AWF2001", "input")
}

func TestSchemaFloorAdditionalPropertiesSchemaForm(t *testing.T) {
	// AWF2002: additionalProperties must be the literal `false`. A SCHEMA OBJECT form
	// (additionalProperties: {type:string}) is valid JSON Schema 2020-12 but forbidden
	// by the §7 floor.
	ld := makeLD(&Workflow{
		ID: "ap-schema", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{},
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"}, // schema-form
					"required":             []any{},
					"properties":           map[string]any{},
				},
			},
		},
	})
	found := false
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" && d.Severity == Warning && strings.Contains(d.Message, "literal `false`") {
			found = true
		}
	}
	if !found {
		t.Errorf("want AWF2002 for additionalProperties schema-form; got %+v", Validate(ld))
	}
}

func TestSchemaFloorTypeAsArrayRejected(t *testing.T) {
	// AWF2002: type as array (e.g. ["string","null"]) is forbidden by the §7 floor.
	// AgentStep with array-typed property surfaces the warning. (The outer schema is
	// type:object so the top-level rule passes; the nested array-type fires.)
	ld := makeLD(&Workflow{
		ID: "type-arr", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "a", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{},
				OutputSchema: &JSONSchema{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"x"},
					"properties": map[string]any{
						"x": map[string]any{"type": []any{"string", "null"}}, // array-form type
					},
				},
			},
		},
	})
	found := false
	for _, d := range Validate(ld) {
		if d.Code == "AWF2002" && d.Severity == Warning && strings.Contains(d.Message, "type as an array") {
			found = true
		}
	}
	if !found {
		t.Errorf("want AWF2002 for array-form type; got %+v", Validate(ld))
	}
}
