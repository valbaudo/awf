package ir

import (
	"strconv"
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

func TestStructuralAssetIDUsesStepIDRules(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "asset-id", Version: 1,
		Assets:     map[string]string{"": "empty.txt", "bad/id": "schema.json", "generate": "prompt.txt"},
		Containers: map[string]Container{},
		Graph:      NodeList{},
	})
	diags := Validate(ld)
	assertErrorAt(t, diags, "AWF1020", "assets.")
	assertErrorAt(t, diags, "AWF1020", "assets.bad/id")
	assertErrorAt(t, diags, "AWF1020", "assets.generate")
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

func TestStructuralMapLiteralOverSatisfiesAWF1012(t *testing.T) {
	// F51: the literal-sequence arm (OverItems) satisfies the `over:` requirement just
	// like the {{ }} expression arm (Over) did before — no AWF1012 "missing over" error.
	ld := makeLD(&Workflow{
		ID: "map-literal-over", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Map{
				OverItems:   []any{"a", "b", "c"},
				As:          "item",
				Container:   "c",
				Concurrency: 2,
				Body:        NodeList{&CodeStep{ID: "a", Container: "c", Run: "true"}},
			},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF1012")
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

func TestStructuralEnvNamesValidated(t *testing.T) {
	// AWF1024: top-level env: lists host env-var NAMES forwarded into agent steps.
	// A malformed name (hyphen) and an empty name are both rejected, each at its index;
	// a well-formed name produces no diagnostic.
	ld := makeLD(&Workflow{
		ID: "env", Version: 1,
		Env:        []string{"OK_NAME", "BAD-NAME", ""},
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph:      NodeList{},
	})
	diags := Validate(ld)
	assertErrorAt(t, diags, "AWF1024", "env[1]") // BAD-NAME (hyphen)
	assertErrorAt(t, diags, "AWF1024", "env[2]") // empty
}

func TestStructuralEnvValidNamesClean(t *testing.T) {
	// A well-formed env: allowlist trips no AWF1024.
	ld := makeLD(&Workflow{
		ID: "env", Version: 1,
		Env:        []string{"OPENAI_API_KEY", "_underscore", "LITELLM_API_KEY"},
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph:      NodeList{},
	})
	assertNoCode(t, Validate(ld), "AWF1024")
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

// TestStructuralSignalStepWhereExpr exercises AWF1036: a signal step's where:
// clause must be a valid bounded boolean expression once its {{ }} slots are
// scanned and stripped. `{{ }}` slots render from the engine scope at runtime
// and bare identifiers resolve against the delivered payload, so the validator
// replaces each slot with a parse-safe placeholder and ParseExprs the remainder.
func TestStructuralSignalStepWhereExpr(t *testing.T) {
	cases := []struct {
		name    string
		where   string
		wantErr bool // true => AWF1036 must fire
	}{
		// 1. quoted-slot string comparison — valid (the author's "" survive the
		//    slot strip, so the placeholder lands inside them: candidate_id == "<ph>").
		{"quoted-slot-string", `candidate_id == "{{ hyp.id }}"`, false},
		// 2. truncated comparison — invalid.
		{"truncated", "candidate_id ==", true},
		// 3. unbalanced slot — the slot scan fails.
		{"unbalanced-slot", "{{ unbalanced ", true},
		// 4. arithmetic — not in the bounded-boolean grammar; ParseExpr rejects `+`.
		{"arithmetic", "candidate_id + 1 == 2", true},
		// 5. empty where (omitted) — no-op, no AWF1036.
		{"empty", "", false},
		// 6. numeric correlation, no inner quotes — valid (bare placeholder is a
		//    valid primary in operand position: count == <ph>).
		{"numeric-bare-slot", "count == {{ n }}", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ld := makeLD(&Workflow{
				ID: "signal-where", Version: 1,
				Containers: map[string]Container{},
				Graph: NodeList{
					&SignalStep{ID: "wait", Await: "oob-hit", Where: tc.where},
				},
			})
			diags := Validate(ld)
			var got bool
			for _, d := range diags {
				if d.Code == "AWF1036" {
					got = true
				}
			}
			if got != tc.wantErr {
				t.Errorf("where=%q: got AWF1036=%v, want %v (diags=%v)", tc.where, got, tc.wantErr, diags)
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
func TestStructuralMapImageTargetMayOmitImage(t *testing.T) {
	// A container declared SOLELY to receive a map's runtime image: (resources-
	// only, no image/compose) is legal — and the templated map.image is not an
	// AWF1019 violation. (P6a.)
	ld := makeLD(&Workflow{
		ID: "p6a-ok", Version: 1,
		InputSchema: &JSONSchema{"type": "object", "properties": map[string]any{
			"items": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		}},
		Containers: map[string]Container{
			"version_lab": {Resources: &Resources{CPU: "1", Mem: "1Gi"}},
		},
		Graph: NodeList{
			&Map{
				Over: "{{ input.items }}", As: "v", Container: "version_lab",
				Image: "{{ v.image }}", Concurrency: 2,
				Body: NodeList{&CodeStep{ID: "probe", Container: "version_lab", Run: "true"}},
			},
		},
	})
	diags := Validate(ld)
	assertNoError(t, diags)
	// The templated map.image must NOT trip AWF1019 ("{{ }}" in a static-name
	// field) — that ban applies to map.container, not the runtime image. Make
	// the invariant explicit so a future AWF1019-on-map.image regression fails here.
	assertNoCode(t, diags, "AWF1019")
}

func TestStructuralResourcesOnlyContainerStillErrorsWhenNotMapImageTarget(t *testing.T) {
	// A resources-only container that is NOT a map.image target still trips
	// AWF1006 — the exemption is scoped to map.image targets only.
	ld := makeLD(&Workflow{
		ID: "p6a-bad", Version: 1,
		Containers: map[string]Container{"c": {Resources: &Resources{CPU: "1"}}},
		Graph:      NodeList{&CodeStep{ID: "a", Container: "c", Run: "true"}},
	})
	assertErrorAt(t, Validate(ld), "AWF1006", "containers.c")
}

func TestMapImageTargetsCollectsTargets(t *testing.T) {
	wf := &Workflow{
		Graph: NodeList{
			&Map{Container: "vl", Image: "{{ v.image }}"},
			&Map{Container: "static"}, // no Image → not a target
			&Loop{Body: NodeList{ // recursion: a map nested in a control kind is still collected
				&Map{Container: "nested", Image: "{{ v.image }}"},
			}},
		},
	}
	got := MapImageTargets(wf)
	if !got["vl"] || !got["nested"] || got["static"] {
		t.Errorf("MapImageTargets = %v, want {vl, nested} true and static absent", got)
	}
}

func TestStructuralMapImageTargetWithStaticImageConflicts(t *testing.T) {
	// A container that is a map.image target AND declares a static (pinned) image
	// trips AWF1025 — the static pin would be silently overwritten at dispatch.
	img := "oci://example.com/x@sha256:" + strings.Repeat("0", 64)
	ld := makeLD(&Workflow{
		ID: "p6a-conflict", Version: 1,
		InputSchema: &JSONSchema{"type": "object", "properties": map[string]any{
			"items": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		}},
		Containers: map[string]Container{"vl": {Image: img}},
		Graph: NodeList{
			&Map{
				Over: "{{ input.items }}", As: "v", Container: "vl",
				Image: "{{ v.image }}", Concurrency: 1,
				Body: NodeList{&CodeStep{ID: "probe", Container: "vl", Run: "true"}},
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1025", "containers.vl")
}

func TestStructuralResourcesOnlyMapImageTargetNoConflict(t *testing.T) {
	// A resources-only map.image target (the intended shape) does NOT trip AWF1025.
	ld := makeLD(&Workflow{
		ID: "p6a-ok2", Version: 1,
		InputSchema: &JSONSchema{"type": "object", "properties": map[string]any{
			"items": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		}},
		Containers: map[string]Container{"vl": {Resources: &Resources{CPU: "1"}}},
		Graph: NodeList{
			&Map{
				Over: "{{ input.items }}", As: "v", Container: "vl",
				Image: "{{ v.image }}", Concurrency: 1,
				Body: NodeList{&CodeStep{ID: "probe", Container: "vl", Run: "true"}},
			},
		},
	})
	assertNoCode(t, Validate(ld), "AWF1025")
}

func TestStructuralMapImageTargetContainerRejectedOutsideOwningMapBody(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "p6a-escape", Version: 1,
		InputSchema: &JSONSchema{"type": "object", "properties": map[string]any{
			"items": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		}},
		Containers: map[string]Container{"vl": {Resources: &Resources{CPU: "1"}}},
		Graph: NodeList{
			&Map{
				Over: "{{ input.items }}", As: "v", Container: "vl",
				Image: "{{ v.image }}", Concurrency: 1,
				Body: NodeList{&CodeStep{ID: "probe", Container: "vl", Run: "true"}},
			},
			&CodeStep{ID: "after", Container: "vl", Run: "true"},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1039", "after")
}

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

func TestAWF1016MessageReflectsLimitConst(t *testing.T) {
	big := strings.Repeat("a", 65*1024)
	ld := makeLD(&Workflow{
		ID: "big-expr", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph:      NodeList{&If{Cond: Expr("{{ " + big + " }}")}},
	})
	var msg string
	for _, d := range Validate(ld) {
		if d.Code == "AWF1016" {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatal("no AWF1016 diagnostic emitted")
	}
	if !strings.Contains(msg, strconv.Itoa(maxExpressionBytes)) {
		t.Errorf("AWF1016 message %q must state the real limit %d bytes (derived from the const)", msg, maxExpressionBytes)
	}
	if strings.Contains(msg, "KiB") {
		t.Errorf("AWF1016 message %q still hardcodes a KiB magnitude that can drift from the const", msg)
	}
}

func TestAWF1024MessageReflectsEnvPattern(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "env", Version: 1,
		Env:        []string{"BAD-NAME"},
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph:      NodeList{},
	})
	var msg string
	for _, d := range Validate(ld) {
		if d.Code == "AWF1024" {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatal("no AWF1024 diagnostic emitted")
	}
	if !strings.Contains(msg, envNamePattern.String()) {
		t.Errorf("AWF1024 message %q must contain the enforcing regex %q (derived, not a hand-typed copy)", msg, envNamePattern.String())
	}
}

func TestStructuralContainerNameCharset(t *testing.T) {
	// AWF1059: a container map key must be a path-safe identifier. The native
	// backend derives a host workdir from this raw per-workflow key, so a key
	// like "../escape" is a path-traversal sink — reject it at the format level.
	ld := makeLD(&Workflow{
		ID: "bad-container-name", Version: 1,
		Containers: map[string]Container{
			"../escape": {Image: "oci://x@sha256:abc"},
		},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "../escape", Run: "true"},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1059", "containers.../escape")
}

func TestStructuralContainerNameValidClean(t *testing.T) {
	// A path-safe container key trips no AWF1059.
	ld := makeLD(&Workflow{
		ID: "ok-container-name", Version: 1,
		Containers: map[string]Container{"runner": {Image: "oci://x@sha256:abc"}},
		Graph:      NodeList{&CodeStep{ID: "a", Container: "runner", Run: "true"}},
	})
	assertNoCode(t, Validate(ld), "AWF1059")
}

func TestAWF1059MessageReflectsContainerNamePattern(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "bad-container-name", Version: 1,
		Containers: map[string]Container{
			"../escape": {Image: "oci://x@sha256:abc"},
		},
		Graph: NodeList{&CodeStep{ID: "a", Container: "../escape", Run: "true"}},
	})
	var msg string
	for _, d := range Validate(ld) {
		if d.Code == "AWF1059" {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatal("no AWF1059 diagnostic emitted")
	}
	if !strings.Contains(msg, containerNamePattern.String()) {
		t.Errorf("AWF1059 message %q must contain the enforcing regex %q (derived, not a hand-typed copy)", msg, containerNamePattern.String())
	}
}

// TestAWF1060CmdOnComposeContainer checks that a compose-mode container that also
// declares cmd: or keepalive: is rejected with AWF1060 (those fields are image-mode only;
// per-service command/lifecycle config lives in the compose file).
func TestAWF1060CmdOnComposeContainer(t *testing.T) {
	boolTrue := true
	cases := []struct {
		name string
		ctr  Container
	}{
		{
			"cmd on compose",
			Container{Compose: "./compose.yml", Service: "web", Cmd: []string{"sleep", "infinity"}},
		},
		{
			"keepalive on compose",
			Container{Compose: "./compose.yml", Service: "web", Keepalive: &boolTrue},
		},
		{
			"cmd and keepalive on compose",
			Container{Compose: "./compose.yml", Service: "web", Cmd: []string{"x"}, Keepalive: &boolTrue},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ld := makeLD(&Workflow{
				ID: "awf1060-test", Version: 1,
				Containers: map[string]Container{"c": c.ctr},
				Graph:      NodeList{},
			})
			assertErrorAt(t, Validate(ld), "AWF1060", "containers.c")
		})
	}
}

// TestAWF1060CmdOnImageContainerOK checks that cmd:/keepalive: on an image-mode
// container does NOT trigger AWF1060.
func TestAWF1060CmdOnImageContainerOK(t *testing.T) {
	boolFalse := false
	img := "oci://example.com/x@sha256:" + strings.Repeat("0", 64)
	ld := makeLD(&Workflow{
		ID: "awf1060-ok", Version: 1,
		Containers: map[string]Container{
			"c": {Image: img, Cmd: []string{"sleep", "infinity"}, Keepalive: &boolFalse},
		},
		Graph: NodeList{&CodeStep{ID: "a", Container: "c", Run: "true"}},
	})
	assertNoCode(t, Validate(ld), "AWF1060")
}
