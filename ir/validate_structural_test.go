package ir

import (
	"strings"
	"testing"
)

// Tests for the AWF1xxx structural pass — see validate_structural.go.

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
	// I1: empty container ref on a CodeStep is AWF1009 (missing). SignalStep is
	// container-less per AWF §4.3. AgentStep may also omit container when the
	// resolved adapter is Containerless (e.g. awf/llm) — see
	// TestStructuralAgentStepAllowsNoContainer; the run-start guard enforces it.
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

func TestStructuralParallelDistinctComposeProject(t *testing.T) {
	// AWF1010: `lab` and `lab:db` reference the same compose project (different services)
	// — they should still trigger the distinct-container rule.
	ld := makeLD(&Workflow{
		ID: "par-compose", Version: 1,
		Containers: map[string]Container{
			"lab": {Compose: "lab/compose.yml", Service: "web"},
		},
		Graph: NodeList{
			&Parallel{Children: NodeList{
				&CodeStep{ID: "a", Container: "lab", Run: "true"},
				&CodeStep{ID: "b", Container: "lab:db", Run: "true"}, // SAME compose project
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

// TestValidateStructuralStepIDReserved exercises AWF1020: step ids must match the runtime
// charset and must not collide with reserved control-keyword path segments (a step id of
// `generate` or one containing dots would shadow keywords in engine.Scope path matching).
func TestValidateStructuralStepIDReserved(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		wantCode string // "" if id is valid
	}{
		{"plain", "step_one", ""},
		{"hyphen-ok", "my-step", ""},
		{"leading-underscore-ok", "_internal", ""},
		{"digit-leading-bad", "1step", "AWF1020"},
		{"dot-bad", "foo.bar", "AWF1020"},
		{"bracket-bad", "gate[0]", "AWF1020"},
		{"reserved-generate", "generate", "AWF1020"},
		{"reserved-evaluate", "evaluate", "AWF1020"},
		{"reserved-until", "until", "AWF1020"},
		{"reserved-then", "then", "AWF1020"},
		{"reserved-body", "body", "AWF1020"},
		{"reserved-finally", "finally", "AWF1020"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ld := makeLD(&Workflow{
				ID: "x", Version: 1,
				Containers: map[string]Container{"c0": {Image: "oci://x@sha256:abc"}},
				Graph: NodeList{
					&CodeStep{ID: c.id, Container: "c0", Run: "true"},
				},
			})
			diags := Validate(ld)
			var found bool
			for _, d := range diags {
				if d.Code == "AWF1020" {
					found = true
				}
			}
			if c.wantCode == "AWF1020" && !found {
				t.Errorf("id=%q: want AWF1020 emitted; diags = %v", c.id, diags)
			}
			if c.wantCode == "" && found {
				t.Errorf("id=%q: AWF1020 emitted unexpectedly; diags = %v", c.id, diags)
			}
		})
	}
}

func TestStructuralSignalStepAwaitCharset(t *testing.T) {
	// M16: Await name must match stepIDPattern (no whitespace, no path
	// separators, no leading digits).
	cases := []struct {
		name    string
		await   string
		wantErr bool
	}{
		{"clean name", "human_review", false},
		{"dashes ok", "tick-tock", false},
		{"underscore ok", "_internal", false},
		{"space rejected", "human review", true},
		{"slash rejected", "../escape", true},
		{"newline rejected", "human\nreview", true},
		{"leading digit rejected", "0day", true},
		// empty not handled here; AWF §4.3 has separate "await required" check
		{"empty skipped", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ld := makeLD(&Workflow{
				ID: "signal-charset", Version: 1,
				Containers: map[string]Container{},
				Graph: NodeList{
					&SignalStep{ID: "approve", Await: tc.await},
				},
			})
			diags := Validate(ld)
			var got bool
			for _, d := range diags {
				if d.Code == "AWF1020" && strings.Contains(d.Message, "await=") {
					got = true
				}
			}
			if got != tc.wantErr {
				t.Errorf("await=%q: got AWF1020=%v, want %v (diags=%v)", tc.await, got, tc.wantErr, diags)
			}
		})
	}
}

func TestStructuralAgentStepAllowsNoContainer(t *testing.T) {
	// Part A: an AgentStep may omit `container:` (the runtime may be a
	// containerless adapter, e.g. awf/llm). No AWF1009 should fire for the
	// empty ref. (The run-start guard in cli/runtimes.go enforces that the
	// resolved adapter is actually Containerless — validation is registry-free.)
	ld := makeLD(&Workflow{
		ID: "llm-no-ctr", Version: 1,
		Containers: map[string]Container{},
		Graph: NodeList{
			&AgentStep{ID: "ask", Uses: "awf/llm", With: RawConfig{"model": "m", "prompt": "hi"}},
		},
	})
	for _, d := range Validate(ld) {
		if d.Code == "AWF1009" {
			t.Errorf("AgentStep with empty container must not emit AWF1009: %v", d)
		}
	}
}

func TestStructuralAgentStepPresentButUnresolvedContainerStillErrors(t *testing.T) {
	// A NON-EMPTY container that doesn't resolve is still AWF1009 — only the
	// empty case is newly permitted.
	ld := makeLD(&Workflow{
		ID: "llm-bad-ctr", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{ID: "ask", Uses: "awf/llm", Container: "undeclared", With: RawConfig{"model": "m", "prompt": "hi"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1009", "ask")
}

// TestValidateSnapshotField exercises the snapshot-field structural rules:
//   - AWF1021: snapshot value other than "workspace"/empty.
//   - AWF1022: snapshot:workspace on a compose-mode container (image-mode only).
//   - AWF1023: snapshot:workspace on a container a map fans out (temporary guard —
//     per-item snapshots land in a later slice).
func TestValidateSnapshotField(t *testing.T) {
	img := "oci://example.com/x@sha256:" + strings.Repeat("0", 64)
	cases := []struct {
		name     string
		snapshot string
		compose  bool
		inMap    bool
		wantCode string // "" = no snapshot-related error
	}{
		{"empty ok", "", false, false, ""},
		{"workspace ok image", "workspace", false, false, ""},
		{"bad value", "frozen", false, false, "AWF1021"},
		{"workspace on compose", "workspace", true, false, "AWF1022"},
		{"workspace in map", "workspace", false, true, "AWF1023"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctr := Container{Snapshot: c.snapshot}
			if c.compose {
				ctr.Compose = "./compose.yml"
				ctr.Service = "web"
			} else {
				ctr.Image = img
			}
			step := &CodeStep{ID: "s", Container: "c", Run: "true"}
			var graph NodeList
			if c.inMap {
				graph = NodeList{&Map{Over: Expr("{{ input.xs }}"), As: "x", Container: "c", Concurrency: 1, Body: NodeList{step}}}
			} else {
				graph = NodeList{step}
			}
			wf := &Workflow{ID: "w", Version: 1, Containers: map[string]Container{"c": ctr}, Graph: graph}
			diags := Validate(&LoadedDefinition{Workflow: wf})
			got := ""
			for _, d := range diags {
				if d.Code == "AWF1021" || d.Code == "AWF1022" || d.Code == "AWF1023" {
					got = d.Code
				}
			}
			if got != c.wantCode {
				t.Errorf("snapshot=%q compose=%v inMap=%v: got %q, want %q (diags: %v)", c.snapshot, c.compose, c.inMap, got, c.wantCode, diags)
			}
		})
	}
}
