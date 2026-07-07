package ir_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
)

// validateForTest writes src to a temp workflow file, loads it through the real
// loader (so LoadedModule.RawDoc is populated end-to-end), and runs the full
// validator. Using the loader — not a hand-built LoadedDefinition — is what lets
// these tests double as an end-to-end check that RawDoc reaches the pass.
func validateForTest(t *testing.T, src string) []ir.Diagnostic {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "workflow.yaml")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ld, err := loader.Load(p)
	if err != nil {
		t.Fatalf("loader.Load(%q): %v", p, err)
	}
	return ir.Validate(ld)
}

func hasCode(diags []ir.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// TestUnknownKeys_TopLevel: a top-level key that is not part of the Workflow
// schema (here the GHA muscle-memory `working-directory:`) is rejected. NOTE: the
// task brief's example used `env: {}`, but `env` IS a valid Workflow field (list
// form) and `{}` also fails to decode into []string — so this uses a genuinely
// unknown key instead.
func TestUnknownKeys_TopLevel(t *testing.T) {
	diags := validateForTest(t, "workflow: x\nversion: 1\ngraph: []\nworking-directory: /app\n")
	if !hasCode(diags, "AWF1062") {
		t.Fatalf("expected AWF1062 for top-level working-directory:, got %v", diags)
	}
}

func TestUnknownKeys_XPrefixTolerated(t *testing.T) {
	diags := validateForTest(t, "workflow: x\nversion: 1\ngraph: []\nx-anchors: {}\n")
	if hasCode(diags, "AWF1062") {
		t.Fatalf("x-* must be tolerated, got %v", diags)
	}
}

// TestUnknownKeys_KnownTopLevelClean confirms a real Workflow field (env, list
// form) is NOT flagged.
func TestUnknownKeys_KnownTopLevelClean(t *testing.T) {
	diags := validateForTest(t, "workflow: x\nversion: 1\ngraph: []\nenv: [FOO]\n")
	if hasCode(diags, "AWF1062") {
		t.Fatalf("env: is a valid Workflow field and must not be flagged, got %v", diags)
	}
}

func TestUnknownKeys_StepLevel(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - run: echo hi\n    ouput_files: []\n"
	diags := validateForTest(t, src)
	if !hasCode(diags, "AWF1062") {
		t.Fatalf("expected AWF1062 for typo'd ouput_files:, got %v", diags)
	}
}

func TestUnknownKeys_WithSubtreeSkipped(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n  - uses: anthropic/claude-code\n    with:\n      anything_opaque: 1\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1062") {
		t.Fatalf("with: subtree must be skipped, got %v", diags)
	}
}

// TestUnknownKeys_ControlChildrenRecursed confirms the walker descends into a
// control node's child blocks (here gate.generate) and flags a typo'd step key
// nested inside.
func TestUnknownKeys_ControlChildrenRecursed(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n" +
		"  - gate:\n" +
		"      generate:\n" +
		"        - id: draft\n" +
		"          uses: awf/llm\n" +
		"          ouput_schema: {}\n" +
		"      evaluate:\n" +
		"        - id: judge\n" +
		"          uses: awf/llm\n" +
		"          output_schema: {type: object}\n" +
		"      until: \"{{ evaluate.ok }}\"\n"
	diags := validateForTest(t, src)
	if !hasCode(diags, "AWF1062") {
		t.Fatalf("expected AWF1062 for typo'd ouput_schema inside gate.generate, got %v", diags)
	}
}

// TestUnknownKeys_OutputSchemaValueNotWalked confirms JSON-Schema subtrees are
// skipped: an arbitrary JSON-Schema property name must not be mistaken for a step
// key.
func TestUnknownKeys_OutputSchemaValueNotWalked(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n" +
		"  - id: draft\n" +
		"    uses: awf/llm\n" +
		"    with: {prompt: hi}\n" +
		"    output_schema:\n" +
		"      type: object\n" +
		"      properties:\n" +
		"        working-directory: {type: string}\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1062") {
		t.Fatalf("output_schema value must not be walked, got %v", diags)
	}
}

// reduceMapWorkflowSrc builds a one-map workflow whose reduce: clause is exactly
// reduceKV (e.g. `field: vulnerable` or `over: vulnerable`), for the F16 hard-
// rename tests below.
func reduceMapWorkflowSrc(reduceKV string) string {
	return "workflow: x\nversion: 1\ngraph:\n" +
		"  - map:\n" +
		"      over: \"{{ input.items }}\"\n" +
		"      as: item\n" +
		"      container: c\n" +
		"      concurrency: 1\n" +
		"      body:\n" +
		"        - id: vote\n" +
		"          container: c\n" +
		"          run: \"true\"\n" +
		"          output_schema: {type: object, properties: {vulnerable: {type: boolean}}}\n" +
		"      reduce:\n" +
		"        quorum: 2\n" +
		"        " + reduceKV + "\n" +
		"containers:\n  c: {image: \"oci://x@sha256:abc\"}\n"
}

// TestUnknownKeys_ReduceFieldAccepted confirms reduce's renamed `field:` key
// (F16) decodes clean — no AWF1062, no AWF1066.
func TestUnknownKeys_ReduceFieldAccepted(t *testing.T) {
	diags := validateForTest(t, reduceMapWorkflowSrc("field: vulnerable"))
	if hasCode(diags, "AWF1062") {
		t.Fatalf("reduce field: must not be flagged unknown, got %v", diags)
	}
	if hasCode(diags, "AWF1066") {
		t.Fatalf("reduce field: must not be flagged renamed, got %v", diags)
	}
}

// TestUnknownKeys_ReduceOverRenamed confirms the OLD reduce `over:` spelling
// (retired F16) is caught as a specific hard rename (AWF1066), not the generic
// unknown-key AWF1062.
func TestUnknownKeys_ReduceOverRenamed(t *testing.T) {
	diags := validateForTest(t, reduceMapWorkflowSrc("over: vulnerable"))
	if !hasCode(diags, "AWF1066") {
		t.Fatalf("expected AWF1066 for reduce over:, got %v", diags)
	}
	if hasCode(diags, "AWF1062") {
		t.Fatalf("reduce over: must get the specific AWF1066, not also the generic AWF1062, got %v", diags)
	}
	var msg string
	for _, d := range diags {
		if d.Code == "AWF1066" {
			msg = d.Message
		}
	}
	if msg != "reduce over: renamed to field:" {
		t.Errorf("AWF1066 message = %q, want %q", msg, "reduce over: renamed to field:")
	}
}

// TestUnknownKeys_MapOverStillValid is the position-awareness proof: a Map's
// OWN `over:` (the fan-out expression, unrelated to Reduce's renamed field) must
// NOT be flagged by either AWF1062 or AWF1066 — the renamed-key registry is
// scoped per Go struct type (Reduce), not by bare key string, so Map.over is
// untouched.
func TestUnknownKeys_MapOverStillValid(t *testing.T) {
	src := "workflow: x\nversion: 1\ngraph:\n" +
		"  - map:\n" +
		"      over: \"{{ input.items }}\"\n" +
		"      as: item\n" +
		"      container: c\n" +
		"      concurrency: 1\n" +
		"      body:\n" +
		"        - id: vote\n" +
		"          container: c\n" +
		"          run: \"true\"\n" +
		"containers:\n  c: {image: \"oci://x@sha256:abc\"}\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1062") {
		t.Fatalf("map over: must not be flagged unknown, got %v", diags)
	}
	if hasCode(diags, "AWF1066") {
		t.Fatalf("map over: must not be flagged renamed (position-aware: only Reduce.over is a hard rename), got %v", diags)
	}
}

// TestUnknownKeys_TopLevelInputSchemaAccepted confirms the renamed top-level
// input_schema: (F17) decodes clean — no AWF1062, no AWF1066.
func TestUnknownKeys_TopLevelInputSchemaAccepted(t *testing.T) {
	src := "workflow: x\nversion: 1\n" +
		"input_schema: {type: object, properties: {foo: {type: string}}}\n" +
		"containers:\n  c: {image: \"oci://x@sha256:abc\"}\n" +
		"graph:\n  - id: a\n    container: c\n    run: \"echo {{ input.foo }}\"\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1062") {
		t.Fatalf("input_schema: must not be flagged unknown, got %v", diags)
	}
	if hasCode(diags, "AWF1066") {
		t.Fatalf("input_schema: must not be flagged renamed, got %v", diags)
	}
}

// TestUnknownKeys_TopLevelInputRenamed confirms the OLD top-level `input:`
// spelling (retired F17) is caught as a specific hard rename (AWF1066), not
// the generic unknown-key AWF1062 — the position-aware counterpart of
// TestUnknownKeys_ReduceOverRenamed, but for the Workflow shape.
func TestUnknownKeys_TopLevelInputRenamed(t *testing.T) {
	src := "workflow: x\nversion: 1\n" +
		"input: {type: object, properties: {foo: {type: string}}}\n" +
		"graph: []\n"
	diags := validateForTest(t, src)
	if !hasCode(diags, "AWF1066") {
		t.Fatalf("expected AWF1066 for top-level input:, got %v", diags)
	}
	if hasCode(diags, "AWF1062") {
		t.Fatalf("top-level input: must get the specific AWF1066, not also the generic AWF1062, got %v", diags)
	}
	var msg string
	for _, d := range diags {
		if d.Code == "AWF1066" {
			msg = d.Message
		}
	}
	if msg != "top-level input: renamed to input_schema:" {
		t.Errorf("AWF1066 message = %q, want %q", msg, "top-level input: renamed to input_schema:")
	}
}

// TestUnknownKeys_InputTemplateRefsStillResolve confirms the runtime
// {{ input.* }} namespace is UNRELATED to the input_schema: rename: a step
// referencing input.foo still resolves against the (renamed) schema producer,
// with no AWF1062/AWF1066/AWF3001.
func TestUnknownKeys_InputTemplateRefsStillResolve(t *testing.T) {
	src := "workflow: x\nversion: 1\n" +
		"input_schema: {type: object, required: [foo], properties: {foo: {type: string}}, additionalProperties: false}\n" +
		"containers:\n  c: {image: \"oci://x@sha256:abc\"}\n" +
		"graph:\n  - id: a\n    container: c\n    run: \"echo {{ input.foo }}\"\n"
	diags := validateForTest(t, src)
	for _, d := range diags {
		if d.Code == "AWF1062" || d.Code == "AWF1066" || d.Code == "AWF3001" {
			t.Errorf("did not expect %s: %+v", d.Code, d)
		}
	}
}

// TestUnknownKeys_CallStepInputUnaffected confirms a call step's OWN `input:`
// (the instance-binding wire key on CallStep, unrelated to the Workflow-level
// input_schema: rename) still validates clean — F17 only retires the
// top-level Workflow shape's `input:`, not CallStep.Input. Mirrors
// TestUnknownKeys_MapOverStillValid's position-awareness proof.
func TestUnknownKeys_CallStepInputUnaffected(t *testing.T) {
	src := "workflow: root\nversion: 1\ncontainers: {}\n" +
		"graph:\n  - id: c1\n    call: child\n    input:\n      x: \"hello\"\n"
	diags := validateForTest(t, src)
	if hasCode(diags, "AWF1062") {
		t.Fatalf("call-step input: must not be flagged unknown, got %v", diags)
	}
	if hasCode(diags, "AWF1066") {
		t.Fatalf("call-step input: must not be flagged renamed (position-aware: only Workflow.input is a hard rename), got %v", diags)
	}
}

// TestUnknownKeys_CorpusZeroFalsePositives loads every examples/**/*.yaml and
// asserts ZERO AWF1062 diagnostics. This is the objective safety net: a false
// positive means the walker is missing a real allowed key or a skip rule — fix the
// PASS, never the corpus.
func TestUnknownKeys_CorpusZeroFalsePositives(t *testing.T) {
	root := "../examples"
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no example workflows found under %s", root)
	}
	for _, f := range files {
		t.Run(filepath.Base(filepath.Dir(f))+"/"+filepath.Base(f), func(t *testing.T) {
			ld, err := loader.Load(f)
			if err != nil {
				t.Fatalf("loader.Load(%q): %v", f, err)
			}
			for _, d := range ir.Validate(ld) {
				if d.Code == "AWF1062" {
					t.Errorf("false positive AWF1062 in %s at %q: %s", f, d.Path, d.Message)
				}
			}
		})
	}
}
