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
